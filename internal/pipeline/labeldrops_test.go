package pipeline

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// The whole point of the counter: a label corpus that cannot state its own coverage
// invites its rate to be read as total. Drops skew toward EXTREME disagreement
// (unparseable candidates), so silent dropping biases the published rate upward.
func TestLabelAgreementCountsUnjudgeableCandidates(t *testing.T) {
	p := &Pipeline{}
	p.cfg.ConfHeadLabelsPath = "" // no sidecar: exercise only the judge path

	// With no labels path the function returns before judging, so the counter must stay 0 —
	// otherwise it would count configuration, not drops.
	p.labelAgreement(core.TaskClassify, ledger.Entry{}, "candidate", core.Result{}, 10)
	if got := p.LabelDrops(); got != 0 {
		t.Fatalf("LabelDrops = %d with no labels path configured, want 0", got)
	}

	// With a path set, a task answersAgree does not judge must be COUNTED, not silent.
	p.cfg.ConfHeadLabelsPath = t.TempDir() + "/labels.jsonl"
	p.labelAgreement(core.TaskSummarize, ledger.Entry{}, "anything", core.Result{}, 10)
	if got := p.LabelDrops(); got != 1 {
		t.Fatalf("LabelDrops = %d after an unjudgeable task, want 1", got)
	}

	// And it must accumulate rather than latch at one.
	p.labelAgreement(core.TaskOCR, ledger.Entry{}, "anything", core.Result{}, 10)
	if got := p.LabelDrops(); got != 2 {
		t.Fatalf("LabelDrops = %d, want 2 — the counter latched instead of accumulating", got)
	}
}
