package compeval

// Two runs of the kv bench against one serving endpoint: the proof that a
// rung is a no-op must not need the runs to share a salt, and a repeated salt
// must not poison the second run's calibration controls.

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// warmSeat models a server with a MULTI-PROMPT cache: every prompt it has ever
// served stays restorable, and a request reuses the longest restorable prefix.
type warmSeat struct {
	seen [][]agent.Msg
}

func (w *warmSeat) probe(_ context.Context, msgs []agent.Msg) (ProbeResult, error) {
	total := agent.EstimateTokens(msgs)
	curLen := len(renderWire(msgs))
	cacheN := 0
	for _, prev := range w.seen {
		rel, shared, _ := Relation(prev, msgs)
		n := 0
		switch rel {
		case RelNone:
			n = 0
		case RelTruncation:
			n = total
		case RelExtension:
			if curLen > 0 {
				n = int(float64(total) * float64(shared) / float64(curLen))
			}
		default:
			n = 1
		}
		if n > cacheN {
			cacheN = n
		}
	}
	if cacheN > total {
		cacheN = total
	}
	promptN := total - cacheN
	if promptN < 1 {
		promptN = 1
	}
	w.seen = append(w.seen, append([]agent.Msg(nil), msgs...))
	return ProbeResult{
		CacheN: cacheN, PromptN: promptN,
		PromptMS: float64(promptN) * 0.3, WallMS: float64(promptN)*0.3 + 5,
		UsagePromptTokens: total, UsageCachedTokens: cacheN,
		Measured: true, KVMeasured: true,
	}, nil
}

// Two runs given the same nonce, back to back on a warm seat: the second run's
// negative control must still be a prompt the server has never seen.
func TestASharedNonceDoesNotWarmTheSecondRunsControls(t *testing.T) {
	seat := &warmSeat{}
	entries := benchCorpus(t)
	run := func() KVReport {
		rep, err := RunBench(context.Background(), seat.probe, entries, "h",
			BenchOpts{Rounds: 1, CtxTokens: 8192, MaxOut: 1024, RunNonce: "same", Arms: []string{ArmOn}})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	first, second := run(), run()
	if first.Verdict != "ADMISSIBLE" {
		t.Fatalf("run 1: %s (%s)", first.Verdict, first.InadmissibleReason)
	}
	if second.Verdict != "ADMISSIBLE" {
		t.Fatalf("run 2 on a warm seat: %s (%s)", second.Verdict, second.InadmissibleReason)
	}
}
