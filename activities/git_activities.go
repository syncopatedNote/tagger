// This file holds the two git / control-plane activities: ResolveBaseCommitActivity
// (pin the base branch to an immutable SHA) and CreatePullRequestActivity (open
// the PR for the pushed branch), plus the git-URL parsing helpers they share.
//
// The shared Activities struct and its NewActivities constructor live in
// activity_base.go; cross-cutting helpers live in common_activities.go.
package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/syncopatedNote/tagger/types"
)

// -----------------------------------------------------------------------------
// Activity: ResolveBaseCommitActivity
// -----------------------------------------------------------------------------

// ResolveBaseCommitActivity resolves the current HEAD of the base branch into an
// immutable commit SHA, pinning the agent to a reproducible starting point. It
// returns only that short string.
func (a *Activities) ResolveBaseCommitActivity(ctx context.Context, in types.ResolveBaseCommitInput) (types.ResolveBaseCommitResult, error) {
	logger := activity.GetLogger(ctx)

	if strings.TrimSpace(in.RepoURL) == "" {
		return types.ResolveBaseCommitResult{}, temporal.NewNonRetryableApplicationError(
			"RepoURL is required", "ValidationError", nil)
	}
	branch := in.BaseBranch
	if branch == "" {
		branch = "main"
	}

	sha, err := a.resolveBaseCommit(ctx, in.RepoURL, branch)
	if err != nil {
		return types.ResolveBaseCommitResult{}, err
	}
	logger.Info("Resolved base commit", "baseBranch", branch, "baseCommit", sha)
	return types.ResolveBaseCommitResult{BaseCommitSHA: sha}, nil
}

// resolveBaseCommit returns the commit SHA at the tip of branch in repoURL via
// the GitHub REST API. No git binary is required — the worker's GITHUB_TOKEN is
// the only credential needed, and the call is a plain HTTPS request.
func (a *Activities) resolveBaseCommit(ctx context.Context, repoURL, branch string) (string, error) {
	owner, repo, err := parseGitHubRepo(repoURL)
	if err != nil {
		return "", temporal.NewNonRetryableApplicationError(
			"could not parse owner/repo from RepoURL", "ValidationError", err)
	}

	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/branches/%s", owner, repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("building branch request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching branch info: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	if closeErr != nil {
		return "", fmt.Errorf("closing response body: %w", closeErr)
	}

	if resp.StatusCode == http.StatusNotFound {
		return "", temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("branch %q not found in %s", branch, repoURL), "ValidationError", nil)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("github API %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing branch response: %w", err)
	}
	if result.Commit.SHA == "" {
		return "", fmt.Errorf("empty commit SHA in branch response for %s/%s", owner, repo)
	}
	return result.Commit.SHA, nil
}

// -----------------------------------------------------------------------------
// Activity 3: CreatePullRequestActivity
// -----------------------------------------------------------------------------

// CreatePullRequestActivity opens a GitHub pull request for the feature branch
// the Dagger agent pushed. By default it runs against a simulated client so the
// pipeline is runnable end-to-end without live credentials; set
// AGENT_SIMULATE_PR=false (and provide GITHUB_TOKEN) to hit the real API.
func (a *Activities) CreatePullRequestActivity(ctx context.Context, in types.CreatePullRequestInput) (types.CreatePullRequestResult, error) {
	logger := activity.GetLogger(ctx)

	owner, repo, err := parseGitHubRepo(in.RepoURL)
	if err != nil {
		return types.CreatePullRequestResult{}, temporal.NewNonRetryableApplicationError(
			"could not parse owner/repo from RepoURL", "ValidationError", err)
	}

	if a.simulatePR || a.githubToken == "" {
		url := fmt.Sprintf("https://github.com/%s/%s/pull/0", owner, repo)
		logger.Info("Simulated PR creation (set AGENT_SIMULATE_PR=false for the live API)",
			"url", url, "head", in.FeatureBranch, "base", in.BaseBranch)
		return types.CreatePullRequestResult{PullRequestURL: url, Number: 0}, nil
	}

	payload, _ := json.Marshal(map[string]string{
		"title": in.Title,
		"head":  in.FeatureBranch,
		"base":  in.BaseBranch,
		"body":  in.Body,
	})
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return types.CreatePullRequestResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return types.CreatePullRequestResult{}, err // retryable network error
	}
	body, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil {
		return types.CreatePullRequestResult{}, fmt.Errorf("reading response: %w", err)
	}
	if closeErr != nil {
		return types.CreatePullRequestResult{}, fmt.Errorf("closing response body: %w", closeErr)
	}

	switch {
	case resp.StatusCode == http.StatusUnprocessableEntity:
		// e.g. branch already has an open PR, or nothing to compare — retrying
		// will not change the outcome.
		return types.CreatePullRequestResult{}, temporal.NewNonRetryableApplicationError(
			"GitHub rejected the PR: "+string(body), "ValidationError", nil)
	case resp.StatusCode >= 300:
		// 5xx / rate limits / transient — let Temporal retry.
		return types.CreatePullRequestResult{}, fmt.Errorf("github API %d: %s", resp.StatusCode, string(body))
	}

	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return types.CreatePullRequestResult{}, err
	}
	logger.Info("Opened pull request", "url", out.HTMLURL, "number", out.Number)
	return types.CreatePullRequestResult{PullRequestURL: out.HTMLURL, Number: out.Number}, nil
}

// -----------------------------------------------------------------------------
// Git URL parsing helpers (shared by the coding agent + PR activities)
// -----------------------------------------------------------------------------

// repoHostPath normalises a clone URL to "host/owner/repo.git" (no scheme, no
// credentials), suitable for building an authenticated https URL.
func repoHostPath(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "git@"):
		s = strings.Replace(strings.TrimPrefix(s, "git@"), ":", "/", 1)
	case strings.HasPrefix(s, "ssh://git@"):
		s = strings.TrimPrefix(s, "ssh://git@")
	case strings.HasPrefix(s, "https://"):
		s = strings.TrimPrefix(s, "https://")
	case strings.HasPrefix(s, "http://"):
		s = strings.TrimPrefix(s, "http://")
	}
	if i := strings.Index(s, "@"); i >= 0 { // strip any embedded credentials
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, "/")
	if !strings.HasSuffix(s, ".git") {
		s += ".git"
	}
	if strings.Count(s, "/") < 2 {
		return "", fmt.Errorf("cannot parse repository path from %q", raw)
	}
	return s, nil
}

// parseGitHubRepo extracts the owner and repo segments from a clone URL.
func parseGitHubRepo(raw string) (owner, repo string, err error) {
	hp, err := repoHostPath(raw)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.TrimSuffix(hp, ".git"), "/")
	if len(parts) < 3 {
		return "", "", fmt.Errorf("cannot parse owner/repo from %q", raw)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}
