// Package types holds the tiny, dependency-free data-transfer objects that
// travel between the Temporal workflow and its activities.
//
// CLAIM-CHECK PATTERN: Nothing in this package carries source code, directories,
// or large blobs. Every field is a short primitive (a SHA, a branch name, a
// requirement string, a URL). The heavy data — the repository working tree — is
// brokered through Git remotes, *not* through Temporal's event history. This is
// what keeps workflow payloads small and the event history cheap to persist and
// replay.
package types

// CodingAgentInput is the sole input to CodingAgentWorkflow. Deliberately tiny:
// three strings that point at *where* the work lives, never the work itself.
type CodingAgentInput struct {
	// IssueReference identifies the unit of work, e.g. a Jira key ("PROJ-123"),
	// a GitHub issue ("owner/repo#42"), or a URL. The activity layer is
	// responsible for turning this into concrete requirements.
	IssueReference string
	// RepoURL is the clone URL of the target repository,
	// e.g. "https://github.com/owner/repo".
	RepoURL string
	// BaseBranch is the branch the feature branch is cut from, e.g. "main".
	BaseBranch string
	// Language optionally pins the coding-agent toolchain ("go", "python", ...).
	// When empty, the workflow detects it from the repository's marker files via
	// DetectLanguageActivity. A non-empty value is an explicit override that
	// always wins over detection.
	Language string
}

// GatherContextInput seeds the Atlassian context-gathering agent.
type GatherContextInput struct {
	// IssueReference is the Jira key (or URL) the crawl starts from.
	IssueReference string
	// Supplements is the accumulated human-supplied context from prior
	// halt-and-signal rounds. Each entry answers a previously-reported gap (e.g.
	// a Confluence URL the ticket failed to link). Empty on the first attempt.
	// CLAIM-CHECK: short strings (URLs, IDs, a sentence), never documents.
	Supplements []string
}

// GatherContextResult is the verdict of the Atlassian context-gathering agent.
// An LLM, using the read-only mcp-atlassian tools, crawls Jira -> Confluence ->
// linked issues and reports both the distilled brief and whether anything is
// still missing. The *workflow* (never the activity) acts on Complete: if it is
// false, the workflow halts for a human "supply-context" signal.
type GatherContextResult struct {
	// Requirements is the distilled, LLM-ready brief assembled from the Jira
	// ticket, its linked Confluence design, and any linked issues.
	Requirements string
	// Title is a short summary, reused as the PR title.
	Title string
	// Complete is the agent's verdict: true only if the ticket has a non-empty
	// body, at least one reachable Confluence design, and every referenced link
	// resolved. When false, the workflow halts for a human signal.
	Complete bool
	// Missing enumerates the specific gaps (human-readable) when Complete is
	// false, e.g. ["PROJ-123 links no Confluence design"]. Shown to the human in
	// the Temporal UI / API so they know exactly what to supply.
	Missing []string
}

// ContextSupplement is the payload a human sends on the "supply-context" signal
// to unblock a halted workflow. CLAIM-CHECK: short text only — a URL, an ID, or
// a sentence of clarification, never a pasted document.
type ContextSupplement struct {
	// Info is the missing context the human provides (e.g. a Confluence URL, or
	// "the design lives in PROJ-200, it isn't linked from the ticket").
	Info string
}

// ResolveBaseCommitInput asks for the immutable commit at the tip of a branch.
type ResolveBaseCommitInput struct {
	RepoURL    string
	BaseBranch string
}

// ResolveBaseCommitResult carries the pinned starting commit for the run.
type ResolveBaseCommitResult struct {
	// BaseCommitSHA pins the agent to an immutable starting point so the run is
	// reproducible even if the branch moves underneath us.
	BaseCommitSHA string
}

// DetectLanguageInput asks which toolchain a repository needs. Detection runs
// against the pinned commit so it inspects the exact tree the coding agent will
// build (no race with a moving branch).
type DetectLanguageInput struct {
	RepoURL       string
	BaseCommitSHA string
}

// DetectLanguageResult is the detected toolchain identifier.
type DetectLanguageResult struct {
	// Language is the canonical toolchain string ("go", "python", ...), chosen by
	// probing repository-root marker files in a fixed priority order.
	Language string
}

// RunCodingAgentInput is handed to the long-running Dagger activity.
type RunCodingAgentInput struct {
	RepoURL        string
	BaseCommitSHA  string
	Requirements   string
	IssueReference string
	// Language is the resolved toolchain identifier (never empty here — the
	// workflow has already applied the override-or-detect selection). The
	// activity resolves it to a CodingAgent via the registry.
	Language string
}

// RunCodingAgentResult is what the Dagger activity hands back to the workflow.
// CLAIM-CHECK: a branch name and a SHA — the *pointer* to the pushed work, never
// the work itself.
type RunCodingAgentResult struct {
	BranchName    string
	HeadCommitSHA string
	// SnapshotPath is the worker-local path where the agent's last written /src
	// tree was exported. Non-empty only when the loop failed before completing —
	// lets a human inspect the generated code without re-running the loop.
	SnapshotPath string
}

// CreatePullRequestInput is handed to the PR-opening activity.
type CreatePullRequestInput struct {
	RepoURL       string
	BaseBranch    string
	FeatureBranch string
	Title         string
	Body          string
}

// CreatePullRequestResult is the outcome of opening the PR.
type CreatePullRequestResult struct {
	PullRequestURL string
	Number         int
}

// CodingAgentResult is the workflow's final output.
type CodingAgentResult struct {
	PullRequestURL string
	BranchName     string
	HeadCommitSHA  string
}
