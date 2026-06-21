// Package main is a Dagger module that exposes a small, curated developer
// "toolbox" to an LLM as FIRST-CLASS, NAMED tools.
//
// Why this exists alongside the inline Container binding in
// activities/coding_agent_activity.go:
//
//   - The Temporal activity binds a *Container* to the LLM Env. That works in a
//     plain Dagger SDK client (no codegen) and surfaces the Container's generic
//     operations (read/write/exec) to the model. The tools are real and schema'd,
//     but named after Container methods.
//
//   - This module instead declares a typed `Toolbox` object whose methods —
//     ReadFile, WriteFile, RunTests — become DOMAIN-SPECIFIC, nicely-named tools
//     when the object is bound to an Env. This is the idiomatic Dagger way to give
//     an agent a tight, legible tool surface.
//
// Each public method of an object bound into an Env is offered to the LLM as a
// callable tool whose JSON schema Dagger derives from the Go signature.
//
// Build / run from this directory:
//
//	dagger develop                       # generates ./internal/dagger
//	dagger call develop --source . --assignment "Add a /healthz endpoint" export --path ./out
package main

import (
	"context"
	"dagger/toolbox/internal/dagger"
)

// Toolbox is the agent's workspace. It holds the source directory under edit and
// exposes the operations the agent is allowed to perform on it.
type Toolbox struct {
	// Source is the working tree the agent reads from and writes to.
	// +private
	Source *dagger.Directory
}

// New constructs a Toolbox around a source directory.
func New(
	// The source directory the agent will work on.
	source *dagger.Directory,
) *Toolbox {
	return &Toolbox{Source: source}
}

// ReadFile returns the contents of a file in the workspace.
//
// Surfaces to the LLM as the `read_file` tool.
func (t *Toolbox) ReadFile(
	ctx context.Context,
	// Path to the file, relative to the workspace root (e.g. "cmd/main.go").
	path string,
) (string, error) {
	return t.Source.File(path).Contents(ctx)
}

// WriteFile writes (creating or replacing) a file in the workspace and returns
// the updated Toolbox. Immutable: callers chain the returned value.
//
// Surfaces to the LLM as the `write_file` tool.
func (t *Toolbox) WriteFile(
	// Path to the file, relative to the workspace root.
	path string,
	// Full new contents of the file.
	contents string,
) *Toolbox {
	t.Source = t.Source.WithNewFile(path, contents)
	return t
}

// RunTests runs `go test ./...` against the current workspace and returns the
// combined output. It deliberately does NOT fail on a non-zero exit code: the
// test output (including failures) is returned to the model so it can read the
// errors and self-correct. This is the self-healing feedback loop.
//
// Surfaces to the LLM as the `run_tests` tool.
func (t *Toolbox) RunTests(ctx context.Context) (string, error) {
	return dag.Container().
		From("golang:1.23").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("go-build")).
		WithDirectory("/src", t.Source).
		WithWorkdir("/src").
		WithExec([]string{"go", "test", "./..."}, dagger.ContainerWithExecOpts{
			Expect: dagger.ReturnTypeAny, // capture output regardless of exit code
		}).
		CombinedOutput(ctx)
}

// Directory returns the (possibly edited) workspace directory.
func (t *Toolbox) Directory() *dagger.Directory {
	return t.Source
}

// Develop runs the full agent loop: it binds this Toolbox to an LLM Env — so the
// model sees `read_file`, `write_file`, and `run_tests` as tools — assigns the
// task, and returns the completed working tree.
//
// This is the "named-tools" counterpart to RunCodingAgentActivity. Invoke it
// with `dagger call develop ...`, or from a Temporal activity that drives this
// module instead of the inline Container approach.
func (t *Toolbox) Develop(
	ctx context.Context,
	// The feature to implement, distilled from the issue tracker.
	assignment string,
) (*dagger.Directory, error) {
	environment := dag.Env().
		WithStringInput("assignment", assignment,
			"the feature to implement; satisfy it exactly").
		WithToolboxInput("workspace", t,
			"the workspace; use read_file, write_file, and run_tests to do the work").
		WithToolboxOutput("completed",
			"the workspace after the feature is implemented and all tests pass")

	work := dag.LLM(dagger.LLMOpts{MaxAPICalls: 25}).
		WithEnv(environment).
		WithPrompt(`You are an expert Go engineer.

Implement the ` + "`assignment`" + ` using the workspace tools:
- read_file to inspect files,
- write_file to create or modify files,
- run_tests to run ` + "`go test ./...`" + `.

Work iteratively: make a change, run the tests, and if anything fails read the
output and fix it. Never weaken or delete tests. Finish only when run_tests
reports that the whole suite passes, then return the workspace as ` + "`completed`" + `.`)

	completed := work.Env().Output("completed").AsToolbox()
	return completed.Directory(), nil
}
