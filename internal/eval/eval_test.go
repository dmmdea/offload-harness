package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/local-offload-pp-cli/internal/core"
)

func TestLoadCases(t *testing.T) {
	cases, err := LoadCases("testdata/sample.jsonl")
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("want 2 cases, got %d", len(cases))
	}
	if cases[0].Task != "triage" || cases[0].Expect != "yes" {
		t.Fatalf("bad case[0]: %+v", cases[0])
	}
	if cases[1].Params["labels"] == nil {
		t.Fatalf("case[1] missing labels param")
	}
}

func TestLoadCases_MissingFile(t *testing.T) {
	cases, err := LoadCases(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(cases) != 0 {
		t.Fatalf("missing file should yield 0 cases, got %d", len(cases))
	}
}

func TestLoadCases_SkipsMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mixed.jsonl")
	content := `{"task":"triage","input":"x","expect":"yes"}` + "\n" + `{not valid json` + "\n" + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cases, err := LoadCases(p)
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("want 1 valid case (malformed+blank skipped), got %d", len(cases))
	}
}

func TestGradeClassify(t *testing.T) {
	c := Case{Task: "classify", Input: "x", Expect: "billing"}
	good := core.Result{OK: true, Data: json.RawMessage(`{"label":"billing","confidence":0.9}`)}
	bad := core.Result{OK: true, Data: json.RawMessage(`{"label":"support"}`)}
	if !Grade(c, good) {
		t.Fatal("expected correct for matching label")
	}
	if Grade(c, bad) {
		t.Fatal("expected incorrect for wrong label")
	}
}

func TestGradeExtractGrounded(t *testing.T) {
	c := Case{Task: "extract", Input: "Total amount 4200 due to Acme Corp"}
	grounded := core.Result{OK: true, Data: json.RawMessage(`{"amount":"4200","party":"Acme Corp"}`)}
	hallucinated := core.Result{OK: true, Data: json.RawMessage(`{"amount":"9999"}`)}
	if !Grade(c, grounded) {
		t.Fatal("expected grounded extract to pass")
	}
	if Grade(c, hallucinated) {
		t.Fatal("expected hallucinated extract to fail")
	}
}

type fakeRunner struct{ res map[string]core.Result }

func (f fakeRunner) Run(_ context.Context, req core.Request) core.Result { return f.res[req.Input] }

func TestRun(t *testing.T) {
	cases := []Case{
		{Task: "classify", Input: "a", Expect: "billing", Params: map[string]any{"labels": []any{"billing", "support"}}},
		{Task: "classify", Input: "b", Expect: "support", Params: map[string]any{"labels": []any{"billing", "support"}}},
	}
	fr := fakeRunner{res: map[string]core.Result{
		"a": {OK: true, Data: json.RawMessage(`{"label":"billing"}`), Meta: core.Meta{TokensOut: 10}},
		"b": {OK: false, Deferred: true, Meta: core.Meta{TokensOut: 5}},
	}}
	outs := Run(context.Background(), fr, cases)
	if len(outs) != 2 {
		t.Fatalf("want 2 outcomes, got %d", len(outs))
	}
	if !outs[0].Accepted || !outs[0].Correct {
		t.Fatalf("case a should be accepted+correct: %+v", outs[0])
	}
	if outs[1].Accepted || !outs[1].Deferred {
		t.Fatalf("case b should be deferred: %+v", outs[1])
	}
}

func TestAggregate(t *testing.T) {
	outs := []Outcome{
		{Case: Case{Task: "classify"}, Accepted: true, Correct: true, TokensOut: 100},
		{Case: Case{Task: "classify"}, Accepted: true, Correct: false, TokensOut: 100},
		{Case: Case{Task: "classify"}, Deferred: true, TokensOut: 0},
	}
	rep := Aggregate(outs)["classify"]
	if rep.N != 3 || rep.Accepted != 2 || rep.AcceptedCorrect != 1 || rep.Deferred != 1 {
		t.Fatalf("bad counts: %+v", rep)
	}
	if rep.AccuracyAccepted != 0.5 {
		t.Fatalf("want accuracy 0.5, got %v", rep.AccuracyAccepted)
	}
	// 1 correct over 200 tok => 5.0 correct per 1k tok
	if rep.AccPer1kTok != 5.0 {
		t.Fatalf("want 5.0 acc/1k, got %v", rep.AccPer1kTok)
	}
}

func TestRiskCoverageAURC(t *testing.T) {
	// Perfect ranking (all correct above all wrong) == oracle => E-AURC ~ 0.
	pts := []RCPoint{
		{Confidence: 0.9, Correct: true}, {Confidence: 0.8, Correct: true},
		{Confidence: 0.4, Correct: false}, {Confidence: 0.3, Correct: false},
	}
	_, _, aurc, eaurc := RiskCoverage(pts)
	if eaurc < -1e-9 || eaurc > 1e-9 {
		t.Fatalf("perfect ranking should give E-AURC ~0, got %v (aurc %v)", eaurc, aurc)
	}
	// Reversed ranking (wrong predictions most confident) => strictly worse AURC.
	rev := []RCPoint{
		{Confidence: 0.9, Correct: false}, {Confidence: 0.8, Correct: false},
		{Confidence: 0.4, Correct: true}, {Confidence: 0.3, Correct: true},
	}
	_, _, aurcRev, _ := RiskCoverage(rev)
	if aurcRev <= aurc {
		t.Fatalf("reversed ranking should have higher AURC: rev %v vs %v", aurcRev, aurc)
	}
}

func TestDeferralCurve(t *testing.T) {
	pts := []OpPoint{
		{Label: "entry", Cost: 10, Quality: 0.6},
		{Label: "full", Cost: 30, Quality: 0.9},
	}
	audc, qnc, peak := DeferralCurve(pts)
	if peak != 0.9 {
		t.Fatalf("peak should be 0.9, got %v", peak)
	}
	if audc < 0.749 || audc > 0.751 { // norm costs 0,1; trapezoid (0.6+0.9)/2
		t.Fatalf("AUDC should be ~0.75, got %v", audc)
	}
	if qnc < 0.999 || qnc > 1.001 { // peak reached only at full (norm cost 1.0)
		t.Fatalf("QNC should be ~1.0, got %v", qnc)
	}
}
