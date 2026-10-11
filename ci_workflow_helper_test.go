package main

// Shared plumbing for the tests that read .github/workflows/ci.yml: H-26's govulncheck job, D-98's
// Windows cage step, and whatever edits the workflow next.
//
// Why they read the workflow instead of trusting it. A CI job that is deleted, indented into a no-op,
// moved to the wrong runner, or left running `go test` where the test only SKIPS is green by
// construction, and nothing else in the repository notices: the Windows cage's environment proof
// existed for months and ran only on a developer's own Windows machine, because the one job on a
// Windows runner never ran `go test`. So each rule here is checked twice: the yaml says the right
// thing (structure), and the step's own script, run under bash with stand-ins for the commands it
// calls, does the right thing (behaviour). The second half is the one that catches a script that
// reads fine and exits 0 when it should not.
//
// The runner starts a `shell: bash` step as `bash --noprofile --norc -e -o pipefail <script file>`
// (github.com/actions/runner, ScriptHandlerHelpers.cs); ciScriptSandbox does the same, so `set -e`
// and pipefail behave here as they do there.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// ciWorkflow is the part of a GitHub Actions workflow file these tests read.
type ciWorkflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]ciJob  `yaml:"jobs"`
}

type ciJob struct {
	RunsOn any               `yaml:"runs-on"`
	Env    map[string]string `yaml:"env"`
	Steps  []ciStep          `yaml:"steps"`
}

type ciStep struct {
	Name  string            `yaml:"name"`
	ID    string            `yaml:"id"`
	Uses  string            `yaml:"uses"`
	Run   string            `yaml:"run"`
	Shell string            `yaml:"shell"`
	If    string            `yaml:"if"`
	Env   map[string]string `yaml:"env"`
	With  map[string]any    `yaml:"with"`
}

// runner is the job's runs-on label when it is a single string, and "" for a list or a matrix
// expression: a rule that names a runner label wants the exact standard one, so anything cleverer
// reads as a mismatch and the test says so.
func (j ciJob) runner() string {
	s, _ := j.RunsOn.(string)
	return s
}

// label is how a step is named in a failure message.
func (s ciStep) label() string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Uses != "":
		return s.Uses
	default:
		return "(unnamed step)"
	}
}

// jobNames is the workflow's job names in a fixed order, so a failure message and a search do not
// depend on map iteration.
func (w ciWorkflow) jobNames() []string {
	names := make([]string, 0, len(w.Jobs))
	for name := range w.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ciRepoRoot is the module root: the nearest directory at or above the working directory that holds a
// go.mod. Under `go test` the working directory is the package directory, which is the root for these
// tests; the walk costs nothing there and keeps the helper usable from a sub-package.
func ciRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for dir := cwd; ; {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod at or above %s", cwd)
		}
		dir = parent
	}
}

// loadCIWorkflow reads and parses .github/workflows/ci.yml.
func loadCIWorkflow(t *testing.T) ciWorkflow {
	t.Helper()
	path := filepath.Join(ciRepoRoot(t), ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the CI workflow: %v", err)
	}
	var w ciWorkflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(w.Jobs) == 0 {
		t.Fatalf("%s declares no jobs", path)
	}
	return w
}

// ciCall is one call a stand-in command recorded.
type ciCall struct {
	Tool      string
	Dir       string // the module directory the call ran in: ".", "tools/comfyui" or "tools/llamaswap"
	Goos      string // GOOS as the call saw it, "<unset>" when it was not exported
	Toolchain string // GOTOOLCHAIN as the call saw it, "<unset>" when it was not exported
	Args      []string
}

// hasArgs reports whether the call's arguments contain want as one consecutive run.
func (c ciCall) hasArgs(want ...string) bool {
	for i := 0; i+len(want) <= len(c.Args); i++ {
		if strings.Join(c.Args[i:i+len(want)], "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}

// ciScriptSandbox runs a workflow step's `run:` text the way the runner does for `shell: bash`, in a
// scratch directory, with stand-ins for the commands the step calls and files standing in for
// GITHUB_ENV and GITHUB_PATH. It skips the test when this host has no usable bash (guardBash).
type ciScriptSandbox struct {
	t        *testing.T
	bash     string
	dir      string // the step's working directory (the repository root, in CI)
	bin      string // the stand-ins, first on PATH
	scripts  string // where the step's script is written
	log      string // the stand-ins append the calls they receive here
	envFile  string // GITHUB_ENV
	pathFile string // GITHUB_PATH
}

func newCIScriptSandbox(t *testing.T) *ciScriptSandbox {
	t.Helper()
	root := t.TempDir()
	s := &ciScriptSandbox{
		t:        t,
		bash:     guardBash(t),
		dir:      filepath.Join(root, "work"),
		bin:      filepath.Join(root, "bin"),
		scripts:  filepath.Join(root, "scripts"),
		log:      filepath.Join(root, "calls.log"),
		envFile:  filepath.Join(root, "github_env"),
		pathFile: filepath.Join(root, "github_path"),
	}
	for _, d := range []string{s.dir, s.bin, s.scripts} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("scratch directory: %v", err)
		}
	}
	for _, f := range []string{s.log, s.envFile, s.pathFile} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatalf("scratch file: %v", err)
		}
	}
	return s
}

// ciStandInPrelude is the part every stand-in shares: it writes the call down. The module directory is
// read from the end of $PWD, because the shell's idea of a path and this process's idea of the same path
// differ under Git Bash on Windows, and the scratch tree holds exactly these three directories.
const ciStandInPrelude = `#!/usr/bin/env bash
record() {
  case "$PWD" in
    */tools/comfyui) module=tools/comfyui ;;
    */tools/llamaswap) module=tools/llamaswap ;;
    *) module=. ;;
  esac
  tool="$1"
  shift
  {
    echo "call $tool"
    echo "dir $module"
    echo "goos ${GOOS-<unset>}"
    echo "toolchain ${GOTOOLCHAIN-<unset>}"
    for a in "$@"; do echo "arg $a"; done
    echo "end"
  } >> "$FAKE_LOG"
}
`

// standIn writes an executable command named name into the sandbox's bin directory. body follows the
// shared prelude; it can call `record <name> "$@"` and read $module after that.
func (s *ciScriptSandbox) standIn(name, body string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, name), []byte(ciStandInPrelude+body), 0o755); err != nil {
		s.t.Fatalf("write the stand-in %s: %v", name, err)
	}
}

// writeFile puts a file in the step's working directory.
func (s *ciScriptSandbox) writeFile(rel, content string) {
	s.t.Helper()
	path := filepath.Join(s.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatalf("create the directory of %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatalf("write %s: %v", rel, err)
	}
}

// file is a path under the scratch tree in the form bash takes on every host: forward slashes.
func (s *ciScriptSandbox) file(name, content string) string {
	s.t.Helper()
	path := filepath.Join(s.scripts, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatalf("write %s: %v", name, err)
	}
	return filepath.ToSlash(path)
}

// environ is the environment of a step: the host's, minus everything that would let a variable of the
// developer's own shell (a GITHUB_* token, a GOTOOLCHAIN override) change the outcome, plus the
// runner's file commands and the stand-ins on PATH. extra wins over all of it.
func (s *ciScriptSandbox) environ(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		k := strings.ToUpper(key)
		if strings.HasPrefix(k, "GITHUB_") || strings.HasPrefix(k, "RUNNER_") || strings.HasPrefix(k, "FAKE_") ||
			k == "GOTOOLCHAIN" || k == "GOOS" || k == "GOARCH" || k == "GOVULNDB" || k == "GOFLAGS" || k == "CI" {
			continue
		}
		env = append(env, kv)
	}
	env = withBashOnPath(env, s.bash)
	env = prependPath(env, s.bin)
	env = append(env,
		"GITHUB_ENV="+filepath.ToSlash(s.envFile),
		"GITHUB_PATH="+filepath.ToSlash(s.pathFile),
		"FAKE_LOG="+filepath.ToSlash(s.log),
	)
	return append(env, extra...)
}

// prependPath puts dir first on the PATH of env.
func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	cur := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			cur = v
			continue
		}
		out = append(out, kv)
	}
	if cur == "" {
		return append(out, "PATH="+dir)
	}
	return append(out, "PATH="+dir+string(os.PathListSeparator)+cur)
}

// run executes script as a `shell: bash` step in the working directory, with the options the runner adds
// (-e and pipefail), and returns its combined output and exit code.
func (s *ciScriptSandbox) run(script string, extraEnv ...string) (string, int) {
	s.t.Helper()
	return s.runWith([]string{"--noprofile", "--norc", "-e", "-o", "pipefail"}, script, extraEnv...)
}

// runPlain executes script under a bare bash, none of the runner's options. A step's script must not lean on
// them: a custom `shell:` template, or a runner that changes its defaults, drops them without a word.
func (s *ciScriptSandbox) runPlain(script string, extraEnv ...string) (string, int) {
	s.t.Helper()
	return s.runWith([]string{"--noprofile", "--norc"}, script, extraEnv...)
}

func (s *ciScriptSandbox) runWith(flags []string, script string, extraEnv ...string) (string, int) {
	s.t.Helper()
	file := s.file("step.sh", strings.ReplaceAll(script, "\r\n", "\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bash, append(flags, file)...)
	cmd.Dir = s.dir
	cmd.Env = s.environ(extraEnv)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	default:
		s.t.Fatalf("run the step under %s: %v\n%s", s.bash, err, out)
		return "", -1
	}
}

// calls parses the stand-ins' log, in call order. tool "" returns every call.
func (s *ciScriptSandbox) calls(tool string) []ciCall {
	s.t.Helper()
	raw, err := os.ReadFile(s.log)
	if err != nil {
		s.t.Fatalf("read the stand-ins' log: %v", err)
	}
	var out []ciCall
	var cur *ciCall
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "call":
			cur = &ciCall{Tool: val}
		case "dir":
			if cur != nil {
				cur.Dir = val
			}
		case "goos":
			if cur != nil {
				cur.Goos = val
			}
		case "toolchain":
			if cur != nil {
				cur.Toolchain = val
			}
		case "arg":
			if cur != nil {
				cur.Args = append(cur.Args, val)
			}
		case "end":
			if cur != nil && (tool == "" || cur.Tool == tool) {
				out = append(out, *cur)
			}
			cur = nil
		}
	}
	return out
}

// githubEnv is what the step appended to $GITHUB_ENV, byte for byte: a carriage return that leaked from a
// CRLF go.mod into the value must show.
func (s *ciScriptSandbox) githubEnv() string {
	s.t.Helper()
	raw, err := os.ReadFile(s.envFile)
	if err != nil {
		s.t.Fatalf("read GITHUB_ENV: %v", err)
	}
	return string(raw)
}

// githubPath is what the step appended to $GITHUB_PATH, byte for byte.
func (s *ciScriptSandbox) githubPath() string {
	s.t.Helper()
	raw, err := os.ReadFile(s.pathFile)
	if err != nil {
		s.t.Fatalf("read GITHUB_PATH: %v", err)
	}
	return string(raw)
}
