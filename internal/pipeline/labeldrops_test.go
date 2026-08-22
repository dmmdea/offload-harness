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
	if got, _ := p.LabelDrops(); got != 0 {
		t.Fatalf("LabelDrops = %d with no labels path configured, want 0", got)
	}

	// With a path set, a task answersAgree does not judge must be COUNTED, not silent.
	p.cfg.ConfHeadLabelsPath = t.TempDir() + "/labels.jsonl"

	// A task answersAgree does not judge is a STRUCTURAL exclusion, not a bias term. It must
	// land in the unjudgeable bucket and must NOT inflate the unparseable count, which is
	// the number published as evidence of upward bias.
	p.labelAgreement(core.TaskSummarize, ledger.Entry{}, "anything", core.Result{}, 10)
	unparseable, unjudgeable := p.LabelDrops()
	if unjudgeable != 1 {
		t.Fatalf("unjudgeableTask = %d after a summarize row, want 1", unjudgeable)
	}
	if unparseable != 0 {
		t.Fatalf("unparseable = %d, want 0 — a task-type exclusion is not a bias term", unparseable)
	}

	// A classify row whose candidate cannot be parsed IS the bias term.
	p.labelAgreement(core.TaskClassify, ledger.Entry{}, "not json at all", core.Result{}, 10)
	unparseable, unjudgeable = p.LabelDrops()
	if unparseable != 1 {
		t.Fatalf("unparseable = %d after an unjudgeable classify candidate, want 1", unparseable)
	}
	if unjudgeable != 1 {
		t.Fatalf("unjudgeableTask = %d, want 1 — it must not have moved", unjudgeable)
	}
}
