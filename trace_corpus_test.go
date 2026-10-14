package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// Tests for the trace-corpus verb. The fixtures under testdata/tracecorpus are
// SYNTHETIC: invented ids, invented seats, invented numbers. They hold a quiet run, a compacted
// run, a run that re-fetched, a record written before the instruments existed, and the shapes a
// real log carries (a refusal row, a dispatch error, a blank line, junk).

const (
	tcFixtureTraces     = "testdata/tracecorpus/traces"
	tcFixtureDelegation = "testdata/tracecorpus/delegation-log"

	// tcWantNeverRan is the skip reason of a delegation row whose agent never ran.
	tcWantNeverRan = "delegation row never ran (zero steps, nothing recorded)"
)

func tcFixtureOpts() tcHarvestOpts {
	return tcHarvestOpts{TracesDir: tcFixtureTraces, DelegationDir: tcFixtureDelegation, ExcludeSeats: map[string]bool{"fake-seat": true}}
}

// tcRunVerb runs the verb in-process and returns stdout, stderr and the error.
func tcRunVerb(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errw bytes.Buffer
	err := runTraceCorpusTo(args, &out, &errw)
	return out.String(), errw.String(), err
}

// tcHarvestFixture harvests both fixture directories to a corpus file in a temp dir.
func tcHarvestFixture(t *testing.T, name string) (path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), name)
	if _, _, err := tcRunVerb(t, "harvest", "--traces", tcFixtureTraces, "--delegation-log", tcFixtureDelegation, "--out", path); err != nil {
		t.Fatalf("harvest: %v", err)
	}
	return path
}

// TestTraceCorpusHarvestFreezeCheckReproducesByteIdentically is the acceptance line: harvesting
// the same records twice gives the same corpus bytes and the same hash, a frozen readout is
// reproduced by check, and any change to the corpus is refused rather than diffed.
func TestTraceCorpusHarvestFreezeCheckReproducesByteIdentically(t *testing.T) {
	c1, c2 := tcHarvestFixture(t, "one.jsonl"), tcHarvestFixture(t, "two.jsonl")
	b1, _ := os.ReadFile(c1)
	b2, _ := os.ReadFile(c2)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("two harvests of the same records differ:\n%s\n---\n%s", b1, b2)
	}
	if _, h1, err := tcLoadCorpus(c1); err != nil {
		t.Fatal(err)
	} else if _, h2, _ := tcLoadCorpus(c2); h1 != h2 {
		t.Fatalf("corpus hash differs between identical harvests: %s vs %s", h1, h2)
	}

	base := filepath.Join(t.TempDir(), "baseline.json")
	if _, _, err := tcRunVerb(t, "freeze", "--corpus", c1, "--out", base); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, _, err := tcRunVerb(t, "check", "--corpus", c2, "--baseline", base); err != nil {
		t.Fatalf("check must reproduce the frozen readout byte for byte: %v", err)
	}
	// readout prints exactly what freeze froze.
	out, _, err := tcRunVerb(t, "readout", "--corpus", c1)
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := os.ReadFile(base)
	if out != string(frozen) {
		t.Fatalf("readout differs from the frozen baseline")
	}

	// A baseline edited by one number no longer reproduces.
	edited := strings.Replace(string(frozen), `"trace_bearing_runs": 7`, `"trace_bearing_runs": 8`, 1)
	if edited == string(frozen) {
		t.Fatalf("fixture drifted: no `trace_bearing_runs: 7` to edit in\n%s", frozen)
	}
	badBase := filepath.Join(t.TempDir(), "bad-baseline.json")
	os.WriteFile(badBase, []byte(edited), 0o644)
	if _, _, err := tcRunVerb(t, "check", "--corpus", c1, "--baseline", badBase); err == nil || !strings.Contains(err.Error(), "BREACHED") {
		t.Fatalf("an edited baseline must breach, got %v", err)
	}
	// A different corpus is refused (hash mismatch), not compared.
	shrunk := filepath.Join(t.TempDir(), "shrunk.jsonl")
	lines := strings.SplitAfter(string(b1), "\n")
	os.WriteFile(shrunk, []byte(strings.Join(lines[:len(lines)-2], "")), 0o644)
	if _, _, err := tcRunVerb(t, "check", "--corpus", shrunk, "--baseline", base); err == nil || !strings.Contains(err.Error(), "check refused") {
		t.Fatalf("a baseline from another corpus must be refused, got %v", err)
	}
	// CRLF line endings (an editor, or a checkout) change neither the corpus hash nor the check.
	crlf := filepath.Join(t.TempDir(), "crlf.jsonl")
	os.WriteFile(crlf, bytes.ReplaceAll(b1, []byte("\n"), []byte("\r\n")), 0o644)
	if _, _, err := tcRunVerb(t, "check", "--corpus", crlf, "--baseline", base); err != nil {
		t.Fatalf("CRLF endings must not break the check: %v", err)
	}
}

// TestTraceCorpusReadoutNumbers pins every number the readout derives from the fixtures. The
// expected values are worked by hand in the comments.
func TestTraceCorpusReadoutNumbers(t *testing.T) {
	runs, stats, err := tcHarvest(tcFixtureOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Traces: goal-a, goal-b, goal-c-old, goal-e-badpager (goal-d-broken is not JSON).
	// Delegation runs, in file order: d1, d2 and d3 (seat-a, seat-a, seat-b) from the first shard
	// and d4 and d5 (both seat-b) from the second: 5 runs; the fake seat, the refusal, the dispatch error, the deferral that never
	// ran an agent (zero steps, nothing recorded), the blank and the junk line are not runs, and
	// notes.jsonl is not a dated shard.
	if len(runs) != 9 || stats.Runs != 9 || stats.BySource[tcSourceAgentTrace] != 4 || stats.BySource[tcSourceDelegation] != 5 {
		t.Fatalf("runs = %d, by source %v; want 9 = 4 traces + 5 delegation rows", len(runs), stats.BySource)
	}
	wantSkipped := map[string]int{
		"trace file not a trace record":                   1,
		"seat excluded":                                   1,
		"delegation row is a capacity refusal, not a run": 1,
		"delegation row has no result (dispatch error)":   1,
		tcWantNeverRan:                                    1,
		"delegation row not JSON":                         1,
		"delegation shard name is not a date":             1,
	}
	for k, want := range wantSkipped {
		if stats.Skipped[k] != want {
			t.Errorf("skipped[%q] = %d, want %d (all: %v)", k, stats.Skipped[k], want, stats.Skipped)
		}
	}
	if stats.Notes["pager object unusable (read as unrecorded)"] != 1 {
		t.Errorf("the self-contradicting pager object must be noted: %v", stats.Notes)
	}

	rep := tcBuildReport(runs, "hash")
	a := rep.All
	// trace-bearing: 4 traces with a transcript + d1, d2, d3 with a step trace = 7.
	if a.Runs != 9 || a.TraceBearingRuns != 7 {
		t.Errorf("runs/trace-bearing = %d/%d, want 9/7", a.Runs, a.TraceBearingRuns)
	}
	// steps: 6+3+4+2 (traces) + d1..d5 5+2+7+4+2 = 35.
	if a.Steps != 35 {
		t.Errorf("steps = %d, want 35", a.Steps)
	}
	// pager recorded by goal-a, goal-b and d1, d2, d4 = 5; measured: goal-a, d1, d4 = 3; distinct
	// evicted 4+5+2 = 11, re-fetched 1+0+1 = 2, so 2/11 = 0.181818. Unrecorded: goal-c-old,
	// goal-e-badpager (its pager object contradicts itself), d3 and d5 = 4.
	p := a.Pager
	if p.RecordedRuns != 5 || p.MeasuredRuns != 3 || p.UnexercisedRuns != 2 || p.UnrecordedRuns != 4 {
		t.Errorf("pager runs recorded/measured/unexercised/unrecorded = %d/%d/%d/%d, want 5/3/2/4", p.RecordedRuns, p.MeasuredRuns, p.UnexercisedRuns, p.UnrecordedRuns)
	}
	if p.DistinctEvicted != 11 || p.RefetchedDistinct != 2 || p.Evictions != 14 {
		t.Errorf("pager distinct/refetched/evictions = %d/%d/%d, want 11/2/14", p.DistinctEvicted, p.RefetchedDistinct, p.Evictions)
	}
	if p.RefetchRate == nil || *p.RefetchRate != 0.181818 || p.Verdict != tcVerdictAbove || p.Basis != tcBasisMeasured || p.Gate != agent.PagerGate {
		t.Errorf("pager rate/verdict/basis/gate = %v/%s/%s/%v, want 0.181818/%s/measured/%v", p.RefetchRate, p.Verdict, p.Basis, p.Gate, tcVerdictAbove, agent.PagerGate)
	}
	// compaction: 5 runs recorded a pass count (goal-a, goal-b, d1, d2, d4); passes 2+0+3+0+1 = 6 over 3 runs with a pass:
	// fire rate 3/5 = 0.6, passes per run 1.2, steps in those runs 6+3+5+2+4 = 20, so 0.3 a step.
	c := a.Compaction
	if c.RecordedRuns != 5 || c.UnrecordedRuns != 4 || c.RunsWithPass != 3 || c.Passes != 6 || c.StepsInRecordedRuns != 20 {
		t.Errorf("compaction counts = %+v", c)
	}
	if c.FireRate == nil || *c.FireRate != 0.6 || c.PassesPerRun == nil || *c.PassesPerRun != 1.2 || c.PassesPerStep == nil || *c.PassesPerStep != 0.3 {
		t.Errorf("compaction rates = %v/%v/%v, want 0.6/1.2/0.3", c.FireRate, c.PassesPerRun, c.PassesPerStep)
	}
	// transcript evidence: 4 traces keep a transcript, 2 of them (goal-a, goal-c-old) carry a ladder
	// artifact, so the fire rate is at least 2/4 = 0.5, whatever the pass counts say.
	if c.TranscriptRuns != 4 || c.RunsWithArtifacts != 2 || c.LowerBoundFireRate == nil || *c.LowerBoundFireRate != 0.5 {
		t.Errorf("transcript evidence = %d/%d/%v, want 4/2/0.5", c.TranscriptRuns, c.RunsWithArtifacts, c.LowerBoundFireRate)
	}
	// prefill: measured by goal-a, goal-b and d1, d2, d4 = 5 runs over 6+3+5+2+4 = 20 steps;
	// prefilled 1200+300+2000+400+800 = 4700, cached 3000+100+8000+0+200 = 11300, so reuse is
	// 11300/16000 = 70.625 %, 235 prefilled tokens a step and 940 a run.
	f := a.Prefill
	if f.MeasuredRuns != 5 || f.NoTokenCountsRuns != 1 || f.UnmeasuredRuns != 3 || f.ObservedSteps != 20 || f.PrefillTokens != 4700 || f.CacheTokens != 11300 {
		t.Errorf("prefill counts = %+v", f)
	}
	if f.KVReusePct == nil || *f.KVReusePct != 70.625 || f.AvgPrefillTokensPerStep == nil || *f.AvgPrefillTokensPerStep != 235 || f.AvgPrefillTokensPerRun == nil || *f.AvgPrefillTokensPerRun != 940 {
		t.Errorf("prefill rates = %v/%v/%v, want 70.625/235/940", f.KVReusePct, f.AvgPrefillTokensPerStep, f.AvgPrefillTokensPerRun)
	}

	// d5 observed 2 steps and reported no token counts (an engine that gives no timings): it is
	// counted apart, adds nothing to the token sums, and is not a measured 0.
	// Seat input tokens come from the three runs that reported them (d1, d2, d5): 10000+400+1500
	// = 11900 over 5+2+2 = 9 steps = 1322.222222 a step.
	if f.SeatInputRuns != 3 || f.SeatInputTokens != 11900 || f.SeatInputSteps != 9 || f.AvgSeatInputTokensPerStep == nil || *f.AvgSeatInputTokensPerStep != 1322.222222 {
		t.Errorf("seat input = %d runs, %d tokens, %d steps, %v a step; want 3/11900/9/1322.222222", f.SeatInputRuns, f.SeatInputTokens, f.SeatInputSteps, f.AvgSeatInputTokensPerStep)
	}

	// Per seat: seat-a has d1 and d2 (its third row never ran an agent); seat-b has d3, d4 and d5;
	// the four traces name no seat. A seat's slice is a slice of the same arithmetic.
	if got := rep.BySeat["seat-a"]; got.Runs != 2 || got.TraceBearingRuns != 2 || got.Pager.MeasuredRuns != 1 || got.Pager.RefetchRate == nil || *got.Pager.RefetchRate != 0 {
		t.Errorf("seat-a = %+v; its one measured run evicted 5 payloads and re-fetched none, a real 0", got.Pager)
	}
	if got := rep.BySeat["seat-b"]; got.Runs != 3 || got.Pager.RefetchRate == nil || *got.Pager.RefetchRate != 0.5 || got.Pager.Verdict != tcVerdictAbove {
		t.Errorf("seat-b = %+v, want 1 of 2 evicted payloads re-fetched", got.Pager)
	}
	if got := rep.BySeat[tcSeatUnrecorded]; got.Runs != 4 || got.Compaction.TranscriptRuns != 4 {
		t.Errorf("unrecorded seat = %+v", got)
	}
	if got := rep.BySource[tcSourceAgentTrace]; got.Runs != 4 {
		t.Errorf("by source = %+v", got)
	}
}

// TestTraceCorpusSkipsRowsThatNeverRan: a delegation row is a run only if an agent executed. A
// deferred subtask (capacity, infrastructure, budget, config, contract) is logged with zero steps
// and nothing else, and counting it would inflate the denominator of every unrecorded count: after
// the recording ships such a row still has no pager object, so the unrecorded count could never
// reach zero. A row that did real work stays a run even when it was deferred, and so does a row
// that recorded anything at all (a step trace, prefill, seat tokens, or a pager object, which a
// run the loop returned from carries, zeros included, unless it is a write-class deferral). That
// includes the zero-step deferred row whose only trace is a setup action: the setup runs inside
// the loop, before its first model turn, so the row reached the loop, will carry a pager object
// once the recording ships, and may have compacted before its first model call.
func TestTraceCorpusSkipsRowsThatNeverRan(t *testing.T) {
	dir := t.TempDir()
	rows := []string{
		// never ran: skipped
		`{"seat":"seat-a","deferred":true,"defer_class":"capacity","result":{"steps":0,"stop_reason":""}}`,
		`{"seat":"seat-a","deferred":true,"defer_class":"infrastructure","result":{"steps":0,"stop_reason":"error"}}`,
		`{"seat":"seat-a","result":{"steps":0}}`,
		// ran: kept
		`{"seat":"seat-a","deferred":true,"defer_class":"budget","result":{"steps":3,"stop_reason":"budget","trace":[{}],"prefill_steps":3,"prefill_tokens":10,"cache_tokens":5}}`,
		`{"seat":"seat-a","result":{"steps":4,"stop_reason":"done"}}`,
		`{"seat":"seat-a","result":{"steps":0,"stop_reason":"error","pager":{"basis":"insufficient_data","evictions":0,"evicted_bytes":0,"distinct_evicted":0,"refetched_distinct":0,"compaction_passes":0}}}`,
		`{"seat":"seat-a","result":{"steps":0,"stop_reason":"error","trace":[{}]}}`,
		`{"seat":"seat-a","deferred":true,"defer_class":"budget","result":{"steps":0,"stop_reason":"error","setup_ran":1,"trace":[{"step":0,"tool":"read_file","status":"committed","setup":true}]}}`,
		`{"seat":"seat-a","result":{"steps":0,"stop_reason":"error","seat_tokens_in":50}}`,
		`{"seat":"seat-a","result":{"steps":0,"stop_reason":"error","prefill_steps":2,"prefill_tokens":7,"cache_tokens":1}}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-03-01.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, stats, err := tcHarvest(tcHarvestOpts{DelegationDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if stats.DelegationRows != 10 || len(runs) != 7 || stats.Runs != 7 {
		t.Fatalf("rows/runs = %d/%d, want 10 rows and 7 runs: %v", stats.DelegationRows, len(runs), stats.Skipped)
	}
	if stats.Skipped[tcWantNeverRan] != 3 {
		t.Errorf("skipped[%q] = %d, want 3 (all: %v)", tcWantNeverRan, stats.Skipped[tcWantNeverRan], stats.Skipped)
	}
	// None of the seven runs recorded a pager object but one: the unrecorded count is now a count
	// of RUNS, 6, not of rows. The setup-only zero-step deferred row is one of the seven.
	if got := tcStatsFor(runs).Pager.UnrecordedRuns; got != 6 {
		t.Errorf("pager unrecorded runs = %d, want 6 of the 7 runs", got)
	}
	zeroStepKept := 0
	for _, r := range runs {
		if r.Steps == 0 && r.TraceBearing {
			zeroStepKept++
		}
	}
	if zeroStepKept != 2 {
		t.Errorf("zero-step runs with a step trace kept = %d, want 2 (a setup-only deferred row and a plain one)", zeroStepKept)
	}
	// A seat filter that rejects the row speaks first: the reason is the filter's, not never-ran.
	_, stats, err = tcHarvest(tcHarvestOpts{DelegationDir: dir, Seats: map[string]bool{"seat-b": true}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped[tcWantNeverRan] != 0 || stats.Skipped["seat filter: seat not selected"] != 10 {
		t.Errorf("under a seat filter every row is skipped by the filter: %v", stats.Skipped)
	}
}

// TestTraceCorpusSeatAndWindowFilters: the filters change what is harvested and are counted.
func TestTraceCorpusSeatAndWindowFilters(t *testing.T) {
	o := tcFixtureOpts()
	o.Seats = map[string]bool{"seat-b": true}
	runs, stats, err := tcHarvest(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("--seat seat-b kept %d runs, want its 3 delegation rows (agent traces name no seat)", len(runs))
	}
	if stats.Skipped["seat filter: record names no seat"] != 5 {
		t.Errorf("the 5 trace files in the directory must be counted as skipped by the seat filter (4 records + 1 broken): %v", stats.Skipped)
	}
	o = tcFixtureOpts()
	o.TracesDir, o.Since = "", "2026-01-02"
	runs, _, err = tcHarvest(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("--since 2026-01-02 kept %d runs, want the second shard's 2 (its other row never ran an agent)", len(runs))
	}
	o.Since, o.Until = "", "2026-01-01"
	if runs, _, _ = tcHarvest(o); len(runs) != 3 {
		t.Fatalf("--until 2026-01-01 kept %d runs, want the first shard's 3", len(runs))
	}
}

// TestTraceCorpusRedactsAtHarvest: anything identity- or credential-shaped in a string that goes
// into the corpus is replaced before the file exists, with stable placeholders, and the vet runs
// over the result. The literals are built at run time so no tracked file carries one.
func TestTraceCorpusRedactsAtHarvest(t *testing.T) {
	email := "someone" + "@" + "example.invalid"
	key := "ghp_" + strings.Repeat("d", 24)
	nodeKey := "ghp_" + strings.Repeat("e", 24)
	dir := t.TempDir()
	tracesDir := filepath.Join(dir, "traces")
	logDir := filepath.Join(dir, "log")
	os.MkdirAll(tracesDir, 0o755)
	os.MkdirAll(logDir, 0o755)
	// The only free strings a run keeps are its file-derived id, seat, node and stop reason.
	rec := `{"id":"x","stop_reason":"` + email + `","steps":1,"transcript":[{"Role":"user","Content":"hi"}]}`
	os.WriteFile(filepath.Join(tracesDir, "run-"+email+".json"), []byte(rec), 0o644)
	row := `{"seat":"` + key + `","node":"` + nodeKey + `","result":{"steps":2,"stop_reason":"done","trace":[{}]}}` + "\n"
	os.WriteFile(filepath.Join(logDir, "2026-02-03.jsonl"), []byte(row), 0o644)

	out := filepath.Join(dir, "corpus.jsonl")
	stdout, _, err := tcRunVerb(t, "harvest", "--traces", tracesDir, "--delegation-log", logDir, "--out", out)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	b, _ := os.ReadFile(out)
	for _, secret := range []string{email, key, nodeKey} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("the corpus still holds %q:\n%s", secret, b)
		}
	}
	if !strings.Contains(string(b), "[redacted-email-1]") || !strings.Contains(string(b), "[redacted-github-token-1]") {
		t.Fatalf("expected stable placeholders in the corpus:\n%s", b)
	}
	var harvested struct {
		Stats tcHarvestStats `json:"stats"`
	}
	if err := json.Unmarshal([]byte(stdout), &harvested); err != nil {
		t.Fatal(err)
	}
	if harvested.Stats.Redactions["email"] < 2 || harvested.Stats.Redactions["github-token"] != 2 {
		t.Errorf("redaction counts = %v, want the email twice (id and stop reason) and a token in the seat and one in the node", harvested.Stats.Redactions)
	}
	// The strict loader is the defense in depth: a corpus that still carries a vet class is refused.
	dirty := filepath.Join(dir, "dirty.jsonl")
	os.WriteFile(dirty, []byte(`{"id":"tr-`+email+`","source":"agent-trace","seat":"","trace_bearing":true,"steps":1,"pager":null,"prefill":null,"ladder_artifacts":null}`+"\n"), 0o644)
	if _, _, err := tcRunVerb(t, "readout", "--corpus", dirty); err == nil || !strings.Contains(err.Error(), "email") {
		t.Fatalf("a corpus holding an email must be refused, got %v", err)
	}
}

// TestTraceCorpusRedactsPathSeats: a seat name that is a local filesystem path (a drive path, a
// rooted path, anything with a backslash: a seat bound by model file) is a by_seat key in every
// readout and baseline, and the vet table has no path class, so the harvest replaces it with a
// stable placeholder and the loader refuses a corpus that still carries one. The paths are built
// at run time so no tracked file carries one.
func TestTraceCorpusRedactsPathSeats(t *testing.T) {
	drive := "Z" + ":" + "/" + "models" + "/" + "m1.gguf"
	winSlash := "Y" + ":" + "\\" + "stuff" + "\\" + "m2.gguf"
	rooted := "/" + "opt" + "/" + "m3.gguf"
	backslashed := "sub" + "\\" + "m4.gguf"
	// Each row is {seat, stop reason}; the last row names a path as its stop reason.
	seats := [][2]string{{drive, "done"}, {winSlash, "done"}, {drive, "done"}, {rooted, "done"}, {backslashed, "done"},
		{"seat-a", "done"}, {"models/org-model", "done"}, {"seat-a", rooted}}
	dir := t.TempDir()
	var rows []string
	for _, row := range seats {
		b, err := json.Marshal(map[string]any{"seat": row[0], "result": map[string]any{"steps": 1, "stop_reason": row[1]}})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, string(b))
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-04-01.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "corpus.jsonl")
	stdout, _, err := tcRunVerb(t, "harvest", "--delegation-log", dir, "--out", out)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	b, _ := os.ReadFile(out)
	raw := string(b)
	for _, leak := range []string{"models" + "/" + "m1", "stuff", "m2.gguf", "m3.gguf", "m4.gguf", "opt"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the corpus still holds a fragment of a path seat (%q):\n%s", leak, raw)
		}
	}
	// Stable: the same path is the same placeholder, numbered by first appearance; a seat that is
	// not path-shaped (a bare name, an org/model id with no leading slash) is left alone.
	runs, _, err := tcLoadCorpus(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.Seat+"/"+r.StopReason)
	}
	want := []string{"[redacted-path-1]/done", "[redacted-path-2]/done", "[redacted-path-1]/done", "[redacted-path-3]/done",
		"[redacted-path-4]/done", "seat-a/done", "models/org-model/done", "seat-a/[redacted-path-3]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("seat/stop reason after harvest = %v, want %v", got, want)
	}
	var harvested struct {
		Stats tcHarvestStats `json:"stats"`
	}
	if err := json.Unmarshal([]byte(stdout), &harvested); err != nil {
		t.Fatal(err)
	}
	if harvested.Stats.Redactions["path"] != 6 {
		t.Errorf("path redactions = %v, want 6 (one per path-shaped value: five seats and one stop reason)", harvested.Stats.Redactions)
	}
	// Defense in depth: the strict loader refuses a corpus that carries a path seat or stop reason.
	for name, fields := range map[string][2]string{"seat": {rooted, "done"}, "stop reason": {"seat-a", rooted}} {
		dirty := filepath.Join(dir, "dirty-"+strings.ReplaceAll(name, " ", "-")+".jsonl")
		line := `{"id":"dl-x","source":"delegation-log","seat":` + strconvQuote(fields[0]) + `,"trace_bearing":false,"steps":1,"stop_reason":` + strconvQuote(fields[1]) +
			`,"pager":null,"prefill":null,"seat_tokens_in":null,"ladder_artifacts":null}` + "\n"
		if err := os.WriteFile(dirty, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := tcRunVerb(t, "readout", "--corpus", dirty); err == nil || !strings.Contains(err.Error(), "local path") {
			t.Fatalf("a corpus holding a path as its %s must be refused, got %v", name, err)
		}
	}
}

// TestTraceCorpusRedactsPathNodes: the node id a delegation row carries is the one other identifier
// the readout keys by (pool_alias_by_node), and it is whatever the node calls itself. A node id that
// is a local filesystem path is vetted exactly like a seat or a stop reason: the harvest replaces it
// with a stable placeholder and the loader refuses a corpus that still carries one. A node id that
// is not path-shaped (a hostname, an id with a dot or a dash) is left as the row wrote it. The paths
// are built at run time so no tracked file carries one.
func TestTraceCorpusRedactsPathNodes(t *testing.T) {
	drive := "Z" + ":" + "/" + "nodes" + "/" + "n1"
	winSlash := "Y" + ":" + "\\" + "nodes" + "\\" + "n2"
	rooted := "/" + "srv" + "/" + "n3"
	backslashed := "sub" + "\\" + "n4"
	// Every row is a pool placement, so each node id becomes a key of pool_alias_by_node.
	nodes := []string{drive, rooted, drive, backslashed, winSlash, "node-1", "host.example", ""}
	dir := t.TempDir()
	var rows []string
	for _, node := range nodes {
		b, err := json.Marshal(map[string]any{"seat": "agent-pool", "node": node, "result": map[string]any{"steps": 1, "stop_reason": "done"}})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, string(b))
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-04-02.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "corpus.jsonl")
	stdout, _, err := tcRunVerb(t, "harvest", "--delegation-log", dir, "--out", out)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	b, _ := os.ReadFile(out)
	raw := string(b)
	for _, leak := range []string{"nodes" + "/" + "n1", "nodes", "srv", "n3", "n4", "n2"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the corpus still holds a fragment of a path node id (%q):\n%s", leak, raw)
		}
	}
	runs, _, err := tcLoadCorpus(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.Node)
	}
	// Stable: the same path is the same placeholder, numbered by first appearance in the corpus order.
	want := []string{"[redacted-path-1]", "[redacted-path-2]", "[redacted-path-1]", "[redacted-path-3]", "[redacted-path-4]",
		"node-1", "host.example", ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("node ids after harvest = %q, want %q", got, want)
	}
	var harvested struct {
		Stats tcHarvestStats `json:"stats"`
	}
	if err := json.Unmarshal([]byte(stdout), &harvested); err != nil {
		t.Fatal(err)
	}
	if harvested.Stats.Redactions["path"] != 5 {
		t.Errorf("path redactions = %v, want 5 (one per path-shaped node id)", harvested.Stats.Redactions)
	}
	// The readout key is the placeholder, never the path.
	readout, _, err := tcRunVerb(t, "readout", "--corpus", out)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		PoolAliasByNode map[string]struct {
			Runs int `json:"runs"`
		} `json:"pool_alias_by_node"`
	}
	if err := json.Unmarshal([]byte(readout), &rep); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]int{"[redacted-path-1]": 2, "[redacted-path-2]": 1, "[redacted-path-3]": 1, "[redacted-path-4]": 1,
		"node-1": 1, "host.example": 1, tcSeatUnrecorded: 1}
	if len(rep.PoolAliasByNode) != len(wantKeys) {
		t.Errorf("pool_alias_by_node = %v, want exactly %v", rep.PoolAliasByNode, wantKeys)
	}
	for k, n := range wantKeys {
		if rep.PoolAliasByNode[k].Runs != n {
			t.Errorf("pool_alias_by_node[%q] = %d runs, want %d", k, rep.PoolAliasByNode[k].Runs, n)
		}
	}
	// Defense in depth: the strict loader refuses a corpus that carries a path as its node id.
	for name, node := range map[string]string{"rooted": rooted, "drive": drive, "backslashed": backslashed} {
		dirty := filepath.Join(dir, "dirty-node-"+name+".jsonl")
		line := `{"id":"dl-x","source":"delegation-log","seat":"seat-a","node":` + strconvQuote(node) + `,"trace_bearing":false,"steps":1,"stop_reason":"done"` +
			`,"pager":null,"prefill":null,"seat_tokens_in":null,"ladder_artifacts":null}` + "\n"
		if err := os.WriteFile(dirty, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := tcRunVerb(t, "readout", "--corpus", dirty); err == nil || !strings.Contains(err.Error(), "local path") {
			t.Fatalf("a corpus holding a %s path as its node id must be refused, got %v", name, err)
		}
	}
}

// TestTraceCorpusPoolAliasRunsAreABucketKeyedByNode: a pool placement logs the pool's ALIAS as the
// seat (the node's real seat is in neither the row nor its result), so those runs cannot be
// attributed to a seat. They are reported as their own bucket, keyed by the node the row names, and
// the readout says how much of the corpus cannot be attributed instead of letting an alias read as
// a seat in by_seat. Nodes and seats are synthetic.
func TestTraceCorpusPoolAliasRunsAreABucketKeyedByNode(t *testing.T) {
	dir := t.TempDir()
	row := func(seat, node, extra string) string {
		return `{"seat":"` + seat + `","node":"` + node + `","result":{"steps":2,"stop_reason":"done","trace":[{}]` + extra + `}}`
	}
	rows := []string{
		row("agent-pool", "node-1", `,"pager":{"basis":"measured","evictions":5,"evicted_bytes":100,"distinct_evicted":4,"refetched_distinct":1,"compaction_passes":1}`),
		row("agent-pool", "node-1", ""),
		row("agent-pool-3card", "node-2", ""),
		row("arm-pool", "", ""),
		row("seat-a", "node-1", ""),
		row("", "node-1", ""),
		// not aliases: the word pool must be a whole hyphen-delimited token
		row("spool", "node-2", ""),
		row("poolside", "node-2", ""),
		row("carpool-a", "node-2", ""),
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-05-01.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(dir, "corpus.jsonl")
	if _, _, err := tcRunVerb(t, "harvest", "--delegation-log", dir, "--out", corpus); err != nil {
		t.Fatalf("harvest: %v", err)
	}
	out, _, err := tcRunVerb(t, "readout", "--corpus", corpus)
	if err != nil {
		t.Fatal(err)
	}
	type runs struct {
		Runs  int `json:"runs"`
		Pager struct {
			RefetchRate *float64 `json:"refetch_rate"`
		} `json:"pager"`
	}
	var rep struct {
		Attribution struct {
			Runs               int      `json:"runs"`
			SeatNamedRuns      int      `json:"seat_named_runs"`
			PoolAliasRuns      int      `json:"pool_alias_runs"`
			SeatUnrecordedRuns int      `json:"seat_unrecorded_runs"`
			UnattributedShare  *float64 `json:"unattributed_share"`
		} `json:"attribution"`
		PoolAlias       runs            `json:"pool_alias"`
		PoolAliasByNode map[string]runs `json:"pool_alias_by_node"`
		BySeat          map[string]runs `json:"by_seat"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	at := rep.Attribution
	if at.Runs != 9 || at.SeatNamedRuns != 4 || at.PoolAliasRuns != 4 || at.SeatUnrecordedRuns != 1 {
		t.Errorf("attribution = %+v, want 9 runs = 4 seat-named + 4 pool alias + 1 with no seat", at)
	}
	if at.UnattributedShare == nil || *at.UnattributedShare != 0.555556 {
		t.Errorf("unattributed share = %v, want (4 aliases + 1 no seat) / 9 = 0.555556", at.UnattributedShare)
	}
	if rep.PoolAlias.Runs != 4 {
		t.Errorf("pool_alias bucket holds %d runs, want 4", rep.PoolAlias.Runs)
	}
	if n1, n2, un := rep.PoolAliasByNode["node-1"], rep.PoolAliasByNode["node-2"], rep.PoolAliasByNode[tcSeatUnrecorded]; n1.Runs != 2 || n2.Runs != 1 || un.Runs != 1 || len(rep.PoolAliasByNode) != 3 {
		t.Errorf("pool_alias_by_node = %v, want node-1:2 node-2:1 and 1 under %s (the row names no node)", rep.PoolAliasByNode, tcSeatUnrecorded)
	}
	if got := rep.PoolAliasByNode["node-1"].Pager.RefetchRate; got == nil || *got != 0.25 {
		t.Errorf("node-1 pool-alias re-fetch rate = %v, want 1 of 4 = 0.25", got)
	}
	// by_seat names only runs a seat can be attributed to: no alias is a key.
	want := map[string]int{"seat-a": 1, "spool": 1, "poolside": 1, "carpool-a": 1, tcSeatUnrecorded: 1}
	if len(rep.BySeat) != len(want) {
		t.Errorf("by_seat = %v, want exactly %v", rep.BySeat, want)
	}
	for k, n := range want {
		if rep.BySeat[k].Runs != n {
			t.Errorf("by_seat[%q] = %d runs, want %d", k, rep.BySeat[k].Runs, n)
		}
	}
	// The sum rule: every run is in exactly one of the three attribution classes.
	if at.SeatNamedRuns+at.PoolAliasRuns+at.SeatUnrecordedRuns != at.Runs {
		t.Errorf("attribution classes do not add up: %+v", at)
	}
}

// tcPagerEdgeCases are the pager objects the validator must tell apart. The bad ones each trip ONE
// guard, so removing any single guard (or one term of it) lets exactly its case through.
var tcPagerEdgeCases = []struct {
	name string
	json string
	ok   bool
}{
	{"recorded zero passes, nothing evicted", `{"basis":"insufficient_data","compaction_passes":0}`, true},
	{"measured and consistent", `{"basis":"measured","evictions":5,"evicted_bytes":10,"distinct_evicted":4,"refetched_distinct":4,"compaction_passes":2}`, true},
	{"passes null", `{"basis":"insufficient_data","compaction_passes":null}`, false},
	{"passes key absent", `{"basis":"insufficient_data"}`, false},
	{"negative passes", `{"basis":"insufficient_data","compaction_passes":-1}`, false},
	{"unknown basis", `{"basis":"guess","compaction_passes":0}`, false},
	{"empty basis", `{"compaction_passes":0}`, false},
	{"negative evictions", `{"basis":"measured","evictions":-1,"distinct_evicted":1,"compaction_passes":0}`, false},
	{"negative evicted bytes", `{"basis":"measured","evictions":1,"evicted_bytes":-1,"distinct_evicted":1,"compaction_passes":0}`, false},
	{"negative distinct", `{"basis":"measured","evictions":1,"distinct_evicted":-1,"refetched_distinct":-2,"compaction_passes":0}`, false},
	{"negative refetched", `{"basis":"measured","evictions":1,"distinct_evicted":1,"refetched_distinct":-1,"compaction_passes":0}`, false},
	{"refetched more than evicted", `{"basis":"measured","evictions":1,"distinct_evicted":1,"refetched_distinct":2,"compaction_passes":0}`, false},
	{"measured with nothing evicted", `{"basis":"measured","compaction_passes":0}`, false},
	{"insufficient with evictions", `{"basis":"insufficient_data","evictions":2,"compaction_passes":0}`, false},
	{"insufficient with distinct evicted", `{"basis":"insufficient_data","distinct_evicted":1,"compaction_passes":0}`, false},
}

// TestTraceCorpusPagerObjectValidatorEdges: the rule the verb exists for is that unrecorded is never
// 0, and its edge is the pager object. A report whose pass count is null or absent (a node from before
// the field) must not read as a recorded zero-pass run, and an object that contradicts itself is
// unrecorded, never trusted. Each shape is read through the real JSON decode, so the null-versus-absent
// pointer semantics are what is under test.
func TestTraceCorpusPagerObjectValidatorEdges(t *testing.T) {
	if p, ok := (*tcPagerIn)(nil).run(); p != nil || !ok {
		t.Errorf("no pager object is a plain absence (nil, true), got %v/%v", p, ok)
	}
	for _, c := range tcPagerEdgeCases {
		var in tcPagerIn
		if err := json.Unmarshal([]byte(c.json), &in); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		p, ok := in.run()
		if ok != c.ok || (p != nil) != c.ok {
			t.Errorf("%s: run() = %v/%v, want usable=%v", c.name, p, ok, c.ok)
		}
	}
	// A usable object carries every field through, and a recorded zero stays a zero.
	var in tcPagerIn
	if err := json.Unmarshal([]byte(tcPagerEdgeCases[1].json), &in); err != nil {
		t.Fatal(err)
	}
	if p, _ := in.run(); p == nil || *p != (tcPager{Basis: tcBasisMeasured, Evictions: 5, EvictedBytes: 10, DistinctEvicted: 4, Refetched: 4, CompactionPasses: 2}) {
		t.Errorf("a usable pager object must map through unchanged, got %+v", p)
	}
	var zero tcPagerIn
	if err := json.Unmarshal([]byte(tcPagerEdgeCases[0].json), &zero); err != nil {
		t.Fatal(err)
	}
	if p, _ := zero.run(); p == nil || p.CompactionPasses != 0 {
		t.Errorf("a recorded 0 passes is a usable zero, got %+v", p)
	}
}

// TestTraceCorpusUnusablePagerObjectsReadAsUnrecordedAndAreNoted: at harvest, every shape the
// validator refuses lands as an unrecorded run with one note, from a delegation row and from a
// standalone trace alike, and the usable ones stay recorded.
func TestTraceCorpusUnusablePagerObjectsReadAsUnrecordedAndAreNoted(t *testing.T) {
	dir := t.TempDir()
	logDir, tracesDir := filepath.Join(dir, "log"), filepath.Join(dir, "traces")
	for _, d := range []string{logDir, tracesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var rows []string
	usable, unusable := 0, 0
	for _, c := range tcPagerEdgeCases {
		rows = append(rows, `{"seat":"seat-a","result":{"steps":1,"trace":[{}],"pager":`+c.json+`}}`)
		if c.ok {
			usable++
		} else {
			unusable++
		}
	}
	if err := os.WriteFile(filepath.Join(logDir, "2026-06-01.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One standalone trace with a null pass count: unusable too.
	trace := `{"stop_reason":"done","steps":1,"pager":` + tcPagerEdgeCases[2].json + `,"transcript":[{"Role":"user","Content":"hi"}]}`
	if err := os.WriteFile(filepath.Join(tracesDir, "goal-null-passes.json"), []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}
	unusable++
	runs, stats, err := tcHarvest(tcHarvestOpts{TracesDir: tracesDir, DelegationDir: logDir})
	if err != nil {
		t.Fatal(err)
	}
	if got := stats.Notes["pager object unusable (read as unrecorded)"]; got != unusable {
		t.Errorf("notes = %d, want %d (one per unusable object): %v", got, unusable, stats.Notes)
	}
	st := tcStatsFor(runs)
	if st.Pager.RecordedRuns != usable || st.Pager.UnrecordedRuns != unusable || st.Compaction.RecordedRuns != usable || st.Compaction.UnrecordedRuns != unusable {
		t.Errorf("recorded/unrecorded = pager %d/%d compaction %d/%d, want %d/%d", st.Pager.RecordedRuns, st.Pager.UnrecordedRuns,
			st.Compaction.RecordedRuns, st.Compaction.UnrecordedRuns, usable, unusable)
	}
}

// TestTraceCorpusLadderArtifactsCountOnlyToolTurns: the transcript evidence for the ladder is a tool
// turn that is a product of it (an elision marker, a dedupe reference). The same text in any other
// role is not evidence: the model can quote it, a user can paste it.
func TestTraceCorpusLadderArtifactsCountOnlyToolTurns(t *testing.T) {
	elided := "[earlier result elided to fit context — 4096 chars]"
	deduped := "[identical to the later result for call c1 — 10 chars]"
	msg := func(role, content string) map[string]string {
		return map[string]string{"Role": role, "Content": content}
	}
	write := func(dir, name string, transcript ...map[string]string) {
		b, err := json.Marshal(map[string]any{"stop_reason": "done", "steps": 2, "transcript": transcript})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	// Artifact-looking text in every role but tool: no ladder evidence.
	write(dir, "quoted", msg("system", elided), msg("user", elided), msg("assistant", deduped), msg("tool", "raw file content"))
	// A tool turn that is an elision marker and one that is a dedupe reference: two.
	write(dir, "compacted", msg("user", "hi"), msg("tool", elided), msg("tool", deduped), msg("tool", "raw file content"))
	runs, _, err := tcHarvest(tcHarvestOpts{TracesDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range runs {
		if r.LadderArtifacts == nil {
			t.Fatalf("%s: a trace with a transcript must carry a ladder_artifacts count", r.ID)
		}
		got[r.ID] = *r.LadderArtifacts
	}
	if got["tr-quoted"] != 0 || got["tr-compacted"] != 2 {
		t.Errorf("ladder artifacts = %v, want quoted:0 compacted:2 (only tool turns count)", got)
	}
}

// TestTraceCorpusPrefillDistribution: an average hides the long runs, so the readout also gives the
// per-run spread of the two input-volume figures: prefilled tokens over the MEASURED runs (a
// no-token-counts run is not a measured zero and is left out), and the mean prompt size per step
// over the runs that reported seat input tokens and ran at least one step. Nearest rank, so every
// figure is a value some run had.
func TestTraceCorpusPrefillDistribution(t *testing.T) {
	var runs []tcRun
	for i := 1; i <= 10; i++ {
		runs = append(runs, tcRun{ID: fmt.Sprintf("m%d", i), Source: tcSourceDelegation, Steps: 2,
			Prefill: &tcPrefill{ObservedSteps: 2, PrefillTokens: int64(i * 100)}})
	}
	// Not measured: observed steps with no token counts, and no prefill at all.
	runs = append(runs, tcRun{ID: "n", Source: tcSourceDelegation, Steps: 2, Prefill: &tcPrefill{ObservedSteps: 2}},
		tcRun{ID: "u", Source: tcSourceDelegation, Steps: 2})
	// Seat input per step: 1000/4 = 250, 900/3 = 300, 100/1 = 100; a run with no step is left out.
	for i, in := range [][2]int64{{1000, 4}, {900, 3}, {100, 1}, {50, 0}} {
		n := in[0]
		runs = append(runs, tcRun{ID: fmt.Sprintf("s%d", i), Source: tcSourceDelegation, Steps: int(in[1]), SeatTokensIn: &n})
	}
	b, err := json.Marshal(tcStatsFor(runs).Prefill)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		PerRun  map[string]*float64 `json:"prefill_tokens_per_run"`
		PerStep map[string]*float64 `json:"seat_input_tokens_per_step_per_run"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := func(name string, d map[string]*float64, runs, p50, p90, max float64) {
		for k, v := range map[string]float64{"runs": runs, "p50": p50, "p90": p90, "max": max} {
			if d[k] == nil || *d[k] != v {
				t.Errorf("%s.%s = %v, want %v (all: %v)", name, k, d[k], v, d)
			}
		}
	}
	want("prefill_tokens_per_run", got.PerRun, 10, 500, 900, 1000)
	want("seat_input_tokens_per_step_per_run", got.PerStep, 3, 250, 300, 300)

	// No measured run: the count is 0 and every figure is null, never a 0.
	b, _ = json.Marshal(tcStatsFor([]tcRun{{ID: "u", Source: tcSourceDelegation, Steps: 1}}).Prefill)
	got.PerRun, got.PerStep = nil, nil
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, d := range []map[string]*float64{got.PerRun, got.PerStep} {
		if d["runs"] == nil || *d["runs"] != 0 || d["p50"] != nil || d["p90"] != nil || d["max"] != nil {
			t.Errorf("an empty distribution must have runs 0 and null figures, got %v", d)
		}
	}
}

// TestTraceCorpusUnrecordedNeverSerializesAsZero is the rule the whole verb exists to keep: a
// value nobody recorded is null, and the counts say how many runs had none. Here every run
// predates the instruments, so every rate must be null and no verdict may be reached.
func TestTraceCorpusUnrecordedNeverSerializesAsZero(t *testing.T) {
	runs := []tcRun{
		{ID: "r1", Source: tcSourceDelegation, Seat: "seat-a", TraceBearing: true, Steps: 5},
		{ID: "r2", Source: tcSourceDelegation, Seat: "seat-a", TraceBearing: true, Steps: 3},
	}
	b, err := tcMarshalReport(tcBuildReport(runs, "h"))
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		All map[string]any `json:"all"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	// rep.All[section] is that section's object.
	rep := struct{ All map[string]map[string]any }{All: map[string]map[string]any{}}
	for _, section := range []string{"pager", "compaction", "prefill"} {
		obj, ok := top.All[section].(map[string]any)
		if !ok {
			t.Fatalf("report has no %s object", section)
		}
		rep.All[section] = obj
	}
	for section, keys := range map[string][]string{
		"pager":      {"refetch_rate"},
		"compaction": {"fire_rate", "passes_per_run", "passes_per_step", "transcript_lower_bound_fire_rate"},
		"prefill":    {"kv_reuse_pct", "avg_prefill_tokens_per_step", "avg_prefill_tokens_per_run"},
	} {
		for _, k := range keys {
			v, present := rep.All[section][k]
			if !present {
				t.Errorf("%s.%s is missing from the report; an unrecorded rate is null, not omitted", section, k)
			} else if v != nil {
				t.Errorf("%s.%s = %v, want null: nothing was recorded", section, k, v)
			}
		}
	}
	if got := rep.All["pager"]["verdict"]; got != tcVerdictNoData {
		t.Errorf("pager verdict = %v, want %s", got, tcVerdictNoData)
	}
	if got := rep.All["pager"]["unrecorded_runs"]; got != float64(2) {
		t.Errorf("pager unrecorded_runs = %v, want 2", got)
	}
	if got := rep.All["compaction"]["unrecorded_runs"]; got != float64(2) {
		t.Errorf("compaction unrecorded_runs = %v, want 2", got)
	}
	if got := rep.All["prefill"]["unmeasured_runs"]; got != float64(2) {
		t.Errorf("prefill unmeasured_runs = %v, want 2", got)
	}

	// The converse: a RECORDED zero is a zero. A run that recorded 0 compaction passes and an
	// unexercised pager moves the fire rate to a real 0 and still reads no verdict on the gate.
	quiet := []tcRun{{ID: "q", Source: tcSourceDelegation, Seat: "seat-a", Steps: 4,
		Pager: &tcPager{Basis: tcBasisInsufficient}}}
	st := tcStatsFor(quiet)
	if st.Compaction.FireRate == nil || *st.Compaction.FireRate != 0 {
		t.Errorf("a recorded run with 0 passes must have fire_rate 0, got %v", st.Compaction.FireRate)
	}
	if st.Pager.RefetchRate != nil || st.Pager.Verdict != tcVerdictNoData || st.Pager.UnexercisedRuns != 1 {
		t.Errorf("an unexercised pager must not produce a rate or a verdict: %+v", st.Pager)
	}
	// Steps observed with no token counts at all (an engine that reports no timings) are not a
	// measured zero: the run is counted apart and the token sums and the reuse rate stay unset.
	nt := tcStatsFor([]tcRun{{ID: "n", Source: tcSourceDelegation, Steps: 3, Prefill: &tcPrefill{ObservedSteps: 3}}})
	if nt.Prefill.MeasuredRuns != 0 || nt.Prefill.NoTokenCountsRuns != 1 || nt.Prefill.KVReusePct != nil || nt.Prefill.AvgPrefillTokensPerStep != nil {
		t.Errorf("a run with observed steps and no token counts must not read as measured: %+v", nt.Prefill)
	}
	// A measured prefill of zero cached tokens is a real 0 % reuse, not null.
	pf := tcStatsFor([]tcRun{{ID: "p", Source: tcSourceDelegation, Steps: 1, Prefill: &tcPrefill{ObservedSteps: 1, PrefillTokens: 100}}})
	if pf.Prefill.KVReusePct == nil || *pf.Prefill.KVReusePct != 0 {
		t.Errorf("measured prefill with nothing cached must read 0 %% reuse, got %v", pf.Prefill.KVReusePct)
	}
}

// TestTraceCorpusPagerGateBoundary: the verdict is the instrument's own rule (below the gate is
// below, the gate itself is not), judged on the pooled rate.
func TestTraceCorpusPagerGateBoundary(t *testing.T) {
	pooled := func(distinct, refetched int) tcPagerStats {
		return tcStatsFor([]tcRun{{ID: "x", Source: tcSourceDelegation, Steps: 1,
			Pager: &tcPager{Basis: tcBasisMeasured, Evictions: distinct, DistinctEvicted: distinct, Refetched: refetched}}}).Pager
	}
	if v := pooled(100, 9).Verdict; v != tcVerdictBelow {
		t.Errorf("9 of 100 = %s, want %s", v, tcVerdictBelow)
	}
	if v := pooled(100, 10).Verdict; v != tcVerdictAbove {
		t.Errorf("10 of 100 (the gate itself) = %s, want %s: the instrument treats `rate < gate` as below", v, tcVerdictAbove)
	}
	if got := pooled(100, 0).RefetchRate; got == nil || *got != 0 {
		t.Errorf("a measured run that re-fetched nothing is a real 0, got %v", got)
	}
}

// TestTraceCorpusLoaderIsStrict: a typo or a duplicate is an error, never a silent miss.
func TestTraceCorpusLoaderIsStrict(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	good := `{"id":"a","source":"agent-trace","seat":"","trace_bearing":true,"steps":1,"pager":null,"prefill":null,"ladder_artifacts":null}`
	cases := map[string]struct{ body, want string }{
		"unknown key":           {strings.Replace(good, `"steps":1`, `"stepz":1`, 1) + "\n", "unknown field"},
		"duplicate id":          {good + "\n" + good + "\n", "duplicate id"},
		"unknown source":        {strings.Replace(good, `agent-trace`, `elsewhere`, 1) + "\n", "unknown source"},
		"negative steps":        {strings.Replace(good, `"steps":1`, `"steps":-1`, 1) + "\n", "negative steps"},
		"empty":                 {"\n", "no runs"},
		"zero seat tokens":      {strings.Replace(good, `"ladder_artifacts":null`, `"ladder_artifacts":null,"seat_tokens_in":0`, 1) + "\n", "seat_tokens_in must be positive"},
		"inconsistent pager":    {strings.Replace(good, `"pager":null`, `"pager":{"basis":"measured","evictions":0,"evicted_bytes":0,"distinct_evicted":0,"refetched_distinct":0,"compaction_passes":0}`, 1) + "\n", "pager object is inconsistent"},
		"prefill with no steps": {strings.Replace(good, `"prefill":null`, `"prefill":{"observed_steps":0,"prefill_tokens":5,"cache_tokens":0}`, 1) + "\n", "prefill object is inconsistent"},
	}
	for name, c := range cases {
		_, _, err := tcLoadCorpus(write(strings.ReplaceAll(name, " ", "_")+".jsonl", c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
		}
	}
}

// TestTraceCorpusVerbArguments covers the refusals at the edge.
func TestTraceCorpusVerbArguments(t *testing.T) {
	tmp := t.TempDir()
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no mode":                {nil, "usage:"},
		"unknown mode":           {[]string{"bogus"}, "unknown mode"},
		"harvest without out":    {[]string{"harvest", "--traces", tcFixtureTraces}, "--out is required"},
		"readout without corpus": {[]string{"readout"}, "--corpus is required"},
		"check without baseline": {[]string{"check"}, "--corpus is required"},
		"stray argument":         {[]string{"readout", "--corpus", "x", "extra"}, "unexpected argument"},
		"not a directory":        {[]string{"harvest", "--traces", filepath.Join(tmp, "absent"), "--out", filepath.Join(tmp, "o")}, "not a directory"},
		"nothing usable":         {[]string{"harvest", "--traces", tmp, "--out", filepath.Join(tmp, "o")}, "no usable runs"},
	} {
		if _, _, err := tcRunVerb(t, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "o")); err == nil {
		t.Errorf("a refused harvest left a corpus behind")
	}
	corpus := tcHarvestFixture(t, "args.jsonl")
	if _, _, err := tcRunVerb(t, "freeze", "--corpus", corpus); err == nil || !strings.Contains(err.Error(), "--out is required") {
		t.Errorf("freeze without --out: err = %v", err)
	}
	if _, _, err := tcRunVerb(t, "check", "--corpus", corpus); err == nil || !strings.Contains(err.Error(), "--baseline is required") {
		t.Errorf("check without --baseline: err = %v", err)
	}
}

// TestTraceCorpusIsRegisteredInMain: the verb is reachable. main.go carries exactly one case
// line for it and one usage line, nothing else.
func TestTraceCorpusIsRegisteredInMain(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, `case "trace-corpus":`); n != 1 {
		t.Fatalf("main.go has %d `case \"trace-corpus\":` lines, want 1", n)
	}
	if !strings.Contains(src, "err = runTraceCorpus(args)") {
		t.Fatalf("the trace-corpus case does not call runTraceCorpus")
	}
	if n := strings.Count(src, "local-offload trace-corpus "); n != 1 {
		t.Fatalf("main.go usage has %d trace-corpus lines, want 1", n)
	}
}

// TestTraceCorpusFixturesAreSynthetic: the fixtures carry no host, path or identity text. The
// banned words are assembled at run time (and a drive path is a pattern) so this file never holds
// one itself: a literal here would be the one place the text reached a tracked file.
func TestTraceCorpusFixturesAreSynthetic(t *testing.T) {
	drivePath := regexp.MustCompile(`(^|[^A-Za-z0-9])[A-Za-z]:[\\/]`)
	banned := []string{"/" + "home/", "/" + "Users/", "@", "Bearer ", "dev" + "-caches", "work" + "trees", "My" + " Drive"}
	err := filepath.Walk("testdata/tracecorpus", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		for _, bad := range banned {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		if m := drivePath.Find(b); m != nil {
			t.Errorf("%s contains a drive path (%q)", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The patterns bite: a planted drive path is caught, and the legitimate shapes are not.
	for in, want := range map[string]bool{
		"Q" + ":" + "/x": true, "Q" + ":" + `\x`: true, "see " + "q" + ":" + "/x": true,
		"https://example.invalid": false, `"Role":"user"`: false, "a:b": false,
	} {
		if got := drivePath.MatchString(in); got != want {
			t.Errorf("drive pattern on %q = %v, want %v", in, got, want)
		}
	}
}

// TestTraceCorpusHarvestIsLineEndingBlind: the same records checked out with CRLF line endings
// (a Windows checkout of the fixtures, or a log copied through an editor) harvest to the same
// corpus bytes.
func TestTraceCorpusHarvestIsLineEndingBlind(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"traces", "delegation-log"} {
		src := filepath.Join("testdata", "tracecorpus", sub)
		dst := filepath.Join(root, sub)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			b = bytes.ReplaceAll(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
			if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	crlf := filepath.Join(t.TempDir(), "crlf.jsonl")
	if _, _, err := tcRunVerb(t, "harvest", "--traces", filepath.Join(root, "traces"), "--delegation-log", filepath.Join(root, "delegation-log"), "--out", crlf); err != nil {
		t.Fatalf("harvest over CRLF records: %v", err)
	}
	lf := tcHarvestFixture(t, "lf.jsonl")
	a, _ := os.ReadFile(crlf)
	b, _ := os.ReadFile(lf)
	if !bytes.Equal(a, b) {
		t.Fatalf("CRLF records harvested to a different corpus:\n%s\n---\n%s", a, b)
	}
}

// TestTraceCorpusHarvestDefaultsToTheInstallRoot: with no source flag, harvest reads the
// agent-traces and delegation-log directories under the configured install root (`home`), the
// same two places the writers use. A root with neither is refused, naming what to pass.
func TestTraceCorpusHarvestDefaultsToTheInstallRoot(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfgJSON, _ := json.Marshal(map[string]string{"home": home})
	if err := os.WriteFile(cfgPath, cfgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "corpus.jsonl")
	if _, _, err := tcRunVerb(t, "harvest", "--config", cfgPath, "--out", out); err == nil || !strings.Contains(err.Error(), "pass --traces and/or --delegation-log") {
		t.Fatalf("an install root with no records must be refused with the flags to pass, got %v", err)
	}
	copyDir := func(src, dst string) {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	copyDir(tcFixtureTraces, filepath.Join(home, "agent-traces"))
	copyDir(tcFixtureDelegation, filepath.Join(home, "delegation-log"))
	if _, _, err := tcRunVerb(t, "harvest", "--config", cfgPath, "--out", out); err != nil {
		t.Fatalf("harvest from the install root: %v", err)
	}
	got, _ := os.ReadFile(out)
	want, _ := os.ReadFile(tcHarvestFixture(t, "explicit.jsonl"))
	if !bytes.Equal(got, want) {
		t.Fatalf("the install-root harvest differs from the explicit one:\n%s\n---\n%s", got, want)
	}
}
