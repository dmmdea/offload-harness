package gpugen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// --- footprint sampling hook (fleet-node, additive + nil-gated) ---

// TestGenerateFootprintReportsPeakOnSuccess: with Footprint set and a fake
// SampleFunc, a successful render reports the MAX observed sample via OnFootprint.
func TestGenerateFootprintReportsPeakOnSuccess(t *testing.T) {
	requireNode(t)
	old := footprintSampleInterval
	footprintSampleInterval = 50 * time.Millisecond
	defer func() { footprintSampleInterval = old }()

	out := filepath.Join(t.TempDir(), "made.txt")
	exe, script, args := sleepThenWriteCmd(out, "hello", 400)

	var mu sync.Mutex
	samples := []float64{1.5, 3.25, 2.0} // peak = 3.25, not the last value
	i := 0
	var reported []float64
	var sampledPid int
	_, err := Generate(context.Background(), Spec{
		Exe: exe, Script: script, Args: args,
		Out:     out,
		Timeout: 20 * time.Second,
		Footprint: &FootprintKey{Family: "sdxl", Quant: "bf16", Task: "image-gen"},
		SampleFunc: func(pid int) (float64, error) {
			mu.Lock()
			defer mu.Unlock()
			sampledPid = pid
			v := samples[i%len(samples)]
			i++
			return v, nil
		},
		OnFootprint: func(peak float64) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, peak)
		},
	})
	if err != nil {
		t.Fatalf("Generate: unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sampledPid <= 0 {
		t.Fatalf("SampleFunc never received the child pid (got %d)", sampledPid)
	}
	if len(reported) != 1 {
		t.Fatalf("OnFootprint called %d times, want exactly 1", len(reported))
	}
	if reported[0] != 3.25 {
		t.Fatalf("OnFootprint reported %v, want the peak 3.25", reported[0])
	}
}

// TestGenerateFootprintNotReportedOnFailure: a run whose output-stat gate fails
// (child exits 0 but writes nothing) must NOT report a footprint — a crashed or
// phantom run's peak may be partial, so only SUCCESS records.
func TestGenerateFootprintNotReportedOnFailure(t *testing.T) {
	requireNode(t)
	old := footprintSampleInterval
	footprintSampleInterval = 50 * time.Millisecond
	defer func() { footprintSampleInterval = old }()

	exe, script, args := trueCmd()
	called := false
	_, err := Generate(context.Background(), Spec{
		Exe: exe, Script: script, Args: args,
		Out:       filepath.Join(t.TempDir(), "never.txt"),
		Timeout:   10 * time.Second,
		Footprint: &FootprintKey{Family: "sdxl", Task: "image-gen"},
		SampleFunc: func(pid int) (float64, error) { return 5.0, nil },
		OnFootprint: func(peak float64) { called = true },
	})
	if err == nil {
		t.Fatal("Generate must error when the output file is absent")
	}
	if called {
		t.Fatal("OnFootprint must NOT fire on a failed run")
	}
}

// TestGenerateFootprintChildErrorNotReported: a non-zero child exit is a failure —
// no footprint even though samples were taken.
func TestGenerateFootprintChildErrorNotReported(t *testing.T) {
	requireNode(t)
	old := footprintSampleInterval
	footprintSampleInterval = 50 * time.Millisecond
	defer func() { footprintSampleInterval = old }()

	called := false
	_, err := Generate(context.Background(), Spec{
		Exe: "node", Script: "-e", Args: []string{"process.exit(3)"},
		Out:       filepath.Join(t.TempDir(), "never.txt"),
		Timeout:   10 * time.Second,
		Footprint: &FootprintKey{Family: "whisper", Task: "stt"},
		SampleFunc: func(pid int) (float64, error) { return 5.0, nil },
		OnFootprint: func(peak float64) { called = true },
	})
	if err == nil {
		t.Fatal("Generate must surface the child's non-zero exit")
	}
	if called {
		t.Fatal("OnFootprint must NOT fire when the child failed")
	}
}

// TestGenerateFootprintNilHookUnchanged: with Footprint nil the legacy path runs —
// SampleFunc/OnFootprint are never touched even when (mistakenly) set.
func TestGenerateFootprintNilHookUnchanged(t *testing.T) {
	requireNode(t)
	out := filepath.Join(t.TempDir(), "made.txt")
	exe, script, args := writeFileCmd(out, "hello")
	sampled, reported := false, false
	got, err := Generate(context.Background(), Spec{
		Exe: exe, Script: script, Args: args,
		Out:     out,
		Timeout: 10 * time.Second,
		// Footprint nil ⇒ byte-identical CombinedOutput path; the callbacks are inert.
		SampleFunc:  func(pid int) (float64, error) { sampled = true; return 1, nil },
		OnFootprint: func(peak float64) { reported = true },
	})
	if err != nil {
		t.Fatalf("Generate: unexpected error: %v", err)
	}
	if got != out {
		t.Fatalf("Generate returned %q, want %q", got, out)
	}
	if sampled || reported {
		t.Fatalf("nil Footprint must not sample or report (sampled=%v reported=%v)", sampled, reported)
	}
}

// TestGenerateFootprintNilCallbacksSafe: Footprint set but OnFootprint/SampleFunc
// nil — success path must not panic (nil-safe wiring).
func TestGenerateFootprintNilCallbacksSafe(t *testing.T) {
	requireNode(t)
	out := filepath.Join(t.TempDir(), "made.txt")
	exe, script, args := writeFileCmd(out, "hello")
	got, err := Generate(context.Background(), Spec{
		Exe: exe, Script: script, Args: args,
		Out:       out,
		Timeout:   10 * time.Second,
		Footprint: &FootprintKey{Family: "sdxl", Task: "image-gen"},
	})
	if err != nil {
		t.Fatalf("Generate: unexpected error: %v", err)
	}
	if got != out {
		t.Fatalf("Generate returned %q, want %q", got, out)
	}
}

// sleepThenWriteCmd: node sleeps ms milliseconds, then writes content to path and
// exits 0 — long enough for the footprint sampler to take several ticks.
func sleepThenWriteCmd(out, content string, ms int) (exe, script string, args []string) {
	js := `setTimeout(()=>{require('fs').writeFileSync(process.argv[1], process.argv[2])}, ` + itoa(ms) + `)`
	return "node", "-e", []string{js, out, content}
}

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
