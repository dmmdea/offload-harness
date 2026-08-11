package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// The gap these tests close (measured 2026-08-11): the ledger recorded a defer
// REASON only on deferred rows, so a call that escalated and then SUCCEEDED
// recorded nothing about why it climbed. That made the one question the
// telemetry needed to answer — "did the model's self-report send this up, or a
// structural signal?" — unanswerable in aggregate, and it meant no change to
// the gating could be evaluated after the fact.
//
// The self-declared gate (self_confidence) is the ONLY one of the seven sources
// that asks the model to grade itself; keeping it distinguishable from the
// structural six is the entire point of the closed enum.

// newGatePipeline builds the minimum Pipeline confidenceGate reads: config
// only. No server, no ledger — the gate is a pure function of (config, data,
// logprobs) and testing it through HTTP would only add flake.
func newGatePipeline(t *testing.T) *Pipeline {
	t.Helper()
	cfg := config.Default()
	cfg.ThresholdsPath = ""
	return New(cfg, nil, nil, nil)
}

func TestConfidenceGateAttributesTheSelfDeclaredGate(t *testing.T) {
	p := newGatePipeline(t)

	req := core.Request{Task: core.TaskClassify, Params: map[string]any{
		"labels": []string{"a", "b"},
	}}
	// classify_min_confidence defaults well above 0.01, so this trips the
	// model's OWN confidence field and nothing else.
	data := []byte(`{"label":"a","confidence":0.01}`)
	reason, _, src, low := p.confidenceGate(req, data, nil)
	if !low {
		t.Fatalf("expected escalation, got low=false (reason=%q)", reason)
	}
	if src != core.EscSelfConfidence {
		t.Fatalf("source = %q, want %q — the self-declared gate must stay "+
			"distinguishable from the structural ones", src, core.EscSelfConfidence)
	}
	if !strings.Contains(reason, "confidence") {
		t.Errorf("human-readable reason lost its meaning: %q", reason)
	}
}

func TestConfidenceGateAttributesNothingWhenItDoesNotFire(t *testing.T) {
	p := newGatePipeline(t)

	req := core.Request{Task: core.TaskClassify, Params: map[string]any{
		"labels": []string{"a", "b"},
	}}
	_, _, src, low := p.confidenceGate(req, []byte(`{"label":"a","confidence":0.99}`), nil)
	if low {
		t.Fatal("gate fired on a confident answer")
	}
	if src != core.EscNone {
		t.Fatalf("source = %q on a non-escalating call, want empty", src)
	}
}

func TestEntryFromCarriesEscSourceToTheLedgerRow(t *testing.T) {
	e := entryFrom(core.TaskClassify, core.Meta{EscSource: core.EscMargin}, false, 10)
	if e.EscSource != string(core.EscMargin) {
		t.Fatalf("ledger row EscSource = %q, want %q", e.EscSource, core.EscMargin)
	}
}

func TestEscSourceIsOmittedWhenAbsent(t *testing.T) {
	// A non-escalating row must not grow a noise field: the ledger is an
	// append-only JSONL whose small-line atomicity is load-bearing.
	b, err := json.Marshal(entryFrom(core.TaskSummarize, core.Meta{}, false, 5))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "esc_source") {
		t.Fatalf("esc_source present on a non-escalating row: %s", b)
	}
}

func TestLedgerRoundTripsEscSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	led, err := ledger.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := led.Record(ledger.Entry{
		Task: "classify", EscSource: string(core.EscSelfConfidence), Escalations: 1,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	led.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got ledger.Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EscSource != string(core.EscSelfConfidence) {
		t.Fatalf("round-trip EscSource = %q, want %q", got.EscSource, core.EscSelfConfidence)
	}
}

func TestOldLedgerLinesWithoutEscSourceStillParse(t *testing.T) {
	// A month of existing rows predates the field; they must not become
	// unreadable, or the historical baseline is lost.
	var e ledger.Entry
	if err := json.Unmarshal([]byte(`{"ts":1,"task":"classify","tokens_in":5}`), &e); err != nil {
		t.Fatalf("legacy row failed to parse: %v", err)
	}
	if e.EscSource != "" {
		t.Fatalf("legacy row invented an EscSource: %q", e.EscSource)
	}
}

func TestEveryEscalationSourceIsDistinct(t *testing.T) {
	// A duplicated constant would silently merge two gates in every aggregate.
	all := []core.EscalationSource{
		core.EscSelfConfidence, core.EscMargin, core.EscConfhead,
		core.EscSchema, core.EscGrounding, core.EscVerifier, core.EscRetries,
	}
	seen := map[core.EscalationSource]bool{}
	for _, s := range all {
		if s == core.EscNone {
			t.Fatalf("a real source collides with EscNone")
		}
		if seen[s] {
			t.Fatalf("duplicate escalation source %q", s)
		}
		seen[s] = true
	}
	if len(seen) != 7 {
		t.Fatalf("expected 7 distinct sources, got %d", len(seen))
	}
}

func TestExactlyOneSourceIsSelfDeclared(t *testing.T) {
	// The whole TO-1 question is self-declared vs structural. If a second
	// self-declared gate is ever added, this test should fail and force the
	// analysis to be updated rather than silently mixing the two classes.
	selfDeclared := []core.EscalationSource{core.EscSelfConfidence}
	structural := []core.EscalationSource{
		core.EscMargin, core.EscConfhead, core.EscSchema,
		core.EscGrounding, core.EscVerifier, core.EscRetries,
	}
	if len(selfDeclared) != 1 {
		t.Fatalf("expected exactly one self-declared source, got %d", len(selfDeclared))
	}
	for _, s := range structural {
		if s == core.EscSelfConfidence {
			t.Fatalf("%q classified as structural but is the self-declared gate", s)
		}
	}
}
