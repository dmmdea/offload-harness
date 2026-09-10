package rig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func row(id string, mut func(*Row)) Row {
	r := Row{TS: 1788700000, JobID: id, Node: "n1", Seat: "seat-a", Deferred: false, AcceptancePass: false,
		Contract: core.AgentContract{Goal: "g"}, Result: &core.AgentWireResult{Steps: 3, StopReason: "done"}}
	if mut != nil {
		mut(&r)
	}
	return r
}

func trace(steps ...core.AgentTraceStep) []core.AgentTraceStep { return steps }
func st(step int, tool, status string, obs int) core.AgentTraceStep {
	return core.AgentTraceStep{Step: step, Tool: tool, Status: status, ObsChars: obs}
}

// One row per axis, each carrying ONLY the evidence of its axis, plus the
// boundary cases where two predicates are both true and precedence decides.
func fixture() []Row {
	return []Row{
		row("pass", func(r *Row) { r.AcceptancePass = true }),
		row("infra", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "infrastructure"
			r.Result.Reason = "agent loop: chat 1: 503"
		}),
		row("timeout", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "budget"
			r.Result.Reason = "wall timeout after 300s"
			r.Result.StopReason = "error"
		}),
		row("timeout-deadline", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "budget"
			r.Result.Reason = ""
			r.Result.StopReason = "error"
		}),
		row("budget", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "budget"
			r.Result.Reason = "step budget exhausted (12 steps)"
			r.Result.StopReason = "budget"
		}),
		row("starved", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "budget"
			r.Result.Reason = "empty final answer after 1 steps and 8192 completion tokens: finish length, 4096 of 4096 completion tokens were reasoning"
			r.Result.StopReason = "reasoning_starved"
		}),
		row("empty-stop", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "abstention"
			r.Result.Reason = "empty final answer after 2 steps and 40 completion tokens: finish stop, empty message"
			r.Result.StopReason = "empty"
		}),
		row("abstain", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "abstention"
			r.Result.Reason = "the document does not say"
		}),
		row("schema", func(r *Row) {
			r.AcceptanceFailures = []string{`min_items:findings:3: field "findings" has 0 items, want ≥ 3`}
			r.Result.Steps = 5
		}),
		row("schema-2step", func(r *Row) {
			r.AcceptanceFailures = []string{`nonempty:summary: field "summary" is an empty string`}
			r.Contract.Context = []core.ContextDoc{{Name: "d.md", Text: "x"}}
			r.Result.Steps = 2
		}),
		row("loop-rules", func(r *Row) { r.Result.RulesFired = 2; r.Result.Trace = trace(st(1, "read_file", "committed", 10)) }),
		row("loop-run", func(r *Row) {
			r.Result.Trace = trace(st(1, "list_dir", "committed", 5), st(2, "list_dir", "committed", 5), st(3, "list_dir", "committed", 5))
		}),
		row("longobs", func(r *Row) { r.Result.Trace = trace(st(1, "read_file", "committed", 9000)) }),
		row("misuse", func(r *Row) { r.Result.Trace = trace(st(1, "grep", "none", 20), st(2, "read_file", "failed", 30)) }),
		row("unclass", func(r *Row) { r.Result.Reason = "something else entirely" }),
		// boundary cases: precedence decides
		row("infra-with-timeout-text", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "infrastructure"
			r.Result.Reason = "wall timeout after 300s while the seat answered 503"
		}),
		row("timeout-with-trace-longobs", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "budget"
			r.Result.Reason = "wall timeout after 300s"
			r.Result.StopReason = "error"
			r.Result.Trace = trace(st(1, "read_file", "committed", 20000))
		}),
		row("schema-with-loop-trace", func(r *Row) {
			r.AcceptanceFailures = []string{`min_items:a:1: field "a" has 0 items, want ≥ 1`}
			r.Result.Trace = trace(st(1, "x", "committed", 1), st(2, "x", "committed", 1), st(3, "x", "committed", 1))
		}),
		row("other-seat", func(r *Row) { r.Seat = "seat-b"; r.Deferred = true; r.DeferClass = "abstention" }),
		// the blind-label pass (2026-09-07) found these three shapes uncovered
		row("dispatch-503", func(r *Row) {
			r.Deferred = false
			r.DeferClass = ""
			r.Error = "dispatch: node http://n1:18811 answered HTTP 503: shed: no idle execution slot"
			r.Result = nil
		}),
		row("abstain-schema-invalid", func(r *Row) {
			r.Deferred = true
			r.DeferClass = "abstention"
			r.Result.Reason = "output failed schema: schema validation failed: jsonschema validation failed with 'file:///schema.json#'"
		}),
		row("anchor", func(r *Row) { r.AcceptanceFailures = []string{`regex:(?i)(alpha|beta): no match`}; r.Result.Steps = 4 }),
		row("anchor-2step", func(r *Row) {
			r.AcceptanceFailures = []string{`contains:PONG-x: not found`}
			r.Contract.Context = []core.ContextDoc{{Name: "d.md", Text: "x"}}
			r.Result.Steps = 1
		}),
	}
}

func TestClassifyEveryAxisAndPrecedence(t *testing.T) {
	want := map[string]Verdict{
		"pass":                       {Axis: ""},
		"infra":                      {Axis: AxisSeatInfra},
		"timeout":                    {Axis: AxisTimeout},
		"timeout-deadline":           {Axis: AxisTimeout},
		"budget":                     {Axis: AxisBudget},
		"starved":                    {Axis: AxisReasoningStarved},
		"empty-stop":                 {Axis: AxisReasoningStarved},
		"abstain":                    {Axis: AxisAbstention},
		"schema":                     {Axis: AxisSchemaMiss},
		"schema-2step":               {Axis: AxisSchemaMiss, Sub: SubTwoStepGrounded},
		"loop-rules":                 {Axis: AxisLoop},
		"loop-run":                   {Axis: AxisLoop},
		"longobs":                    {Axis: AxisLongObservation},
		"misuse":                     {Axis: AxisToolMisuse},
		"unclass":                    {Axis: AxisUnclassified},
		"infra-with-timeout-text":    {Axis: AxisSeatInfra},
		"timeout-with-trace-longobs": {Axis: AxisTimeout},
		"schema-with-loop-trace":     {Axis: AxisSchemaMiss},
		"dispatch-503":               {Axis: AxisSeatInfra},
		"abstain-schema-invalid":     {Axis: AxisSchemaMiss, Sub: SubInvalid},
		"anchor":                     {Axis: AxisAnchorMiss},
		"anchor-2step":               {Axis: AxisAnchorMiss, Sub: SubTwoStepGrounded},
	}
	for _, r := range fixture() {
		w, ok := want[r.JobID]
		if !ok {
			continue
		}
		got := Classify(r)
		if got.Axis != w.Axis || got.Sub != w.Sub {
			t.Errorf("%s: got %s/%s want %s/%s (evidence %q)", r.JobID, got.Axis, got.Sub, w.Axis, w.Sub, got.Evidence)
		}
		if w.Axis != "" && got.Evidence == "" {
			t.Errorf("%s: no evidence", r.JobID)
		}
	}
}

func TestBuildWeightsOverEligibleRowsAndIsDeterministic(t *testing.T) {
	since, until := time.Unix(1788600000, 0), time.Unix(1788800000, 0)
	rep, err := Build(fixture(), "seat-a", "", since, until)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows != 22 || rep.Bad != 21 { // +starved, +empty-stop (0.115.8)
		t.Fatalf("rows=%d bad=%d", rep.Rows, rep.Bad)
	}
	// trace-bearing bad rows: loop-rules, loop-run, longobs, misuse, timeout-with-trace-longobs, schema-with-loop-trace = 6
	if rep.BadWithTrace != 6 {
		t.Fatalf("bad_with_trace = %d", rep.BadWithTrace)
	}
	find := func(a Axis, sub string) AxisReport {
		for _, x := range rep.Axes {
			if x.Axis == a && x.Sub == sub {
				return x
			}
		}
		t.Fatalf("axis %s/%s missing", a, sub)
		return AxisReport{}
	}
	if x := find(AxisLoop, ""); x.Hits != 2 || x.Eligible != 6 || x.Weight < 0.33 || x.Weight > 0.34 {
		t.Fatalf("loop = %+v (weight must be over the TRACE-bearing bad rows, not all bad rows)", x)
	}
	if x := find(AxisSeatInfra, ""); x.Hits != 3 || x.Eligible != 21 || x.Remedy.Key != "" || !strings.Contains(x.TopReason, "x") {
		t.Fatalf("seat-infra = %+v", x)
	}
	if x := find(AxisSchemaMiss, SubTwoStepGrounded); x.Hits != 1 || x.Remedy.Key != "agent_seed_context_reads" {
		t.Fatalf("two-step-grounded = %+v", x)
	}
	if x := find(AxisSchemaMiss, SubInvalid); x.Hits != 1 {
		t.Fatalf("schema-miss/invalid = %+v", x)
	}
	if x := find(AxisSchemaMiss, ""); x.Hits != 4 {
		t.Fatalf("schema-miss parent must count its sub-axis rows too: %+v", x)
	}
	if x := find(AxisAnchorMiss, ""); x.Hits != 2 || x.Remedy.Applies != "contract" {
		t.Fatalf("anchor-miss = %+v", x)
	}
	if x := find(AxisAnchorMiss, SubTwoStepGrounded); x.Hits != 1 {
		t.Fatalf("anchor-miss/two-step-grounded = %+v", x)
	}
	if len(rep.Precedence) != len(Precedence) || rep.Thresholds["long_observation_chars"] != LongObservationChars {
		t.Fatalf("report must publish the precedence and thresholds: %+v", rep)
	}
	// deterministic: the same input twice is byte-identical
	a, _ := json.Marshal(rep)
	rep2, _ := Build(fixture(), "seat-a", "", since, until)
	b, _ := json.Marshal(rep2)
	if string(a) != string(b) {
		t.Fatal("two builds on the same rows differ")
	}
	md := Markdown(rep)
	for _, s := range []string{"seat seat-a", "precedence: seat-infra → timeout", "| loop | 2 / 6 |", "not a rule matter", "agent_seed_context_reads", "proposes nothing"} {
		if !strings.Contains(md, s) {
			t.Errorf("markdown lacks %q", s)
		}
	}
}

func TestBuildUnknownSeatNamesTheSeatsSeenAndCleanCorpusHasNothingToPropose(t *testing.T) {
	since, until := time.Unix(1788600000, 0), time.Unix(1788800000, 0)
	_, err := Build(fixture(), "seat-zzz", "", since, until)
	if err == nil || !strings.Contains(err.Error(), "seat-a") || !strings.Contains(err.Error(), "seat-b") {
		t.Fatalf("unknown seat must fail naming the seats seen: %v", err)
	}
	clean := []Row{row("p1", func(r *Row) { r.AcceptancePass = true }), row("p2", func(r *Row) { r.AcceptancePass = true })}
	rep, err := Build(clean, "seat-a", "", since, until)
	if err != nil || rep.Bad != 0 {
		t.Fatalf("clean corpus: %v %+v", err, rep)
	}
	for _, x := range rep.Axes {
		if x.Hits != 0 || x.Weight != 0 {
			t.Fatalf("clean corpus must have zero hits everywhere: %+v", x)
		}
	}
	// node filter
	_, err = Build(fixture(), "seat-a", "n-other", since, until)
	if err == nil {
		t.Fatal("a node with no rows must fail")
	}
}

// TestAgreementWithBlindLabels is the acceptance instrument for the
// classifier: RIG_SAMPLE names a JSON array of row FACTS (job_id, seat,
// deferred, defer_class, acceptance_pass, error, reason, stop_reason, steps,
// rules_fired, trace, context_docs, acceptance_failures) and RIG_LABELS a
// JSON array of {job_id, axis, sub} an independent reader produced from the
// published rules. It prints agreement per axis and the disagreements; it
// fails only when RIG_MIN_AGREEMENT (a fraction) is set and not met. Skipped
// when the env is unset, so the suite stays hermetic.
func TestAgreementWithBlindLabels(t *testing.T) {
	sample, labels := os.Getenv("RIG_SAMPLE"), os.Getenv("RIG_LABELS")
	if sample == "" || labels == "" {
		t.Skip("RIG_SAMPLE / RIG_LABELS unset")
	}
	var facts []struct {
		JobID              string                `json:"job_id"`
		Seat               string                `json:"seat"`
		Deferred           bool                  `json:"deferred"`
		DeferClass         string                `json:"defer_class"`
		AcceptancePass     bool                  `json:"acceptance_pass"`
		Error              string                `json:"error"`
		Reason             string                `json:"reason"`
		StopReason         string                `json:"stop_reason"`
		Steps              int                   `json:"steps"`
		RulesFired         int                   `json:"rules_fired"`
		Trace              []core.AgentTraceStep `json:"trace"`
		ContextDocs        int                   `json:"context_docs"`
		AcceptanceFailures []string              `json:"acceptance_failures"`
	}
	var labelled []struct {
		JobID string `json:"job_id"`
		Axis  string `json:"axis"`
		Sub   string `json:"sub"`
	}
	mustLoad := func(p string, v any) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	mustLoad(sample, &facts)
	mustLoad(labels, &labelled)
	want := map[string][2]string{}
	for _, l := range labelled {
		want[l.JobID] = [2]string{l.Axis, l.Sub}
	}
	agree, total := 0, 0
	perAxis := map[string][2]int{}
	for _, f := range facts {
		r := Row{JobID: f.JobID, Seat: f.Seat, Deferred: f.Deferred, DeferClass: f.DeferClass, AcceptancePass: f.AcceptancePass, Error: f.Error,
			AcceptanceFailures: f.AcceptanceFailures,
			Result:             &core.AgentWireResult{Reason: f.Reason, StopReason: f.StopReason, Steps: f.Steps, RulesFired: f.RulesFired, Trace: f.Trace}}
		for i := 0; i < f.ContextDocs; i++ {
			r.Contract.Context = append(r.Contract.Context, core.ContextDoc{Name: "d", Text: "t"})
		}
		got := Classify(r)
		w, ok := want[f.JobID]
		if !ok {
			continue
		}
		total++
		pa := perAxis[w[0]]
		pa[1]++
		if string(got.Axis) == w[0] && got.Sub == w[1] {
			agree++
			pa[0]++
		} else {
			t.Logf("DISAGREE %s: classifier %s/%s vs label %s/%s (%s)", f.JobID, got.Axis, got.Sub, w[0], w[1], got.Evidence)
		}
		perAxis[w[0]] = pa
	}
	t.Logf("AGREEMENT %d/%d = %.0f%%", agree, total, 100*float64(agree)/float64(total))
	for a, pa := range perAxis {
		t.Logf("  %-18s %d/%d", a, pa[0], pa[1])
	}
	if min := os.Getenv("RIG_MIN_AGREEMENT"); min != "" {
		var f float64
		fmt.Sscanf(min, "%g", &f)
		if float64(agree)/float64(total) < f {
			t.Fatalf("agreement %d/%d below %s", agree, total, min)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for spec, want := range map[string]time.Time{
		"":                     now.Add(-7 * 24 * time.Hour),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"36h":                  now.Add(-36 * time.Hour),
		"2026-09-01T00:00:00Z": time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := ParseSince(spec, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseSince(%q) = %v, %v; want %v", spec, got, err, want)
		}
	}
	for _, bad := range []string{"7", "0d", "-1d", "yesterday", "7w"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("ParseSince(%q) must fail", bad)
		}
	}
}

func TestReadShardsWindowAndSkips(t *testing.T) {
	dir := t.TempDir()
	// An instant late in the LOCAL day: the shard is named by the local date,
	// and a UTC day walk west of Greenwich would look for tomorrow's file.
	day := time.Date(2026, 9, 6, 23, 30, 0, 0, time.Local)
	shard := filepath.Join(dir, day.Format("2006-01-02")+".jsonl")
	lines := []string{
		fmt.Sprintf(`{"ts":%d,"job_id":"a","seat":"s","deferred":true,"defer_class":"abstention","contract":{"goal":"g"}}`, day.Unix()),
		`not json`,
		`{"ts":1000,"job_id":"old","seat":"s","deferred":true,"contract":{"goal":"g"}}`,
	}
	if err := os.WriteFile(shard, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, skipped, err := ReadShards(dir, day.Add(-time.Hour), day.Add(time.Hour))
	if err != nil || skipped != 1 || len(rows) != 1 || rows[0].JobID != "a" {
		t.Fatalf("rows=%+v skipped=%d err=%v", rows, skipped, err)
	}
	if _, _, err := ReadShards(filepath.Join(dir, "missing"), day, day); err != nil {
		t.Fatalf("missing shards are not an error: %v", err)
	}
}
