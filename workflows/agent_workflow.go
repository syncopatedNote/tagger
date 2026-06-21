// Package workflows contains the Temporal workflow definitions.
//
// DETERMINISM CONTRACT: Code in this package must be strictly deterministic. It
// never touches the network, the clock (except via workflow.Now), the
// filesystem, randomness, or — critically — the Dagger SDK. All non-deterministic
// and side-effecting work lives in activities. The workflow only orchestrates:
// it sequences activities and shuttles tiny text payloads between them.
package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/syncopatedNote/tagger/activities"
	"github.com/syncopatedNote/tagger/types"
)

// TaskQueue is the queue the worker listens on and the starter dispatches to.
const TaskQueue = "coding-agent-task-queue"

// SupplyContextSignal is the signal name a human (via the Temporal UI or the
// HTTP API) sends to unblock a workflow that halted because the Atlassian
// context-gathering agent reported missing context. The payload is a
// types.ContextSupplement. See the gather loop in CodingAgentWorkflow.
const SupplyContextSignal = "supply-context"

// CodingAgentWorkflow drives an issue from a Jira ticket to an open pull request.
//
// Phases (only short strings ever cross between them — claim-check pattern):
//
//  1. Gather context (Atlassian LLM crawl) + resolve the base commit, in
//     parallel. The gather phase loops: if the agent reports missing context,
//     the workflow HALTS and waits for a human "supply-context" signal, folds
//     the supplement in, and re-runs the gather. Only a workflow can wait like
//     this — activities cannot receive signals.
//  2. Run the sandboxed LLM coding loop; returns a branch name + head SHA.
//  3. Open the PR for that branch.
//
// The repository contents never enter the workflow; they are brokered through
// the Git remote.
func CodingAgentWorkflow(ctx workflow.Context, input types.CodingAgentInput) (*types.CodingAgentResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("CodingAgentWorkflow started",
		"issue", input.IssueReference, "repo", input.RepoURL, "baseBranch", input.BaseBranch)

	if input.IssueReference == "" || input.RepoURL == "" {
		return nil, temporal.NewNonRetryableApplicationError(
			"IssueReference and RepoURL are required", "ValidationError", nil)
	}

	// A nil receiver is used purely so the Temporal SDK can resolve the
	// activity *names* from the typed method values below. The methods are never
	// invoked here — the real, dependency-injected *activities.Activities
	// instance lives on the worker. This keeps the workflow free of any concrete
	// dependency on Dagger, secrets, or HTTP clients.
	var a *activities.Activities

	// Retry policy for short, idempotent, network-bound activities (commit
	// resolution, PR creation). These fail mostly for transient reasons, so retry
	// generously — but never retry genuine validation failures.
	infraRetry := &temporal.RetryPolicy{
		InitialInterval:    2 * time.Second,
		BackoffCoefficient: 2.0,
		MaximumInterval:    time.Minute,
		MaximumAttempts:    5,
		NonRetryableErrorTypes: []string{
			"ValidationError", // bad input — retrying can never help.
		},
	}

	// -------------------------------------------------------------------------
	// Phase 1a: resolve the immutable base commit. Fast; runs concurrently with
	// the (slower) context gather below via a Temporal future.
	// -------------------------------------------------------------------------
	resolveCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         infraRetry,
	})
	baseBranch := input.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	commitFuture := workflow.ExecuteActivity(resolveCtx, a.ResolveBaseCommitActivity,
		types.ResolveBaseCommitInput{RepoURL: input.RepoURL, BaseBranch: baseBranch})

	// -------------------------------------------------------------------------
	// Phase 1b: gather Atlassian context, halting for human signals on gaps.
	// -------------------------------------------------------------------------
	gathered, err := gatherContextWithSignals(ctx, a, input.IssueReference)
	if err != nil {
		return nil, err
	}
	logger.Info("Context gathered", "title", gathered.Title)

	// Join the base-commit future before the expensive coding phase.
	var commit types.ResolveBaseCommitResult
	if err := commitFuture.Get(ctx, &commit); err != nil {
		return nil, err
	}
	logger.Info("Base commit resolved", "baseCommit", commit.BaseCommitSHA)

	// -------------------------------------------------------------------------
	// Phase 1c: select the coding-agent toolchain. An explicit input.Language is
	// an override that wins outright; otherwise detect it from the repository's
	// root marker files at the pinned commit. Detection is an activity (it does
	// I/O); the workflow only routes on the resulting string. A failure to
	// determine the language is fatal — we never guess and run the wrong
	// toolchain.
	// -------------------------------------------------------------------------
	language, err := selectLanguage(ctx, a, input.Language, input.RepoURL, commit.BaseCommitSHA)
	if err != nil {
		return nil, err
	}
	logger.Info("Coding-agent language selected", "language", language)

	// -------------------------------------------------------------------------
	// Phase 2: the Dagger sandbox coding agent. Long-running and expensive.
	//
	// Distinct, tighter options than the infra activities:
	//   - StartToCloseTimeout caps the *wall-clock* cost of a single attempt and
	//     is the backstop against a runaway LLM loop (see Verification Q2).
	//   - HeartbeatTimeout lets Temporal detect a dead/stuck worker in ~2 min
	//     instead of waiting out the full 45-minute StartToCloseTimeout.
	//   - MaximumAttempts is 2: re-running a 30-minute LLM loop is costly, so we
	//     allow exactly one retry for transient infra faults and no more.
	//   - AgentExhaustedError is non-retryable: if the agent burned its loop
	//     budget without making the tests pass, a blind retry will likely do the
	//     same. Surface it for human triage instead.
	// -------------------------------------------------------------------------
	daggerCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 45 * time.Minute,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    10 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    2 * time.Minute,
			MaximumAttempts:    2,
			NonRetryableErrorTypes: []string{
				"ValidationError",
				"AgentExhaustedError",
			},
		},
	})
	var agentResult types.RunCodingAgentResult
	daggerInput := types.RunCodingAgentInput{
		RepoURL:        input.RepoURL,
		BaseCommitSHA:  commit.BaseCommitSHA,
		Requirements:   gathered.Requirements,
		IssueReference: input.IssueReference,
		Language:       language,
	}
	if err := workflow.ExecuteActivity(daggerCtx, a.RunCodingAgentActivity, daggerInput).Get(daggerCtx, &agentResult); err != nil {
		return nil, err
	}
	logger.Info("Agent produced a branch", "branch", agentResult.BranchName, "head", agentResult.HeadCommitSHA)

	// -------------------------------------------------------------------------
	// Phase 3: open the pull request for the pushed branch. Fast.
	// -------------------------------------------------------------------------
	prCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         infraRetry,
	})
	prInput := types.CreatePullRequestInput{
		RepoURL:       input.RepoURL,
		BaseBranch:    baseBranch,
		FeatureBranch: agentResult.BranchName,
		Title:         gathered.Title,
		Body:          buildPRBody(input.IssueReference, gathered.Requirements),
	}
	var prResult types.CreatePullRequestResult
	if err := workflow.ExecuteActivity(prCtx, a.CreatePullRequestActivity, prInput).Get(prCtx, &prResult); err != nil {
		return nil, err
	}

	logger.Info("CodingAgentWorkflow completed", "pr", prResult.PullRequestURL)
	return &types.CodingAgentResult{
		PullRequestURL: prResult.PullRequestURL,
		BranchName:     agentResult.BranchName,
		HeadCommitSHA:  agentResult.HeadCommitSHA,
	}, nil
}

// gatherContextWithSignals runs the Atlassian context-gathering agent and, if it
// reports missing context, HALTS the workflow to wait for a human "supply-context"
// signal. Each supplement is folded into the next attempt. The loop continues
// until the agent reports a complete bundle.
//
// This is the canonical Temporal human-in-the-loop pattern: the workflow blocks
// on a signal channel for as long as it takes (Temporal persists the state and
// rehydrates on signal — the worker holds nothing in memory while waiting). An
// activity could never do this, which is exactly why the completeness verdict is
// computed in the activity but ACTED ON here in the workflow.
func gatherContextWithSignals(ctx workflow.Context, a *activities.Activities, issueRef string) (types.GatherContextResult, error) {
	logger := workflow.GetLogger(ctx)

	// The gather activity is an LLM crawl: long enough to need a generous timeout
	// and a heartbeat, but it never retries an exhausted/validation failure.
	gatherCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		// The crawl runs as ONE blocking Dagger call (the whole LLM loop), so a
		// single slow model turn can exceed a tight heartbeat window and be
		// mistaken for a dead worker. 5m tolerates a slow Bedrock/Nova turn while
		// still catching a genuinely hung worker well before StartToClose.
		HeartbeatTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    3,
			NonRetryableErrorTypes: []string{
				"ValidationError",
				"AgentExhaustedError",
			},
		},
	})

	supplements := []string{}
	signalCh := workflow.GetSignalChannel(ctx, SupplyContextSignal)

	for {
		var result types.GatherContextResult
		in := types.GatherContextInput{IssueReference: issueRef, Supplements: supplements}
		if err := workflow.ExecuteActivity(gatherCtx, a.RunContextAgentActivity, in).Get(ctx, &result); err != nil {
			return types.GatherContextResult{}, err
		}
		if result.Complete {
			return result, nil
		}

		// Context is incomplete. Surface the gaps and HALT until a human signals.
		// The workflow can sit here for days at zero compute cost.
		logger.Info("Context incomplete — awaiting human supply-context signal",
			"missing", result.Missing)

		var supplement types.ContextSupplement
		signalCh.Receive(ctx, &supplement) // blocks until a signal arrives
		logger.Info("Received context supplement; retrying gather", "info", supplement.Info)
		if supplement.Info != "" {
			supplements = append(supplements, supplement.Info)
		}
	}
}

// selectLanguage resolves which coding-agent toolchain to use. An explicit
// override (input.Language) wins immediately with no activity call. Otherwise it
// runs DetectLanguageActivity against the pinned commit. This is the workflow's
// "selection step": a deterministic string decision plus, at most, one
// conditional activity — fully legal inside the deterministic workflow because
// the only I/O (probing repo files) happens inside the activity.
func selectLanguage(ctx workflow.Context, a *activities.Activities, override, repoURL, baseCommitSHA string) (string, error) {
	if override != "" {
		return override, nil
	}

	// Detection is a cheap, idempotent, file-presence probe — retry generously on
	// transient transport errors, but never on a genuine "can't detect" verdict.
	detectCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    5,
			NonRetryableErrorTypes: []string{
				"ValidationError",
			},
		},
	})

	var result types.DetectLanguageResult
	in := types.DetectLanguageInput{RepoURL: repoURL, BaseCommitSHA: baseCommitSHA}
	if err := workflow.ExecuteActivity(detectCtx, a.DetectLanguageActivity, in).Get(ctx, &result); err != nil {
		return "", err
	}
	return result.Language, nil
}

// buildPRBody assembles the PR description. Pure string work, so it is safe to
// run inside the deterministic workflow.
func buildPRBody(issueRef, requirements string) string {
	return "Automated change generated by the issue coding agent.\n\n" +
		"Closes/relates to: " + issueRef + "\n\n" +
		"## Requirements\n\n" + requirements + "\n\n" +
		"---\n_All tests (`go test ./...`) passed in the sandbox before this branch was pushed._\n"
}
