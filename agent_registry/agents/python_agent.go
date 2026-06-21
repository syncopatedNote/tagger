package agents

// pythonAgent is the Python toolchain. Unexported; obtained via the registry.
// Stateless, like every agent — pure toolchain configuration.
//
// Warm-up assumes a requirements.txt at the repo root. Projects that pin deps in
// pyproject.toml/poetry/uv instead will simply no-op the warm-up (pip reports
// nothing to install) and the agent installs what it needs during the loop; a
// dedicated poetry/uv agent is a natural follow-up using the same interface.
type pythonAgent struct{}

// NewPythonAgent returns the Python coding-agent toolchain.
func NewPythonAgent() CodingAgent { return pythonAgent{} }

func (pythonAgent) Language() Language { return LangPython }

func (pythonAgent) BaseImage() string { return "python:3.12" }

func (pythonAgent) CacheMounts() []CacheMount {
	return []CacheMount{
		{Path: "/root/.cache/pip", Volume: "pip-cache"},
	}
}

// WarmupExec installs declared dependencies if a requirements.txt is present.
// `|| true` keeps a missing file from failing the warm-up — dependency-less or
// pyproject-based repos proceed without error.
func (pythonAgent) WarmupExec() []string {
	return []string{"sh", "-c", "pip install -r requirements.txt || true"}
}

func (pythonAgent) TestExec() []string { return []string{"pytest"} }

func (pythonAgent) Persona() string { return "senior Python engineer" }
