package activities

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"dagger.io/dagger"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"

	"github.com/syncopatedNote/tagger/agent_registry/agents"
	"github.com/syncopatedNote/tagger/types"
)

// RunCodingAgentActivity is Activity 2: the containerised, self-healing coding
// loop. It is language-agnostic: the per-language toolchain (base image, caches,
// warm-up, test command, persona) is resolved from the agent registry by
// in.Language, and every language-specific step below is driven through the
// CodingAgent interface.
//
// Flow:
//  1. Resolve the toolchain for in.Language from the registry.
//  2. Connect to the Dagger engine.
//  3. Fetch the source tree at BaseCommitSHA and mount it into a workspace
//     container built from the toolchain's image (warming its dep caches).
//  4. Hand that workspace to an LLM via a Dagger Env, with a toolchain-specific
//     system prompt. The LLM iterates — read → edit → test → fix — until the
//     suite passes. Test failures flow straight back into the model's context as
//     tool results, which is the self-healing mechanism.
//  5. Independently re-run the toolchain's test command as a verification gate
//     (never trust the agent's self-report).
//  6. In a SEPARATE container that the agent never sees, attach the Git token as
//     a Dagger secret, overlay the agent's files onto a fresh clone, commit, and
//     push a new unique feature branch.
//  7. Return only the branch name + head SHA (claim-check).
func (a *Activities) RunCodingAgentActivity(ctx context.Context, in types.RunCodingAgentInput) (types.RunCodingAgentResult, error) {
	logger := activity.GetLogger(ctx)
	info := activity.GetInfo(ctx)
	volName := snapshotVolumeName(in.IssueReference, info.Attempt)
	logger.Info("Starting coding agent",
		"attempt", info.Attempt, "baseCommit", in.BaseCommitSHA,
		"language", in.Language, "provider", a.llms[RoleCoding].Provider, "model", a.llms[RoleCoding].Model)

	// Resolve the per-language toolchain. An unsupported/unknown language is a
	// non-retryable setup error — retrying can never make it supported.
	agentTool, err := a.agents.GetAgentByLanguage(agents.Language(in.Language))
	if err != nil {
		return types.RunCodingAgentResult{}, temporal.NewNonRetryableApplicationError(
			err.Error(), "ValidationError", nil)
	}

	if a.githubToken == "" {
		return types.RunCodingAgentResult{}, temporal.NewNonRetryableApplicationError(
			"GITHUB_TOKEN is required on the worker to push the feature branch", "ValidationError", nil)
	}
	repoPath, err := repoHostPath(in.RepoURL)
	if err != nil {
		return types.RunCodingAgentResult{}, temporal.NewNonRetryableApplicationError(
			"invalid RepoURL", "ValidationError", err)
	}

	// Keep the Temporal heartbeat alive for the whole long-running call so the
	// HeartbeatTimeout can detect a dead worker quickly. The heartbeat ticker is
	// torn down on return.
	stopHeartbeat := startHeartbeat(ctx, 20*time.Second)
	defer stopHeartbeat()

	// 1. Connect to the Dagger engine (see connectDagger in common_activities.go).
	client, err := connectDagger(ctx)
	if err != nil {
		return types.RunCodingAgentResult{}, err
	}
	defer client.Close()

	// 2. Source tree at the pinned commit, mounted into a workspace built from the
	//    toolchain's image with its dependency caches. The tree contains source
	//    only — no .git, no credentials.
	source := client.Git(in.RepoURL, dagger.GitOpts{
		HTTPAuthUsername: "x-access-token",
		HTTPAuthToken:    client.SetSecret("github-token-read", a.githubToken),
	}).Commit(in.BaseCommitSHA).Tree()
	workspace := client.Container().
		From(agentTool.BaseImage()).
		WithDirectory("/src", source).
		WithWorkdir("/src")
	for _, m := range agentTool.CacheMounts() {
		workspace = workspace.WithMountedCache(m.Path, client.CacheVolume(m.Volume))
	}
	workspace = workspace.WithMountedCache("/snapshot", client.CacheVolume(volName))
	// Warm the dependency cache up front so the agent's first test run is fast.
	if warmup := agentTool.WarmupExec(); len(warmup) > 0 {
		workspace = workspace.WithExec(warmup)
	}

	testCmd := strings.Join(agentTool.TestExec(), " ")

	// 3. Declarative LLM environment: typed inputs/outputs become the model's
	//    contract. Binding `workspace` as a Container exposes that container's
	//    operations (read/write/exec) to the model as tools. The `completed`
	//    Container output is what the model must produce.
	env := client.Env().
		WithStringInput("requirements", in.Requirements,
			"the feature to implement, distilled from the issue tracker").
		WithStringInput("issue", in.IssueReference,
			"the originating issue reference, for context only").
		WithContainerInput("workspace", workspace,
			fmt.Sprintf("a container with the repo at /src; use its tools to read, write, and run `%s`", testCmd)).
		WithContainerOutput("completed",
			fmt.Sprintf("the workspace container after the feature is implemented and `%s` passes", testCmd))

	// Context7 MCP server: gives the agent up-to-date library/framework docs it
	// can't get from the local clone. It is the ONLY external MCP tool in the
	// coding loop — the repo itself is already mounted in the workspace, so no
	// GitHub/code-reading MCP is needed. Each Context7 call counts against
	// MaxAPICalls, so the budget below accounts for some doc lookups.
	context7 := a.mcpServers[mcpContext7].build(client)

	// MaxAPICalls is the Dagger-level runaway guard: it bounds how many tool /
	// model round-trips the loop may take (see Verification Q2). If your Dagger
	// version predates this option, set the DAGGER_LLM_MAX_API_CALLS env var on
	// the engine instead.
	agent := client.LLM(dagger.LLMOpts{
		Model:       a.llms[RoleCoding].Model,
		MaxAPICalls: a.MaxAgentLoops,
	}).
		WithEnv(env).
		WithMCPServer(mcpContext7, context7).
		WithPrompt(agents.BuildSystemPrompt(agentTool))

	// Pull the produced container out of the post-run environment. Calling
	// Sync forces the entire lazy agent loop to actually execute.
	completed := agent.Env().Output("completed").AsContainer()
	if _, err := completed.Sync(ctx); err != nil {
		// A Sync error here usually means the loop hit its MaxAPICalls cap or the
		// engine aborted (e.g. Temporal cancelled ctx). Either way it is not
		// worth blindly retrying the full expensive loop.
		snapshotPath := exportSnapshot(ctx, client, agentTool.BaseImage(), volName, in.IssueReference, info.Attempt, logger)
		return types.RunCodingAgentResult{SnapshotPath: snapshotPath},
			temporal.NewNonRetryableApplicationError("agent loop did not complete", "AgentExhaustedError", err)
	}

	// 4. Independent verification gate — never trust the agent's self-report.
	//    Re-run the toolchain's suite ourselves; if it still fails, the agent
	//    gave up.
	if _, err := completed.WithExec(agentTool.TestExec()).Sync(ctx); err != nil {
		return types.RunCodingAgentResult{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("agent finished but `%s` still fails", testCmd), "AgentExhaustedError", err)
	}
	logger.Info("Agent passed verification; publishing branch")
	cleanupSnapshot(ctx, client, agentTool.BaseImage(), volName, logger)

	// 5. Publish. The Git token is attached ONLY to this pusher container, which
	//    the LLM never had access to. SECRET CUSTODY (Verification Q3): the
	//    secret is provided as a Dagger Secret and exposed via WithSecretVariable,
	//    so it is scrubbed from build caches and logs and is never part of the
	//    agent's workspace context.
	finalDir := completed.Directory("/src")
	branch := featureBranchName(in.IssueReference)
	secret := client.SetSecret("github-token", a.githubToken)

	cloneScript := strings.Join([]string{
		"set -e",
		`git config --global user.email "agent@coding-agent.dev"`,
		`git config --global user.name "Issue Coding Agent"`,
		// Clone with the token, then immediately scrub it from .git/config so it
		// is not persisted in this container's filesystem.
		fmt.Sprintf(`git clone --quiet "https://x-access-token:${GITHUB_TOKEN}@%s" /repo`, repoPath),
		"cd /repo",
		fmt.Sprintf(`git remote set-url origin "https://%s"`, repoPath),
		fmt.Sprintf(`git checkout --quiet %s`, in.BaseCommitSHA),
		fmt.Sprintf(`git switch -c %s`, branch),
	}, "\n")

	commitMsg := fmt.Sprintf("feat: implement %s", in.IssueReference)
	pushScript := strings.Join([]string{
		"set -e",
		"git add -A",
		fmt.Sprintf(`git commit --quiet -m %s || echo "no changes to commit"`, shellSingleQuote(commitMsg)),
		// The token is supplied inline on the push command only (process-scoped),
		// not written to git config.
		fmt.Sprintf(`git push --quiet "https://x-access-token:${GITHUB_TOKEN}@%s" "%s:%s"`, repoPath, branch, branch),
		"git rev-parse HEAD",
	}, "\n")

	pusher := client.Container().
		From(a.GitImage).
		WithSecretVariable("GITHUB_TOKEN", secret).
		// Bust the layer cache so each attempt performs a fresh clone/push.
		WithEnvVariable("CACHEBUST", time.Now().UTC().Format(time.RFC3339Nano)).
		WithExec([]string{"sh", "-c", cloneScript}).
		// Overlay the agent's edited tree on top of the clone (.git is excluded
		// so the clone's history is preserved).
		WithDirectory("/repo", finalDir, dagger.ContainerWithDirectoryOpts{
			Exclude: []string{".git", ".git/**"},
		}).
		WithWorkdir("/repo")

	out, err := pusher.WithExec([]string{"sh", "-c", pushScript}).Stdout(ctx)
	if err != nil {
		// Push failures (auth, protected branch) are typically retryable infra
		// issues — return a plain error so Temporal's retry policy applies.
		return types.RunCodingAgentResult{}, fmt.Errorf("publishing feature branch: %w", err)
	}

	headSHA := lastNonEmptyLine(out)
	logger.Info("Pushed feature branch", "branch", branch, "head", headSHA)
	return types.RunCodingAgentResult{BranchName: branch, HeadCommitSHA: headSHA}, nil
}

// featureBranchName builds a unique, push-safe branch name for the run.
func featureBranchName(issueRef string) string {
	return fmt.Sprintf("agent/%s-%s", slugify(issueRef), randHex(4))
}

// slugify lowercases and keeps only [a-z0-9-], collapsing other runes to '-'.
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "task"
	}
	return out
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is exceptional; fall back to a time-based suffix.
		return fmt.Sprintf("%x", time.Now().UnixNano())[:n*2]
	}
	return hex.EncodeToString(buf)
}

// shellSingleQuote POSIX-quotes s for safe interpolation into an `sh -c` string.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// snapshotVolumeName returns a unique Dagger cache volume name scoped to a
// specific issue and attempt so concurrent or retried runs never share state.
func snapshotVolumeName(issueRef string, attempt int32) string {
	return fmt.Sprintf("tagger-snapshot-%s-%d", slugify(issueRef), attempt)
}

// exportSnapshot reads the /snapshot cache volume the agent populated during its
// loop and exports its contents to the worker filesystem. Returns the export path
// on success, or an empty string if the volume was empty or the export failed.
func exportSnapshot(ctx context.Context, client *dagger.Client, baseImage, volName, issueRef string, attempt int32, logger log.Logger) string {
	exportPath := fmt.Sprintf("/tmp/tagger-snapshot-%s-%d", slugify(issueRef), attempt)
	reader := client.Container().
		From(baseImage).
		WithMountedCache("/snapshot", client.CacheVolume(volName))
	if _, err := reader.Directory("/snapshot").Export(ctx, exportPath); err != nil {
		logger.Warn("Agent loop failed; no snapshot available (agent may not have reached code-writing stage)",
			"exportErr", err)
		return ""
	}
	logger.Warn("Agent loop failed; code snapshot exported", "path", exportPath, "volume", volName)
	return exportPath
}

// cleanupSnapshot scrubs the /snapshot cache volume after a successful run so a
// future retry of the same issue starts with an empty snapshot.
func cleanupSnapshot(ctx context.Context, client *dagger.Client, baseImage, volName string, logger log.Logger) {
	cleanup := client.Container().
		From(baseImage).
		WithMountedCache("/snapshot", client.CacheVolume(volName)).
		WithExec([]string{"sh", "-c", "rm -rf /snapshot/*"})
	if _, err := cleanup.Sync(ctx); err != nil {
		logger.Warn("Failed to clean up snapshot volume (non-fatal)", "volume", volName, "err", err)
	}
}
