package gpuactivity

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The registry carries JOB liveness beside the holder heartbeat (0.131.0):
// OnProgress/OnAllowance write the last progress, the decode rate and the
// stall bound, and Liveness() renders one line every status reader shares.
func TestHandleOnProgressAndLivenessLine(t *testing.T) {
	reg := OpenAt(t.TempDir())
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	clock := base
	reg.now = func() time.Time { return clock }
	h, err := reg.Begin(Run{PID: os.Getpid(), Seat: "seat", Kind: "contract", Phase: PhaseAdmission})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()

	h.OnAllowance("prefill", 214*time.Second)
	h.OnProgress(10)
	clock = base.Add(2 * time.Second)
	h.OnProgress(30) // 20 tokens in 2 s = 10 tok/s
	runs := reg.List(clock)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	r := runs[0]
	if r.TokensOut != 30 || r.LastProgressMs != clock.UnixMilli() || r.TokS < 9.9 || r.TokS > 10.1 || r.LivePhase != "decoding" || r.AllowanceMs != 214000 {
		t.Fatalf("run = %+v", r)
	}
	if r.Phase != PhaseRunning {
		t.Fatalf("a run that is producing is not still admitting: phase = %s", r.Phase)
	}
	line := r.Liveness(clock.Add(2 * time.Second))
	if !strings.HasPrefix(line, "producing 10.0 tok/s, last token 2s ago (allowed 214s in decoding)") {
		t.Fatalf("liveness line = %q", line)
	}
	if !strings.Contains(r.Summary(clock), "producing 10.0 tok/s") {
		t.Fatalf("summary must carry the liveness line: %q", r.Summary(clock))
	}

	silent := Run{LivePhase: "prefill", AllowanceMs: 214000, LastProgressMs: base.UnixMilli(), TokS: 3}
	if got := silent.Liveness(base.Add(187 * time.Second)); got != "silent 187s of 214s allowed in prefill" {
		t.Fatalf("silent line = %q", got)
	}
	if got := (Run{}).Liveness(base); got != "" {
		t.Fatalf("a run that never reported progress renders nothing, got %q", got)
	}
}

func TestNilHandleProgressIsSafe(t *testing.T) {
	var h *Handle
	h.OnProgress(3)
	h.OnAllowance("tool", time.Second) // no panic: Start returns nil when the registry cannot open
}
