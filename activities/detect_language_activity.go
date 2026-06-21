package activities

import (
	"context"
	"fmt"

	"dagger.io/dagger"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/syncopatedNote/tagger/agent_registry/agents"
	"github.com/syncopatedNote/tagger/types"
)

// languageMarker pairs a repository-root marker file with the language it
// implies. The slice is ordered by PRIORITY: the first marker present wins, so a
// repo carrying both go.mod and a tooling package.json resolves to Go. Keep the
// most specific / authoritative markers first.
var languageMarkers = []struct {
	File     string
	Language agents.Language
}{
	{"go.mod", agents.LangGo},
	{"pyproject.toml", agents.LangPython},
	{"requirements.txt", agents.LangPython},
	{"setup.py", agents.LangPython},
	{"package.json", agents.LangJS},
	{"pom.xml", agents.LangJava},
	{"build.gradle", agents.LangJava},
	{"build.gradle.kts", agents.LangJava},
}

// DetectLanguageActivity determines which coding-agent toolchain a repository
// needs by probing its root marker files. It is a Temporal activity because it
// does I/O (it reaches the Git remote) — the workflow may not. It is, however,
// functionally deterministic: same tree in, same language out. No LLM, no clock,
// no randomness; just a fixed-priority file-presence check.
//
// It inspects the tree at the PINNED BaseCommitSHA — the exact immutable tree
// the coding agent will later build — so detection can never race a moving
// branch. It returns only the short language string (claim-check).
//
// Note: this recognises JavaScript and Java even though their agents are not yet
// registered. Detecting them here lets the workflow surface a precise
// "unsupported language" error from RunCodingAgentActivity rather than a vague
// "could not detect" — the marker was found, the toolchain simply isn't built.
func (a *Activities) DetectLanguageActivity(ctx context.Context, in types.DetectLanguageInput) (types.DetectLanguageResult, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("Detecting repository language", "repo", in.RepoURL, "baseCommit", in.BaseCommitSHA)

	if in.RepoURL == "" || in.BaseCommitSHA == "" {
		return types.DetectLanguageResult{}, temporal.NewNonRetryableApplicationError(
			"RepoURL and BaseCommitSHA are required", "ValidationError", nil)
	}

	client, err := connectDagger(ctx)
	if err != nil {
		return types.DetectLanguageResult{}, err
	}
	defer client.Close()

	// One cheap call: list the repository ROOT entries at the pinned commit. We
	// only need top-level markers, so there is no full clone and no per-file
	// round-trip. (Per-subdirectory languages in a monorepo are out of scope for
	// now — a documented seam to extend later.)
	tree := client.Git(in.RepoURL, dagger.GitOpts{
		HTTPAuthUsername: "x-access-token",
		HTTPAuthToken:    client.SetSecret("github-token", a.githubToken),
	}).Commit(in.BaseCommitSHA).Tree()
	entries, err := tree.Entries(ctx)
	if err != nil {
		// Network/transport hiccup — let Temporal's retry policy handle it.
		return types.DetectLanguageResult{}, fmt.Errorf("listing repository root: %w", err)
	}

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e] = true
	}

	for _, m := range languageMarkers {
		if present[m.File] {
			logger.Info("Detected language", "language", m.Language, "marker", m.File)
			return types.DetectLanguageResult{Language: string(m.Language)}, nil
		}
	}

	// No marker matched — a human must specify the language explicitly via the
	// override field. Non-retryable: re-probing the same tree won't change this.
	return types.DetectLanguageResult{}, temporal.NewNonRetryableApplicationError(
		"could not detect repository language from root marker files; supply the language explicitly",
		"ValidationError", nil)
}
