package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// Tests for the trace-corpus verb (RSI-OH-2). The fixtures under testdata/tracecorpus are
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
// run that reached the loop always carries, zeros included).
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
	if stats.DelegationRows != 9 || len(runs) != 6 || stats.Runs != 6 {
		t.Fatalf("rows/runs = %d/%d, want 9 rows and 6 runs: %v", stats.DelegationRows, len(runs), stats.Skipped)
	}
	if stats.Skipped[tcWantNeverRan] != 3 {
		t.Errorf("skipped[%q] = %d, want 3 (all: %v)", tcWantNeverRan, stats.Skipped[tcWantNeverRan], stats.Skipped)
	}
	// None of the six runs recorded a pager object but one: the unrecorded count is now a count
	// of RUNS, 5, not of rows.
	if got := tcStatsFor(runs).Pager.UnrecordedRuns; got != 5 {
		t.Errorf("pager unrecorded runs = %d, want 5 of the 6 runs", got)
	}
	// A seat filter that rejects the row speaks first: the reason is the filter's, not never-ran.
	_, stats, err = tcHarvest(tcHarvestOpts{DelegationDir: dir, Seats: map[string]bool{"seat-b": true}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped[tcWantNeverRan] != 0 || stats.Skipped["seat filter: seat not selected"] != 9 {
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
	dir := t.TempDir()
	tracesDir := filepath.Join(dir, "traces")
	logDir := filepath.Join(dir, "log")
	os.MkdirAll(tracesDir, 0o755)
	os.MkdirAll(logDir, 0o755)
	// The only free strings a run keeps are its file-derived id, seat and stop reason.
	rec := `{"id":"x","stop_reason":"` + email + `","steps":1,"transcript":[{"Role":"user","Content":"hi"}]}`
	os.WriteFile(filepath.Join(tracesDir, "run-"+email+".json"), []byte(rec), 0o644)
	row := `{"seat":"` + key + `","result":{"steps":2,"stop_reason":"done","trace":[{}]}}` + "\n"
	os.WriteFile(filepath.Join(logDir, "2026-02-03.jsonl"), []byte(row), 0o644)

	out := filepath.Join(dir, "corpus.jsonl")
	stdout, _, err := tcRunVerb(t, "harvest", "--traces", tracesDir, "--delegation-log", logDir, "--out", out)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	b, _ := os.ReadFile(out)
	for _, secret := range []string{email, key} {
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
	if harvested.Stats.Redactions["email"] < 2 || harvested.Stats.Redactions["github-token"] != 1 {
		t.Errorf("redaction counts = %v, want the email twice (id and stop reason) and the token once", harvested.Stats.Redactions)
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
	// Defense in depth: the strict loader refuses a corpus that carries a path seat.
	dirty := filepath.Join(dir, "dirty.jsonl")
	line := `{"id":"dl-x","source":"delegation-log","seat":` + strconvQuote(rooted) + `,"trace_bearing":false,"steps":1,"pager":null,"prefill":null,"seat_tokens_in":null,"ladder_artifacts":null}` + "\n"
	if err := os.WriteFile(dirty, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tcRunVerb(t, "readout", "--corpus", dirty); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("a corpus holding a path seat must be refused, got %v", err)
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

// TestTraceCorpusFixturesAreSynthetic: the fixtures carry no host, path or identity text.
func TestTraceCorpusFixturesAreSynthetic(t *testing.T) {
	banned := []string{"C:\\", "D:\\", "G:\\", "C:/", "D:/", "G:/", "/home/", "/Users/", "@", "Bearer ", "dev-caches", "worktrees"}
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
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
