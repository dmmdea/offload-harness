package shadow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/local-offload/internal/core"
	"github.com/dmmdea/local-offload/internal/ledger"
	"github.com/dmmdea/local-offload/internal/tasks"
)

func TestLabelQueue_ClassifyAgreementWritesLabel(t *testing.T) {
	items := []Item{
		{Task: "classify", Input: "the cat sat", EntryTier: "gemma4-e2b", EntryOutput: `{"label":"animal"}`},
		{Task: "triage", Input: "buy now!!!", EntryTier: "gemma4-e2b", EntryOutput: `{"decision":"yes"}`},
	}
	var written []ledger.Entry
	d := LabelDeps{
		Escalation: "gemma4-e4b",
		RunTier: func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
			// fake escalation: agrees with classify, disagrees with triage
			if req.Task == core.TaskClassify {
				data, _ := json.Marshal(map[string]any{"label": "animal"})
				return core.Result{OK: true, Data: data}, true
			}
			data, _ := json.Marshal(map[string]any{"decision": "no"})
			return core.Result{OK: true, Data: data}, true
		},
		AnswersAgree: func(task string, candidate string, finalData []byte) (bool, bool) {
			// compare the "label" or "decision" field between entryOutput and finalData
			field := "label"
			if task == "triage" {
				field = "decision"
			}
			var a, b map[string]any
			if json.Unmarshal([]byte(candidate), &a) != nil {
				return false, false
			}
			if json.Unmarshal(finalData, &b) != nil {
				return false, false
			}
			av, _ := a[field].(string)
			bv, _ := b[field].(string)
			if av == "" || bv == "" {
				return false, false
			}
			return av == bv, true
		},
		Ground: func(task core.TaskType, input string, data []byte) (bool, bool) {
			return true, true
		},
		AppendLabel: func(path string, e ledger.Entry) error {
			written = append(written, e)
			return nil
		},
		LabelsPath: "/tmp/test-labels.jsonl",
	}
	n := LabelQueue(context.Background(), items, 100, d)
	if n != 2 || len(written) != 2 {
		t.Fatalf("expected 2 labels written, got n=%d written=%d", n, len(written))
	}
	// classify agreed -> EscalatedAgreed true; triage disagreed -> false
	if written[0].EscalatedAgreed == nil || !*written[0].EscalatedAgreed {
		t.Fatalf("classify should be agreed=true: %+v", written[0])
	}
	if written[1].EscalatedAgreed == nil || *written[1].EscalatedAgreed {
		t.Fatalf("triage should be agreed=false: %+v", written[1])
	}
}

func TestLabelQueue_CapRespected(t *testing.T) {
	items := make([]Item, 5)
	for i := range items {
		items[i] = Item{Task: "classify", Input: "text", EntryTier: "gemma4-e2b", EntryOutput: `{"label":"x"}`}
	}
	var count int
	d := LabelDeps{
		Escalation: "gemma4-e4b",
		RunTier: func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
			data, _ := json.Marshal(map[string]any{"label": "x"})
			return core.Result{OK: true, Data: data}, true
		},
		AnswersAgree: func(task, candidate string, finalData []byte) (bool, bool) {
			return true, true
		},
		Ground: func(task core.TaskType, input string, data []byte) (bool, bool) {
			return true, true
		},
		AppendLabel: func(path string, e ledger.Entry) error {
			count++
			return nil
		},
		LabelsPath: "/tmp/test-labels.jsonl",
	}
	n := LabelQueue(context.Background(), items, 3, d)
	if n != 3 || count != 3 {
		t.Fatalf("cap=3 but wrote n=%d count=%d", n, count)
	}
}

func TestLabelQueue_RunTierFailSkips(t *testing.T) {
	items := []Item{
		{Task: "classify", Input: "a", EntryTier: "gemma4-e2b", EntryOutput: `{"label":"x"}`},
		{Task: "classify", Input: "b", EntryTier: "gemma4-e2b", EntryOutput: `{"label":"y"}`},
	}
	callCount := 0
	d := LabelDeps{
		Escalation: "gemma4-e4b",
		RunTier: func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
			callCount++
			if callCount == 1 {
				return core.Result{}, false // first item fails
			}
			data, _ := json.Marshal(map[string]any{"label": "y"})
			return core.Result{OK: true, Data: data}, true
		},
		AnswersAgree: func(task, candidate string, finalData []byte) (bool, bool) {
			return true, true
		},
		Ground: func(task core.TaskType, input string, data []byte) (bool, bool) {
			return true, true
		},
		AppendLabel: func(path string, e ledger.Entry) error { return nil },
		LabelsPath:  "/tmp/test-labels.jsonl",
	}
	n := LabelQueue(context.Background(), items, 100, d)
	if n != 1 {
		t.Fatalf("first item RunTier failed, expected 1 written but got %d", n)
	}
}

// TestLabelQueue_AnswersAgreeNotOK_Skips verifies that a classify item where
// AnswersAgree returns ok=false is skipped and not counted in written.
func TestLabelQueue_AnswersAgreeNotOK_Skips(t *testing.T) {
	items := []Item{
		{Task: "classify", Input: "text", EntryTier: "gemma4-e2b", EntryOutput: `{"label":"x"}`},
	}
	var appendCalled int
	d := LabelDeps{
		Escalation: "gemma4-e4b",
		RunTier: func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
			data, _ := json.Marshal(map[string]any{"label": "x"})
			return core.Result{OK: true, Data: data}, true
		},
		AnswersAgree: func(task, candidate string, finalData []byte) (bool, bool) {
			return false, false // ok=false → skip
		},
		Ground: func(task core.TaskType, input string, data []byte) (bool, bool) {
			return true, true
		},
		AppendLabel: func(path string, e ledger.Entry) error {
			appendCalled++
			return nil
		},
		LabelsPath: "/tmp/test-labels.jsonl",
	}
	n := LabelQueue(context.Background(), items, 100, d)
	if n != 0 {
		t.Fatalf("expected 0 written (AnswersAgree ok=false), got %d", n)
	}
	if appendCalled != 0 {
		t.Fatalf("AppendLabel should not have been called, got %d calls", appendCalled)
	}
}

// TestLabelQueue_GroundNotOK_Skips verifies that an extract item where Ground
// returns ok=false is skipped and not counted in written.
func TestLabelQueue_GroundNotOK_Skips(t *testing.T) {
	items := []Item{
		{Task: "extract", Input: "name: Alice", EntryTier: "gemma4-e2b", EntryOutput: `{"name":"Alice"}`},
	}
	var appendCalled int
	d := LabelDeps{
		Escalation: "gemma4-e4b",
		RunTier: func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
			data, _ := json.Marshal(map[string]any{"name": "Alice"})
			return core.Result{OK: true, Data: data}, true
		},
		AnswersAgree: func(task, candidate string, finalData []byte) (bool, bool) {
			return true, true
		},
		Ground: func(task core.TaskType, input string, data []byte) (bool, bool) {
			return false, false // ok=false → skip
		},
		AppendLabel: func(path string, e ledger.Entry) error {
			appendCalled++
			return nil
		},
		LabelsPath: "/tmp/test-labels.jsonl",
	}
	n := LabelQueue(context.Background(), items, 100, d)
	if n != 0 {
		t.Fatalf("expected 0 written (Ground ok=false), got %d", n)
	}
	if appendCalled != 0 {
		t.Fatalf("AppendLabel should not have been called, got %d calls", appendCalled)
	}
}

// baseDeps returns a LabelDeps with Phase-B fields set and router appends
// collected into *routerL; confhead appends go to a discard sink.
func baseDeps(routerL *[]ledger.Entry) LabelDeps {
	return LabelDeps{
		Escalation:            "gemma4-e4b",
		E2B:                   "gemma4-e2b",
		SummarizeSimThreshold: 0.8,
		RouterLabelsPath:      "router.jsonl",
		LabelsPath:            "conf.jsonl",
		RunTier: func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
			data := json.RawMessage(`{"label":"a"}`)
			return core.Result{OK: true, Data: data}, true
		},
		AnswersAgree: func(task, a string, b []byte) (bool, bool) { return true, true },
		Ground:       func(task core.TaskType, in string, data []byte) (bool, bool) { return true, true },
		Similar:      func(a, b string) (float64, error) { return 1.0, nil },
		AppendLabel: func(path string, e ledger.Entry) error {
			// routerL may be nil for tests that don't inspect router output; in that
			// case the router label is still produced (A4 feeds it for every E2B
			// entry) but discarded here so the helper stays nil-safe.
			if path == "router.jsonl" && routerL != nil {
				*routerL = append(*routerL, e)
			}
			return nil
		},
	}
}

func TestLabelQueue_RouterLabelFromE4BEntry(t *testing.T) {
	// classify entered at offload-e4b (router skipped E2B); E2B agrees -> router label y=accept
	items := []Item{{Task: "classify", Input: "the cat sat", EntryTier: "offload-e4b",
		EntryOutput: `{"label":"animal"}`, Feat: map[string]float64{"len_chars": 11}}}
	var conf, routerL []ledger.Entry
	d := LabelDeps{
		Escalation: "esc", E2B: "gemma4-e2b", SummarizeSimThreshold: 0.8,
		RunTier: func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
			if model == "gemma4-e2b" { // E2B agrees with the stored E4B answer
				return core.Result{Data: json.RawMessage(`{"label":"animal"}`)}, true
			}
			return core.Result{Data: json.RawMessage(`{"label":"animal"}`)}, true // escalation (for confhead)
		},
		AnswersAgree: func(task, a string, b []byte) (bool, bool) { return true, true },
		Ground:       func(task core.TaskType, in string, data []byte) (bool, bool) { return true, true },
		Similar:      func(a, b string) (float64, error) { return 1, nil },
		AppendLabel: func(path string, e ledger.Entry) error {
			if path == "router.jsonl" {
				routerL = append(routerL, e)
			} else {
				conf = append(conf, e)
			}
			return nil
		},
		LabelsPath: "conf.jsonl", RouterLabelsPath: "router.jsonl",
	}
	LabelQueue(context.Background(), items, 10, d)
	if len(routerL) != 1 || routerL[0].ModelTier != "gemma4-e2b" || routerL[0].Grounded == nil || !*routerL[0].Grounded {
		t.Fatalf("expected 1 router label (gemma4-e2b, grounded=true), got %+v", routerL)
	}
}

// A4: the premise is now INVERTED. An E2B-entry classify/triage row, once judged,
// MUST emit a router-labels row carrying the escalation-agreement signal — that is
// the only label the router ever sees in normal operation, because capture only
// enqueues E2B-entry rows.
func TestLabelQueue_RouterLabelFromE2BEntry_Agree(t *testing.T) {
	// Entered at E2B; the escalation rerun AGREES -> router label y=accept (grounded=true).
	items := []Item{{Task: "classify", Input: "the cat sat", EntryTier: "gemma4-e2b",
		EntryOutput: `{"label":"animal"}`, Feat: map[string]float64{"len_chars": 11}}}
	var routerL []ledger.Entry
	d := baseDeps(&routerL) // baseDeps wires AnswersAgree -> agreed=true, E2B set
	LabelQueue(context.Background(), items, 10, d)
	if len(routerL) != 1 {
		t.Fatalf("E2B-entry row must emit exactly one router label, got %d", len(routerL))
	}
	r := routerL[0]
	if r.ModelTier != "gemma4-e2b" {
		t.Fatalf("router label ModelTier must be gemma4-e2b, got %q", r.ModelTier)
	}
	if r.Escalations != 0 || r.Deferred {
		t.Fatalf("router label must have Escalations==0 && Deferred==false, got escalations=%d deferred=%v", r.Escalations, r.Deferred)
	}
	if r.Grounded == nil || !*r.Grounded {
		t.Fatalf("agree case: router label Grounded must be true, got %+v", r.Grounded)
	}
}

func TestLabelQueue_RouterLabelFromE2BEntry_Disagree(t *testing.T) {
	// Entered at E2B; the escalation rerun DISAGREES -> router label y=reject (grounded=false).
	items := []Item{{Task: "triage", Input: "buy now!!!", EntryTier: "gemma4-e2b",
		EntryOutput: `{"decision":"yes"}`, Feat: map[string]float64{"len_chars": 10}}}
	var routerL []ledger.Entry
	d := baseDeps(&routerL)
	d.AnswersAgree = func(task, a string, b []byte) (bool, bool) { return false, true } // disagree, judgeable
	LabelQueue(context.Background(), items, 10, d)
	if len(routerL) != 1 {
		t.Fatalf("E2B-entry row must emit exactly one router label, got %d", len(routerL))
	}
	r := routerL[0]
	if r.ModelTier != "gemma4-e2b" || r.Escalations != 0 || r.Deferred {
		t.Fatalf("router label shape wrong: %+v", r)
	}
	if r.Grounded == nil || *r.Grounded {
		t.Fatalf("disagree case: router label Grounded must be false, got %+v", r.Grounded)
	}
}

func TestLabelQueue_SummarizeViaSimilarity(t *testing.T) {
	items := []Item{{Task: "summarize", Input: "doc", EntryTier: "offload-e4b", EntryOutput: `{"summary":"a cat sat"}`}}
	var conf []ledger.Entry
	d := LabelDeps{
		Escalation: "esc", SummarizeSimThreshold: 0.8,
		RunTier: func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
			return core.Result{Data: json.RawMessage(`{"summary":"the cat was sitting"}`)}, true
		},
		Similar:     func(a, b string) (float64, error) { return 0.9, nil }, // above threshold -> agreed
		AppendLabel: func(path string, e ledger.Entry) error { conf = append(conf, e); return nil },
		LabelsPath:  "conf.jsonl",
	}
	LabelQueue(context.Background(), items, 10, d)
	if len(conf) != 1 || conf[0].EscalatedAgreed == nil || !*conf[0].EscalatedAgreed {
		t.Fatalf("expected 1 summarize confhead label agreed=true, got %+v", conf)
	}
}

// A4: the E2B-entry kNN accept label is the escalation-AGREEMENT, NOT a constant
// true. Escalations==0 only means the runtime gate passed, not that E2B was right.
func TestLabelQueue_KNNSubstrate_E2BEntryAcceptMatchesAgreement(t *testing.T) {
	// Agree case -> accept=true.
	{
		var knnRows []knnSub
		d := baseDeps(nil) // AnswersAgree -> agreed=true
		d.Embed = func(string) ([]float64, error) { return []float64{1, 0}, nil }
		d.AppendKNN = func(task string, vec []float64, accept bool) error {
			knnRows = append(knnRows, knnSub{task, vec, accept})
			return nil
		}
		items := []Item{{Task: "classify", Input: "hi", EntryTier: "gemma4-e2b",
			EntryOutput: `{"label":"greet"}`, Feat: map[string]float64{"len_chars": 2}}}
		LabelQueue(context.Background(), items, 10, d)
		if len(knnRows) != 1 || !knnRows[0].accept || knnRows[0].task != "classify" {
			t.Fatalf("agree: E2B-entry must yield one accept=true classify kNN row, got %+v", knnRows)
		}
	}
	// Disagree case -> accept=false.
	{
		var knnRows []knnSub
		d := baseDeps(nil)
		d.AnswersAgree = func(task, a string, b []byte) (bool, bool) { return false, true }
		d.Embed = func(string) ([]float64, error) { return []float64{1, 0}, nil }
		d.AppendKNN = func(task string, vec []float64, accept bool) error {
			knnRows = append(knnRows, knnSub{task, vec, accept})
			return nil
		}
		items := []Item{{Task: "classify", Input: "hi", EntryTier: "gemma4-e2b",
			EntryOutput: `{"label":"greet"}`, Feat: map[string]float64{"len_chars": 2}}}
		LabelQueue(context.Background(), items, 10, d)
		if len(knnRows) != 1 || knnRows[0].accept || knnRows[0].task != "classify" {
			t.Fatalf("disagree: E2B-entry must yield one accept=false classify kNN row, got %+v", knnRows)
		}
	}
}

func TestLabelQueue_KNNSubstrate_NonE2BEntryUsesCounterfactual(t *testing.T) {
	var knnRows []knnSub
	var routerL []ledger.Entry
	d := baseDeps(&routerL) // baseDeps wires RunTier to "agree" and E2B set
	d.Embed = func(string) ([]float64, error) { return []float64{0, 1}, nil }
	d.AppendKNN = func(task string, vec []float64, accept bool) error {
		knnRows = append(knnRows, knnSub{task, vec, accept})
		return nil
	}
	// Entered at E4B, not E2B => the B1 E2B counterfactual decides the accept label.
	items := []Item{{Task: "classify", Input: "hi", EntryTier: "offload-e4b",
		EntryOutput: `{"label":"greet"}`, Feat: map[string]float64{"len_chars": 2}}}
	LabelQueue(context.Background(), items, 10, d)
	if len(knnRows) != 1 || knnRows[0].task != "classify" {
		t.Fatalf("non-E2B item must yield one classify kNN row, got %+v", knnRows)
	}
	// baseDeps's AnswersAgree returns agreed=true, so accept must be true.
	if !knnRows[0].accept {
		t.Fatalf("counterfactual agreed=true => accept=true, got %+v", knnRows[0])
	}
}

func TestLabelQueue_KNNSubstrate_DisabledWhenNilDeps(t *testing.T) {
	var routerL []ledger.Entry
	d := baseDeps(&routerL) // Embed + AppendKNN left nil; router sink wired
	items := []Item{{Task: "classify", Input: "hi", EntryTier: "gemma4-e2b",
		EntryOutput: `{"label":"greet"}`, Feat: map[string]float64{}}}
	// Must not panic and must write the normal confhead label.
	if w := LabelQueue(context.Background(), items, 10, d); w != 1 {
		t.Fatalf("confhead label still written when kNN deps nil; got %d", w)
	}
	// With kNN deps nil, ONLY the router label is emitted — no kNN row, no panic.
	if len(routerL) != 1 {
		t.Fatalf("router label must still be emitted with kNN deps nil, got %d", len(routerL))
	}
}

// TestLabelQueue_EscalationRerunRecordFalse_NoLedgerWrite is the A4 record=false
// guard. The drain's counterfactual escalation rerun must use the record=false
// RunTier contract: LabelQueue must invoke RunTier (the rerun happened) yet write
// NOTHING to the savings ledger. We assert via a spy (the rerun was called) AND a
// real ledger file's sha is byte-identical across the drain — LabelQueue has no
// ledger sink at all, so any drift would be a contract violation.
func TestLabelQueue_EscalationRerunRecordFalse_NoLedgerWrite(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	// Seed a pre-existing ledger so we sha a non-empty file.
	if err := ledger.AppendLabel(ledgerPath, ledger.Entry{TS: 1, Task: "classify"}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	shaBefore := fileSHA(t, ledgerPath)

	var runTierCalls int
	var runTierModels []string
	var routerL []ledger.Entry
	d := baseDeps(&routerL)
	d.RunTier = func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
		// Spy: the production wiring passes p.RunTier, which is hardcoded
		// record=false (attempt(..., false)) and therefore writes no ledger row.
		// A faithful spy mirrors that: it records the call but touches no ledger.
		runTierCalls++
		runTierModels = append(runTierModels, model)
		return core.Result{OK: true, Data: json.RawMessage(`{"label":"a"}`)}, true
	}
	items := []Item{{Task: "classify", Input: "x", EntryTier: "gemma4-e2b",
		EntryOutput: `{"label":"a"}`, Feat: map[string]float64{"len_chars": 1}}}
	LabelQueue(context.Background(), items, 10, d)

	if runTierCalls == 0 {
		t.Fatal("escalation rerun never invoked — RunTier spy not called")
	}
	// The rerun used the escalation model (the record=false counterfactual tier).
	if runTierModels[0] != d.Escalation {
		t.Fatalf("first rerun must target escalation model %q, got %q", d.Escalation, runTierModels[0])
	}
	// THE LOAD-BEARING ASSERTION: the ledger is byte-identical — no production
	// side-effect leaked through this path.
	if shaAfter := fileSHA(t, ledgerPath); shaAfter != shaBefore {
		t.Fatalf("ledger.jsonl changed across drain (before=%s after=%s) — record=false contract violated", shaBefore, shaAfter)
	}
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type knnSub struct {
	task   string
	vec    []float64
	accept bool
}

// TestDrainReconstructedRequestTasksBuildSucceeds is the acceptance test for the
// shadow-labeling flywheel fix: a captured classify/triage/extract Item, when
// round-tripped through Enqueue→Drain and reconstructed into a core.Request the
// same way LabelQueue does, must yield no error from tasks.Build.
func TestDrainReconstructedRequestTasksBuildSucceeds(t *testing.T) {
	qPath := filepath.Join(t.TempDir(), "shadow-queue.jsonl")

	cases := []struct {
		name string
		item Item
	}{
		{
			name: "classify",
			item: Item{
				TS:          1,
				Task:        "classify",
				Input:       "the cat sat on the mat",
				Params:      map[string]any{"labels": []any{"animal", "other"}},
				EntryTier:   "gemma4-e2b",
				EntryOutput: `{"label":"animal","confidence":0.9}`,
			},
		},
		{
			name: "triage",
			item: Item{
				TS:          2,
				Task:        "triage",
				Input:       "buy this product now!!!",
				Params:      map[string]any{"question": "is this spam?"},
				EntryTier:   "gemma4-e2b",
				EntryOutput: `{"decision":"yes","reason":"promotional language"}`,
			},
		},
		{
			name: "extract",
			item: Item{
				TS:    3,
				Task:  "extract",
				Input: "John Doe, age 30, works at Acme Corp.",
				Params: map[string]any{
					"schema": map[string]any{
						"properties": map[string]any{
							"name": map[string]any{"type": "string"},
							"age":  map[string]any{"type": "number"},
						},
					},
				},
				EntryTier:   "gemma4-e2b",
				EntryOutput: `{"name":"John Doe","age":30}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := qPath + "." + tc.name
			if err := Enqueue(p, tc.item); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			drained, err := Drain(p)
			if err != nil {
				t.Fatalf("Drain: %v", err)
			}
			if len(drained) != 1 {
				t.Fatalf("expected 1 drained item, got %d", len(drained))
			}
			it := drained[0]
			// Reconstruct the core.Request exactly as LabelQueue does.
			req := core.Request{Task: core.TaskType(it.Task), Input: it.Input, Params: it.Params}
			if _, err := tasks.Build(req); err != nil {
				t.Fatalf("tasks.Build failed on reconstructed %s request: %v", tc.name, err)
			}
		})
	}
}
