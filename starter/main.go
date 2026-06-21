// Command starter kicks off a single CodingAgentWorkflow execution and blocks
// until it completes, printing the resulting pull-request URL.
//
// Usage:
//
//	go run ./starter \
//	  -issue "PROJ-123" \
//	  -repo  "https://github.com/owner/repo" \
//	  -base  "main"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"
	"go.temporal.io/sdk/client"

	"github.com/syncopatedNote/tagger/types"
	"github.com/syncopatedNote/tagger/workflows"
)

func main() {
	_ = godotenv.Load()

	issue := flag.String("issue", "", "issue reference (Jira key, GitHub issue, or URL)")
	repo := flag.String("repo", "", "repository clone URL")
	base := flag.String("base", "main", "base branch")
	lang := flag.String("lang", "", "coding-agent language override (e.g. go, python); empty = auto-detect")
	flag.Parse()

	if *issue == "" || *repo == "" {
		flag.Usage()
		log.Fatal("both -issue and -repo are required")
	}

	hostPort := os.Getenv("TEMPORAL_HOSTPORT")
	if hostPort == "" {
		hostPort = client.DefaultHostPort
	}

	c, err := client.Dial(client.Options{HostPort: hostPort})
	if err != nil {
		log.Fatalf("unable to create Temporal client: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		TaskQueue: workflows.TaskQueue,
	}, workflows.CodingAgentWorkflow, types.CodingAgentInput{
		IssueReference: *issue,
		RepoURL:        *repo,
		BaseBranch:     *base,
		Language:       *lang,
	})
	if err != nil {
		log.Fatalf("unable to start workflow: %v", err)
	}
	log.Printf("started workflow: id=%s runID=%s", run.GetID(), run.GetRunID())

	var result types.CodingAgentResult
	if err := run.Get(ctx, &result); err != nil {
		log.Fatalf("workflow failed: %v", err)
	}

	fmt.Printf("\n✅ Pull request: %s\n   branch: %s @ %s\n",
		result.PullRequestURL, result.BranchName, result.HeadCommitSHA)
}
