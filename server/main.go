// Command server exposes an HTTP API (Gin) for triggering and steering coding-agent
// workflows. It is a thin, STATELESS adapter over the Temporal client: Temporal
// holds all durable state, so the server needs no database of its own.
//
// Routes:
//
//	POST /v1/runs              start a workflow; returns {workflow_id, run_id}
//	GET  /v1/runs/:id          describe a run + return its result when finished
//	POST /v1/runs/:id/signal   supply missing context to a halted run (human-in-the-loop)
//
// The signal route is how a frontend lets a user unblock a workflow that paused
// because the Atlassian context-gathering agent reported a gap — the production
// equivalent of pasting context into the Temporal debug UI.
//
// Interactive API docs (Swagger UI) are served at /swagger/index.html. The
// OpenAPI spec under docs/ is generated from the annotations below by `swag init`
// (see `make swagger`); regenerate it whenever a handler or its types change.
//
// Run:  go run ./server   (reads .env; needs a reachable Temporal frontend)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"go.temporal.io/sdk/client"

	docs "github.com/syncopatedNote/tagger/server/docs"
	"github.com/syncopatedNote/tagger/types"
	"github.com/syncopatedNote/tagger/workflows"
)

// @title           Coding Agent API
// @version         1.0
// @description     Trigger and steer issue-driven coding-agent workflows. A thin,
// @description     stateless adapter over Temporal — Temporal holds all durable state.
// @BasePath        /
//
// @tag.name        runs
// @tag.description Start, inspect, and unblock coding-agent runs
func main() {
	// Serve Swagger relative to the configured host:port at runtime, so the
	// "Try it out" buttons hit this same server regardless of where it's bound.
	docs.SwaggerInfo.BasePath = "/"

	_ = godotenv.Load()

	hostPort := os.Getenv("TEMPORAL_HOSTPORT")
	if hostPort == "" {
		hostPort = client.DefaultHostPort
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	c, err := client.Dial(client.Options{HostPort: hostPort})
	if err != nil {
		log.Fatalf("unable to create Temporal client: %v", err)
	}
	defer c.Close()

	s := &server{temporal: c}

	r := gin.Default()
	// authMiddleware is the seam for OAuth/JWT validation. It is a no-op stub
	// today; wire golang.org/x/oauth2 + a JWT check here and every /v1 route is
	// protected. Kept as a single chokepoint so auth is impossible to forget.
	v1 := r.Group("/v1", authMiddleware())
	{
		v1.POST("/runs", s.startRun)
		v1.GET("/runs/:id", s.getRun)
		v1.POST("/runs/:id/signal", s.signalRun)
	}
	r.GET("/healthz", healthz)

	// Interactive API docs. The spec is generated into server/docs by `swag init`
	// (make swagger). Browse at /swagger/index.html.
	r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	log.Printf("HTTP API listening on %s (temporal=%s)", addr, hostPort)
	if err := r.Run(addr); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

type server struct {
	temporal client.Client
}

// healthResponse is the GET /healthz body.
type healthResponse struct {
	OK bool `json:"ok" example:"true"`
}

// errorResponse is the shape every handler returns on failure.
type errorResponse struct {
	Error string `json:"error" example:"repo_url is required"`
}

// healthz reports liveness.
//
// @Summary  Liveness probe
// @Tags     health
// @Produce  json
// @Success  200 {object} healthResponse
// @Router   /healthz [get]
func healthz(g *gin.Context) { g.JSON(http.StatusOK, healthResponse{OK: true}) }

// startRunRequest is the POST /v1/runs body.
type startRunRequest struct {
	IssueReference string `json:"issue_reference" binding:"required" example:"PROJ-123"`
	RepoURL        string `json:"repo_url" binding:"required" example:"https://github.com/owner/repo"`
	BaseBranch     string `json:"base_branch" example:"main"`
	// Language optionally overrides the coding-agent toolchain ("go", "python",
	// ...). Omit it to have the workflow auto-detect from the repo.
	Language string `json:"language" example:"python"`
}

// startRunResponse is returned when a run is accepted.
type startRunResponse struct {
	WorkflowID string `json:"workflow_id" example:"coding-agent-PROJ-123-1718800000"`
	RunID      string `json:"run_id" example:"7b674799-1c95-4b10-975a-f90968d88036"`
	StatusURL  string `json:"status_url" example:"/v1/runs/coding-agent-PROJ-123-1718800000"`
}

// startRun kicks off a coding-agent workflow for an issue.
//
// @Summary  Start a run
// @Tags     runs
// @Accept   json
// @Produce  json
// @Param    request body     startRunRequest true "Run parameters"
// @Success  202     {object} startRunResponse
// @Failure  400     {object} errorResponse "invalid body"
// @Failure  500     {object} errorResponse "failed to start the workflow"
// @Router   /v1/runs [post]
func (s *server) startRun(g *gin.Context) {
	var req startRunRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		g.JSON(http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}

	// A stable-but-unique workflow id: the issue key plus a timestamp. Using the
	// issue key alone would make Temporal reject a second run for the same ticket
	// while one is in flight — append the timestamp so re-runs are allowed.
	workflowID := fmt.Sprintf("coding-agent-%s-%d", req.IssueReference, time.Now().Unix())

	ctx, cancel := context.WithTimeout(g.Request.Context(), 10*time.Second)
	defer cancel()

	run, err := s.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: workflows.TaskQueue,
	}, workflows.CodingAgentWorkflow, types.CodingAgentInput{
		IssueReference: req.IssueReference,
		RepoURL:        req.RepoURL,
		BaseBranch:     req.BaseBranch,
		Language:       req.Language,
	})
	if err != nil {
		g.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	g.JSON(http.StatusAccepted, startRunResponse{
		WorkflowID: run.GetID(),
		RunID:      run.GetRunID(),
		StatusURL:  "/v1/runs/" + run.GetID(),
	})
}

// runStatusResponse is the GET /v1/runs/:id body. Result is populated only once
// the run has finished; Error carries a terminal workflow failure.
type runStatusResponse struct {
	WorkflowID string                   `json:"workflow_id" example:"coding-agent-PROJ-123-1718800000"`
	Status     string                   `json:"status" example:"Running"`
	Result     *types.CodingAgentResult `json:"result,omitempty"`
	Error      string                   `json:"error,omitempty"`
}

// getRun reports a run's status, plus its result once finished.
//
// @Summary  Get run status
// @Tags     runs
// @Produce  json
// @Param    id  path     string true "Workflow ID"
// @Success  200 {object} runStatusResponse
// @Failure  404 {object} errorResponse "run not found"
// @Router   /v1/runs/{id} [get]
func (s *server) getRun(g *gin.Context) {
	id := g.Param("id")

	ctx, cancel := context.WithTimeout(g.Request.Context(), 10*time.Second)
	defer cancel()

	desc, err := s.temporal.DescribeWorkflowExecution(ctx, id, "")
	if err != nil {
		g.JSON(http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}

	resp := runStatusResponse{
		WorkflowID: id,
		Status:     desc.GetWorkflowExecutionInfo().GetStatus().String(),
	}

	// If the run has finished, fetch the result (non-blocking: we already know
	// it's closed). A non-nil error here is the workflow's terminal failure.
	if desc.GetWorkflowExecutionInfo().GetCloseTime() != nil {
		var result types.CodingAgentResult
		if err := s.temporal.GetWorkflow(ctx, id, "").Get(ctx, &result); err != nil {
			resp.Error = err.Error()
		} else {
			resp.Result = &result
		}
	}
	g.JSON(http.StatusOK, resp)
}

// signalRunRequest is the POST /v1/runs/:id/signal body. It carries the missing
// context a human is supplying to unblock a halted gather phase.
type signalRunRequest struct {
	Info string `json:"info" binding:"required" example:"design doc: https://acme.atlassian.net/wiki/..."`
}

// signalResponse confirms a signal was delivered.
type signalResponse struct {
	WorkflowID string `json:"workflow_id" example:"coding-agent-PROJ-123-1718800000"`
	Signaled   string `json:"signaled" example:"supply-context"`
}

// signalRun delivers human-supplied context to unblock a halted run.
//
// @Summary  Signal a run (supply missing context)
// @Tags     runs
// @Accept   json
// @Produce  json
// @Param    id      path     string           true "Workflow ID"
// @Param    request body     signalRunRequest true "Context to supply"
// @Success  200     {object} signalResponse
// @Failure  400     {object} errorResponse "invalid body"
// @Failure  502     {object} errorResponse "failed to deliver the signal"
// @Router   /v1/runs/{id}/signal [post]
func (s *server) signalRun(g *gin.Context) {
	id := g.Param("id")
	var req signalRunRequest
	if err := g.ShouldBindJSON(&req); err != nil {
		g.JSON(http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(g.Request.Context(), 10*time.Second)
	defer cancel()

	err := s.temporal.SignalWorkflow(ctx, id, "", workflows.SupplyContextSignal,
		types.ContextSupplement{Info: req.Info})
	if err != nil {
		g.JSON(http.StatusBadGateway, errorResponse{Error: err.Error()})
		return
	}
	g.JSON(http.StatusOK, signalResponse{WorkflowID: id, Signaled: workflows.SupplyContextSignal})
}

// authMiddleware is a placeholder for OAuth/JWT validation. Replace the body with
// real token verification (e.g. validate a bearer JWT against your IdP's JWKS).
// Returning early with 401 here blocks the request before it reaches Temporal.
func authMiddleware() gin.HandlerFunc {
	required := os.Getenv("API_AUTH_REQUIRED") == "true"
	return func(g *gin.Context) {
		if !required {
			g.Next()
			return
		}
		if g.GetHeader("Authorization") == "" {
			g.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"error": errors.New("missing Authorization header").Error()})
			return
		}
		// TODO: validate the bearer token here.
		g.Next()
	}
}
