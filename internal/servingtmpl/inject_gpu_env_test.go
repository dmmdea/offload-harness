package servingtmpl

import (
	"regexp"
	"strings"
	"testing"
)

// TestInjectGPUEnvAddsAndExtendsEnvOnce carries the assertions the PowerShell suite made of
// install.ps1's Add-GpuEnvToYaml before the render moved here (injectGPUEnv is its port): a model
// without an env gains one right after its key, a model with one keeps its own entries first and
// gains the new ones, each model block is touched once, other sections are left alone, and a second
// pass changes nothing.
func TestInjectGPUEnvAddsAndExtendsEnvOnce(t *testing.T) {
	const yaml = `healthCheckTimeout: 300

macros:
  common: >-
    --ctx-size 32768 --port ${PORT}

models:
  offload-e4b:
    aliases: [gemma4-e4b]
    cmd: >-
      C:/x/llama/llama-server.exe -m C:/x/models/e4b.gguf
      -ngl 99 ${common}
    ttl: 300
  gemma4-26b-a4b:
    env: [GGML_CUDA_DISABLE_GRAPHS=1]
    cmd: >-
      C:/x/llama/llama-server.exe -m C:/x/models/26b.gguf
      -ngl 99 ${common}
    ttl: 300

groups:
  offload-family:
    swap: true
    members: [offload-e4b, gemma4-26b-a4b]
`
	vars := []string{"CUDA_VISIBLE_DEVICES=0", "CUDA_MODULE_LOADING=LAZY"}
	out := injectGPUEnv(yaml, vars)
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")

	e4b := -1
	for i, l := range lines {
		if regexp.MustCompile(`^\s{2}offload-e4b:`).MatchString(l) {
			e4b = i
			break
		}
	}
	if e4b < 0 || e4b+1 >= len(lines) {
		t.Fatalf("offload-e4b key not found in:\n%s", out)
	}
	if !regexp.MustCompile(`^\s{4}env: \[CUDA_VISIBLE_DEVICES=0, CUDA_MODULE_LOADING=LAZY\]$`).MatchString(lines[e4b+1]) {
		t.Errorf("e4b must gain an env line right after its key, got %q", lines[e4b+1])
	}
	if !regexp.MustCompile(`(?m)^\s{4}env: \[GGML_CUDA_DISABLE_GRAPHS=1, CUDA_VISIBLE_DEVICES=0, CUDA_MODULE_LOADING=LAZY\]$`).MatchString(out) {
		t.Errorf("the 26B env list must be extended with its own flag kept first:\n%s", out)
	}
	if n := strings.Count(out, "CUDA_VISIBLE_DEVICES=0"); n != 2 {
		t.Errorf("want exactly one injection per model block (2), got %d", n)
	}
	if groups := out[strings.Index(out, "\ngroups:"):]; strings.Contains(groups, "CUDA_VISIBLE_DEVICES") {
		t.Errorf("the groups section must be untouched, got:\n%s", groups)
	}
	if !strings.Contains(out, "macros:\n  common: >-\n    --ctx-size 32768 --port ${PORT}\n") {
		t.Errorf("the macros section must be untouched:\n%s", out)
	}
	if twice := injectGPUEnv(out, vars); twice != out {
		t.Errorf("a second application must be a no-op; it changed the text to:\n%s", twice)
	}
}

// TestInjectGPUEnvKeepsAKeyTheBlockAlreadySets: a seat pinned to its own device keeps that pin when
// the tier-wide env names the same key, and gains only the keys it lacks.
func TestInjectGPUEnvKeepsAKeyTheBlockAlreadySets(t *testing.T) {
	const yaml = "models:\n  seat-x:\n    env: [CUDA_VISIBLE_DEVICES=2]\n    cmd: run\n"
	out := injectGPUEnv(yaml, []string{"CUDA_VISIBLE_DEVICES=0", "CUDA_MODULE_LOADING=LAZY"})
	if want := "    env: [CUDA_VISIBLE_DEVICES=2, CUDA_MODULE_LOADING=LAZY]"; !strings.Contains(out, want+"\n") {
		t.Errorf("want %q, got:\n%s", want, out)
	}
	if strings.Contains(out, "CUDA_VISIBLE_DEVICES=0") {
		t.Errorf("the tier-wide value must not override the seat's own pin:\n%s", out)
	}
}
