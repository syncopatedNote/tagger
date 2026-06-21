// Package agents defines the per-language coding-agent toolchains and the
// CodingAgent interface they implement. It is a pure, dependency-light catalog:
// every method returns plain data (image names, cache paths, argv slices, prose)
// and the package imports neither Dagger nor the Temporal SDK. The activities
// layer consumes this data to build the actual Dagger objects and to drive the
// shared coding loop, so the import direction is strictly one-way
// (activities -> agent_registry -> agents) with no cycle back.
package agents

import (
	"fmt"
	"strings"
)

// Language is the canonical identifier for a supported coding-agent toolchain.
// It is the short string that crosses the workflow/activity boundary (claim-check)
// and the key the registry looks up.
type Language string

const (
	LangGo     Language = "go"
	LangPython Language = "python"
	LangJS     Language = "javascript" // recognised by detection; agent not yet built
	LangJava   Language = "java"       // recognised by detection; agent not yet built
)

// CacheMount describes one Dagger cache volume mount: a container path backed by
// a named, persistent cache volume. Kept as plain data so this package never
// imports Dagger — the activity translates each into client.CacheVolume(Volume)
// mounted at Path.
type CacheMount struct {
	// Path is the absolute mount point inside the workspace container, e.g.
	// "/go/pkg/mod" or "/root/.cache/pip".
	Path string
	// Volume is the stable name of the Dagger cache volume backing Path. Sharing
	// a name across runs is what makes dependency downloads incremental.
	Volume string
}

// CodingAgent is the per-language toolchain contract. The single, shared coding
// loop in activities.RunCodingAgentActivity drives ANY implementation purely
// through these methods — the loop itself (connect, env-bind, self-heal, verify,
// publish) is entirely language-agnostic. Adding a language means adding one
// implementation here and registering it; no workflow or activity logic changes.
type CodingAgent interface {
	// Language returns the agent's canonical identifier.
	Language() Language
	// BaseImage is the workspace container image, which must contain the
	// language's toolchain (compiler/interpreter + package manager), e.g.
	// "golang:1.23" or "python:3.12".
	BaseImage() string
	// CacheMounts lists the dependency/build caches to mount into the workspace
	// so repeated runs don't re-download the world. May be empty.
	CacheMounts() []CacheMount
	// WarmupExec is the argv run once after the source is mounted to pre-fetch
	// dependencies (e.g. ["go","mod","download"] or ["pip","install","-r",
	// "requirements.txt"]). Returning nil skips the warm-up step.
	WarmupExec() []string
	// TestExec is the argv of the verification gate — the suite the activity
	// re-runs itself, never trusting the agent's self-report (e.g.
	// ["go","test","./..."] or ["pytest"]).
	TestExec() []string
	// Persona is the engineer role used in the system prompt, e.g.
	// "senior Go engineer". It tunes the model to the language's idioms.
	Persona() string
}

// BuildSystemPrompt assembles the coding agent's system prompt for a given
// toolchain. Rules 1–7 are identical across languages; only the persona and the
// test command are interpolated from the agent. Keeping the template here (next
// to the interface) means a new language gets a correct prompt for free.
func BuildSystemPrompt(a CodingAgent) string {
	testCmd := strings.Join(a.TestExec(), " ")
	return fmt.Sprintf(`You are an autonomous %s working inside a sandboxed CI environment.

You are given:
- `+"`requirements`"+`: the feature to implement, distilled from an issue tracker.
- `+"`issue`"+`: the originating issue reference, for context only.
- `+"`workspace`"+`: a container with the repository checked out at /src and the toolchain installed. Use its tools to list and read files, write files, and run commands.

Your job: implement `+"`requirements`"+` by editing files in the workspace, then return the modified container as the `+"`completed`"+` output.

Rules:
1. Explore before editing. Read the relevant files and match the codebase's existing structure and style.
2. Make the smallest change that fully satisfies the requirements. Do not reformat unrelated code or add unrequested features.
3. After every change, run the test suite with `+"`%s`"+` inside the workspace.
4. If the tests (or the build) fail, read the failure output carefully, fix your code, and run the tests again. Repeat until the entire suite passes.
5. Never weaken, skip, comment out, or delete tests to make them pass. Fix the implementation instead.
6. When you need current, version-accurate documentation for a library or framework, use the Context7 tools (resolve the library id, then fetch its docs) rather than relying on memory. Use it sparingly — each call counts against your budget.
7. Only set the `+"`completed`"+` output once `+"`%s`"+` passes cleanly. Do not finish while anything is failing.

You have no network credentials and cannot push code — a later, separate stage handles publishing. Focus solely on producing a correct, test-passing workspace.`,
		a.Persona(), testCmd, testCmd)
}
