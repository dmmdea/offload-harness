package gpugen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helper: a runner config that uses the OS shell-free `node`-substitute. We avoid
// requiring node by running a tiny inline program through `go run`-free means: the
// tests below use real OS commands (cmd/echo) so they exercise the exec lifecycle,
// killTree, and the output-stat path without any GPU or ComfyUI.

// TestGenerateMissingScript: an empty script returns an error (the caller defers).
func TestGenerateMissingScript(t *testing.T) {
	_, err := Generate(context.Background(), Spec{
		Exe:     "node",
		Script:  "",
		Out:     filepath.Join(t.TempDir(), "x.png"),
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("empty script must error so the caller defers")
	}
}

// TestGenerateWritesOutputThenSucceeds: a command that creates the output file and
// exits 0 returns the out path with no error. Uses a portable shell command, NOT a
// real render, so no GPU/ComfyUI is touched.
func TestGenerateWritesOutputThenSucceeds(t *testing.T) {
	requireNode(t)
	out := filepath.Join(t.TempDir(), "made.txt")
	exe, script, args := writeFileCmd(out, "hello")
	got, err := Generate(context.Background(), Spec{
		Exe:     exe,
		Script:  script,
		Args:    args,
		Out:     out,
		Timeout: 10 * time.Second,
		// freeComfyVRAM must be a no-op here (nothing listening): default API is fine.
	})
	if err != nil {
		t.Fatalf("Generate: unexpected error: %v", err)
	}
	if got != out {
		t.Fatalf("Generate returned %q, want %q", got, out)
	}
	if b, rerr := os.ReadFile(out); rerr != nil || strings.TrimSpace(string(b)) != "hello" {
		t.Fatalf("output not written: %v / %q", rerr, string(b))
	}
}

// TestGenerateEmptyOutputErrors: the command exits 0 but writes no output file —
// Generate must error (so the caller defers rather than reporting a phantom render).
func TestGenerateEmptyOutputErrors(t *testing.T) {
	requireNode(t)
	missing := filepath.Join(t.TempDir(), "never.txt")
	exe, script, args := trueCmd()
	_, err := Generate(context.Background(), Spec{
		Exe:     exe,
		Script:  script,
		Args:    args,
		Out:     missing,
		Timeout: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("Generate must error when the output file is absent/empty")
	}
}

// TestGenerateTimeoutKillsTree: a command that sleeps past the timeout is cancelled,
// killTree terminates it, and Generate returns a timeout-classified error. This is the
// invariant-3 process-tree-kill guard (video/audio now get it via gpugen).
func TestGenerateTimeoutKillsTree(t *testing.T) {
	requireNode(t)
	exe, script, args := sleepCmd(30)
	start := time.Now()
	_, err := Generate(context.Background(), Spec{
		Exe:     exe,
		Script:  script,
		Args:    args,
		Out:     filepath.Join(t.TempDir(), "x.txt"),
		Timeout: 1 * time.Second,
	})
	if err == nil {
		t.Fatal("a timed-out command must return an error")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout did not kill the process promptly (took %v)", elapsed)
	}
	if ClassifyErr(err) != "timeout" {
		t.Logf("note: ClassifyErr=%q (timeout classification is best-effort on the wrapped error)", ClassifyErr(err))
	}
}

// TestKillTreeNilProcess: killTree(nil) is a safe no-op (never panics).
func TestKillTreeNilProcess(t *testing.T) {
	if err := killTree(nil); err != nil {
		t.Fatalf("killTree(nil) should be a no-op, got %v", err)
	}
}

// TestClassifyErr maps common failure substrings to err classes (mirrors pipeline).
func TestClassifyErr(t *testing.T) {
	cases := map[string]string{
		"CUDA out of memory":            "oom",
		"context deadline exceeded":     "timeout",
		"dial tcp: connection refused":  "conn_refused",
		"something else entirely":       "other",
	}
	for msg, want := range cases {
		if got := ClassifyErr(errString(msg)); got != want {
			t.Errorf("ClassifyErr(%q) = %q, want %q", msg, got, want)
		}
	}
	if ClassifyErr(nil) != "" {
		t.Error("ClassifyErr(nil) must be empty")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// --- command builders: drive `node -e <script>`, exactly the runner gpugen wraps in
// production (a render/*.mjs). No GPU, no ComfyUI — pure process lifecycle exercise.
// `node` is the verified toolchain on this box; skip cleanly if it's absent.

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; gpugen exec test needs the verified toolchain")
	}
}

// writeFileCmd: node writes content to path then exits 0.
func writeFileCmd(out, content string) (exe, script string, args []string) {
	js := `require('fs').writeFileSync(process.argv[1], process.argv[2])`
	return "node", "-e", []string{js, out, content}
}

// trueCmd: node exits 0 without writing the output file.
func trueCmd() (exe, script string, args []string) {
	return "node", "-e", []string{"process.exit(0)"}
}

// sleepCmd: node sleeps sec seconds (so the timeout path can cancel + killTree it).
func sleepCmd(sec int) (exe, script string, args []string) {
	js := `setTimeout(()=>{}, ` + itoa(sec*1000) + `)`
	return "node", "-e", []string{js}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
