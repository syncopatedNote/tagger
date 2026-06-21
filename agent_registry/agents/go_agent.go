package agents

// goAgent is the Go toolchain. Unexported: instances are obtained only through
// the registry (agent_registry.New), never constructed ad hoc. It holds no
// state — it is pure toolchain configuration — so a single shared value is safe.
type goAgent struct{}

// NewGoAgent returns the Go coding-agent toolchain.
func NewGoAgent() CodingAgent { return goAgent{} }

func (goAgent) Language() Language { return LangGo }

func (goAgent) BaseImage() string { return "golang:1.23" }

func (goAgent) CacheMounts() []CacheMount {
	return []CacheMount{
		{Path: "/go/pkg/mod", Volume: "go-mod"},
		{Path: "/root/.cache/go-build", Volume: "go-build"},
	}
}

func (goAgent) WarmupExec() []string { return []string{"go", "mod", "download"} }

func (goAgent) TestExec() []string { return []string{"go", "test", "./..."} }

func (goAgent) Persona() string { return "senior Go engineer" }
