// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/cliutil/testenv"
)

func benchWant(t *testing.T, label, got string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(got, n) {
			t.Errorf("%s does not mention %q\ngot: %s", label, n, got)
		}
	}
}

// ---------------------------------------------------------------------------
// dispersion
// ---------------------------------------------------------------------------

// The n-1 denominator is the point. With n, this sample's stddev is 2.0; the
// sample stddev is 2.236. Using n understates the spread, which is the
// direction that makes a noisy bench look repeatable.
func TestBenchStatsUsesTheSampleStddev(t *testing.T) {
	s := benchStats([]float64{10, 12, 14}, "tg")
	if s.N != 3 {
		t.Fatalf("N = %d", s.N)
	}
	if math.Abs(s.Mean-12) > 1e-9 {
		t.Errorf("Mean = %v, want 12", s.Mean)
	}
	if math.Abs(s.Median-12) > 1e-9 {
		t.Errorf("Median = %v, want 12", s.Median)
	}
	want := 2.0
	if got := s.Stddev; math.Abs(got-want) > 1e-9 {
		t.Errorf("Stddev = %v, want the SAMPLE stddev %v (population would be %.4f)", got, want, math.Sqrt(8.0/3.0))
	}
	if s.Min != 10 || s.Max != 14 {
		t.Errorf("Min/Max = %v/%v", s.Min, s.Max)
	}
	// 2 / 12 = 16.7% > 3%, so this run is unstable.
	if !s.Unstable {
		t.Errorf("a %.1f%% spread must be flagged unstable", s.CoefVarPct)
	}
	benchWant(t, "instability note", s.Note, "UNSTABLE", "tg", "10.0-14.0")
}

func TestBenchStatsTightRunIsStable(t *testing.T) {
	s := benchStats([]float64{100, 101, 100.5}, "pp")
	if s.Unstable {
		t.Errorf("a %.2f%% spread was flagged unstable: %s", s.CoefVarPct, s.Note)
	}
	if s.Note != "" {
		t.Errorf("unexpected note on a stable run: %s", s.Note)
	}
	if !strings.Contains(s.String(), "±") {
		t.Errorf("String() = %q, want a mean ± stddev form", s.String())
	}
}

// A single run has NO measurable dispersion. Reporting stddev 0 would claim
// perfect repeatability from one observation.
func TestBenchStatsSingleRunClaimsNoRepeatability(t *testing.T) {
	s := benchStats([]float64{42}, "tg")
	if s.Stddev != 0 || s.Unstable {
		t.Errorf("single run produced stddev %v unstable=%v", s.Stddev, s.Unstable)
	}
	benchWant(t, "single-run note", s.Note, "no dispersion is measurable", "--runs 3")
	if !strings.Contains(s.String(), "n=1") {
		t.Errorf("String() = %q; a single run must not render as mean ± 0", s.String())
	}
}

func TestBenchStatsEmptyIsNotZeroThroughput(t *testing.T) {
	s := benchStats(nil, "pp")
	if s.N != 0 || s.Mean != 0 {
		t.Fatalf("%+v", s)
	}
	benchWant(t, "empty note", s.Note, "no successful pp samples")
	if s.String() != "n/a" {
		t.Errorf("String() = %q, want n/a rather than a 0.00 rate", s.String())
	}
}

// ---------------------------------------------------------------------------
// depth
// ---------------------------------------------------------------------------

func TestParseDepths(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"", []int{0}},
		{"0", []int{0}},
		{"0,4096", []int{0, 4096}},
		{"4096, 0 ,1024", []int{0, 1024, 4096}},
	}
	for _, c := range cases {
		got, err := parseDepths(c.in)
		if err != nil {
			t.Fatalf("parseDepths(%q): %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseDepths(%q) = %v, want %v (ascending)", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"abc", "-1", "0,x"} {
		if _, err := parseDepths(bad); err == nil {
			t.Errorf("parseDepths(%q) accepted a non-depth", bad)
		}
	}
}

// At depth 0 the prompt cache stays OFF (the fantasy-PP trap). At a non-zero
// depth it MUST be on, or the prefill the measurement depends on is discarded
// and the run silently reports depth-0 numbers under a depth-N label.
func TestBenchRequestSwitchesPromptCacheOnOnlyForDepth(t *testing.T) {
	var seen map[string]any
	flags, closeFn := benchStub(t, func(body map[string]any) any {
		seen = body
		return map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
			"timings": map[string]any{
				"cache_n": 4000, "prompt_n": 320, "prompt_ms": 200.0, "prompt_per_second": 1600.0,
				"predicted_n": 128, "predicted_ms": 1000.0, "predicted_per_second": 128.0,
			},
		}
	})
	defer closeFn()

	run := benchRequest(context.Background(), flags, "m", benchPrompt, "FILLER TEXT", 128, true, 10*time.Second)
	if run.Error != "" {
		t.Fatalf("request failed: %s", run.Error)
	}
	if cache, _ := seen["cache_prompt"].(bool); !cache {
		t.Error(`a --depth run must send "cache_prompt": true or the prefill it depends on is thrown away`)
	}
	msgs, _ := seen["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(content, "FILLER TEXT") {
		t.Error("the prefill must be a PREFIX of the measured prompt; otherwise the cached tokens are not reused")
	}
	if !strings.HasSuffix(content, benchPrompt) {
		t.Error("the measured prompt must remain the fixed benchmark prompt, appended after the prefill")
	}
	// cache_n is what makes the depth checkable rather than assumed.
	if run.CacheN != 4000 {
		t.Errorf("cache_n = %d, want the server's 4000", run.CacheN)
	}
	if run.PromptN != 320 {
		t.Errorf("prompt_n = %d; only the NEW tokens are timed at depth", run.PromptN)
	}
}

func TestBenchDepthVerdictCallsOutAPrefillThatDidNotStick(t *testing.T) {
	d := &benchDepthResult{DepthRequested: 4096, DepthActual: 4100, DepthObserved: 0}
	benchWant(t, "failed-prefill verdict", benchDepthVerdict(d),
		"REQUESTED depth 4096", "measured at depth 0", "Do not quote it as a deep-context rate")

	partial := &benchDepthResult{DepthRequested: 4096, DepthActual: 4100, DepthObserved: 1000}
	benchWant(t, "partial verdict", benchDepthVerdict(partial), "PARTIAL depth", "1000", "effective depth is the observed number")

	ok := &benchDepthResult{DepthRequested: 4096, DepthActual: 4100, DepthObserved: 4100}
	benchWant(t, "reached verdict", benchDepthVerdict(ok), "depth reached", "excluded from the timed window")

	// d0 must SAY that it is the flattering end of the curve.
	benchWant(t, "d0 verdict", benchDepthVerdict(&benchDepthResult{}), "depth 0", "OVERSTATES")
}

// ---------------------------------------------------------------------------
// comparability key
// ---------------------------------------------------------------------------

const benchTestSeat = `C:/llama.cpp-b10356/llama-server.exe --port 9200 --host 127.0.0.1 ` +
	`-m V:/models/demo-Q4_K_M.gguf --ctx-size 8192 --batch-size 4096 --ubatch-size 4096 --parallel 4 -ngl 99`

func benchTestHost() benchHostInfo {
	return benchHostInfo{
		CPU: "Intel(R) Core(TM) i9-10980XE", CPUCores: 18, CPUThreads: 36,
		OS: "Microsoft Windows 11 Pro 25H2", Arch: "x86_64",
		SwapVer: "v249", SwapCommit: "f94c94a", GPUs: "RTX 5060 Ti + RTX 5070 Ti",
	}
}

func TestComparabilityKeyIsStableForAnIdenticalConfiguration(t *testing.T) {
	a := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "V:/models/demo-Q4_K_M.gguf", "Q4_K_M", 1234, 5678, benchTestHost())
	b := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "V:/models/demo-Q4_K_M.gguf", "Q4_K_M", 1234, 5678, benchTestHost())
	if a.SHA != b.SHA {
		t.Fatalf("the same configuration produced two shas: %s vs %s", a.SHA, b.SHA)
	}
	if a.SHA == "" {
		t.Fatal("no comparability sha was produced")
	}
	if a.Observed == 0 || a.Total < 28 {
		t.Errorf("key covers %d of %d fields; the llama-bench-style contract is a broad property set", a.Observed, a.Total)
	}
	benchWant(t, "contract", a.Contract, "LLAMA_BENCH_KEY_PROPERTIES", "observed=false")
	// The build string must be split into number and commit, not stored whole.
	if got := benchFieldValue(a, "build_number"); got != "b10356" {
		t.Errorf("build_number = %q, want b10356", got)
	}
	if got := benchFieldValue(a, "build_commit"); got != "0666ad2b2" {
		t.Errorf("build_commit = %q, want 0666ad2b2", got)
	}
}

// The whole point of the key: any flag that moves a number must move the sha.
func TestComparabilityKeyChangesWhenASeatFlagChanges(t *testing.T) {
	host := benchTestHost()
	base := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, host)
	for _, mutated := range []struct{ name, seat string }{
		{"batch size", strings.Replace(benchTestSeat, "--batch-size 4096", "--batch-size 2048", 1)},
		{"context", strings.Replace(benchTestSeat, "--ctx-size 8192", "--ctx-size 4096", 1)},
		{"slots", strings.Replace(benchTestSeat, "--parallel 4", "--parallel 1", 1)},
		{"gpu layers", strings.Replace(benchTestSeat, "-ngl 99", "-ngl 20", 1)},
		{"kv cache dtype", benchTestSeat + " -ctk q8_0"},
		{"flash attention", benchTestSeat + " -fa on"},
		{"mmap", benchTestSeat + " --no-mmap"},
	} {
		k := buildComparabilityKey(mutated.seat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, host)
		if k.SHA == base.SHA {
			t.Errorf("changing the %s left the comparability sha unchanged; rows would be diffed across a config change", mutated.name)
		}
		if diff := diffKeys(base, k); len(diff) == 0 {
			t.Errorf("changing the %s produced no named field difference", mutated.name)
		}
	}
	// A different llama.cpp build is a different machine-as-configured.
	if k := buildComparabilityKey(benchTestSeat, "b10400-deadbeef", "m.gguf", "Q4_K_M", 1, 2, host); k.SHA == base.SHA {
		t.Error("a different llama.cpp build left the sha unchanged")
	}
	// So is a different host.
	other := host
	other.GPUs = "RTX 3070"
	if k := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, other); k.SHA == base.SHA {
		t.Error("a different GPU left the sha unchanged")
	}
}

// A row measured with a field missing is not interchangeable with one where
// it was read, even if every other field matches.
func TestComparabilityKeyDistinguishesUnobservedFields(t *testing.T) {
	full := benchTestHost()
	blind := full
	blind.GPUs = ""
	a := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, full)
	b := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, blind)
	if a.SHA == b.SHA {
		t.Fatal("a key with an unobserved gpu_info hashed identically to one that read it")
	}
	if len(b.Unobserved) == 0 || !containsStr(b.Unobserved, "gpu_info") {
		t.Errorf("Unobserved = %v, want gpu_info named", b.Unobserved)
	}
	diff := diffKeys(a, b)
	joined := strings.Join(diff, " | ")
	benchWant(t, "key diff", joined, "gpu_info", "<unobserved>")
}

// An absent flag is the server DEFAULT, which is part of the configuration -
// not an unobservable property.
func TestComparabilityKeyTreatsAnAbsentFlagAsTheDefault(t *testing.T) {
	k := buildComparabilityKey(benchTestSeat, "b1-c1", "m.gguf", "Q4_K_M", 1, 2, benchTestHost())
	if got := benchFieldValue(k, "type_k"); got != "(default)" {
		t.Errorf("type_k = %q, want (default) for a seat with no -ctk", got)
	}
	if containsStr(k.Unobserved, "type_k") {
		t.Error("an absent seat flag must not be reported as unobservable")
	}
	if got := benchFieldValue(k, "no_mmap"); got != "0" {
		t.Errorf("no_mmap = %q, want 0 for a seat without --no-mmap", got)
	}
	if got := benchFieldValue(k, "n_parallel"); got != "4" {
		t.Errorf("n_parallel = %q, want the seat's 4", got)
	}
}

func benchFieldValue(k benchComparabilityKey, name string) string {
	for _, f := range k.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return "<missing>"
}

func containsStr(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// significance
// ---------------------------------------------------------------------------

func TestBenchDeltaRefusesToCallNoiseARegression(t *testing.T) {
	// 2% apart, each wobbling 4%: inside the combined spread.
	d := benchMakeDelta("tg", 100, 4, 98, 4)
	if d.Significant {
		t.Errorf("a 2%% change between two rows that each wobble 4%% was called significant: %s", d.Note)
	}
	benchWant(t, "noise note", d.Note, "INSIDE", "not distinguishable from noise")

	// 30% apart with 1% wobble: a real finding.
	big := benchMakeDelta("tg", 100, 1, 70, 1)
	if !big.Significant {
		t.Errorf("a 30%% drop with 1%% spread was dismissed as noise: %s", big.Note)
	}
	if big.DeltaPct > -29 || big.DeltaPct < -31 {
		t.Errorf("DeltaPct = %v, want about -30", big.DeltaPct)
	}
	benchWant(t, "significant note", big.Note, "clears the")

	// No recorded dispersion means the delta is unqualified, not proven.
	none := benchMakeDelta("tg", 100, 0, 90, 0)
	benchWant(t, "no-stddev note", none.Note, "no noise floor to clear", "unqualified")
}

// ---------------------------------------------------------------------------
// canonical row
// ---------------------------------------------------------------------------

func TestStandardRowCarriesBuildHardwareAndProvenance(t *testing.T) {
	host := benchTestHost()
	m := &benchModelResult{
		Model: "gemma-4-e2b", ModelQuant: "Q4_K_M", ModelSizeBytes: 3 * 1024 * 1024 * 1024,
		ModelParams: 2_600_000_000, BuildInfo: "b10356-0666ad2b2",
		Key: buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, host),
		Depths: []benchDepthResult{{
			DepthRequested: 0,
			PP:             benchStats([]float64{1600, 1610, 1590}, "pp"),
			TG:             benchStats([]float64{128, 129, 127}, "tg"),
		}},
	}
	row := benchStandardRow(m, host)
	benchWant(t, "standard row", row,
		"| model | size | params | backend | ngl | test | t/s |",
		"pp512", "tg128", "±",
		"3.00 GiB", "2.60B", "99",
		"b10356-0666ad2b2", "llama-swap v249",
		"Intel(R) Core(TM) i9-10980XE",
		"comparability_sha:",
		// The honesty footnote: this is not a llama-bench run.
		"NOT byte-identical to a `llama-bench` run")
	if strings.Contains(row, "| pp512 |  |") {
		t.Error("the row rendered an empty rate cell")
	}
}

func TestStandardRowLabelsDepthedRows(t *testing.T) {
	host := benchTestHost()
	m := &benchModelResult{
		Model: "m", BuildInfo: "b1-c1",
		Key: buildComparabilityKey(benchTestSeat, "b1-c1", "m.gguf", "Q4_K_M", 1, 2, host),
		Depths: []benchDepthResult{
			{DepthRequested: 0, PP: benchStats([]float64{100, 101}, "pp"), TG: benchStats([]float64{10, 10.1}, "tg")},
			{DepthRequested: 4096, PP: benchStats([]float64{60, 61}, "pp"), TG: benchStats([]float64{7, 7.1}, "tg")},
		},
	}
	row := benchStandardRow(m, host)
	benchWant(t, "depthed standard row", row, "pp512 @ d4096", "tg128 @ d4096")
}

// ---------------------------------------------------------------------------
// compare
// ---------------------------------------------------------------------------

func TestBenchKeyDiffNamesTheDifferingFields(t *testing.T) {
	host := benchTestHost()
	a := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, host)
	b := buildComparabilityKey(strings.Replace(benchTestSeat, "-ngl 99", "-ngl 20", 1), "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, host)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	diff := benchKeyDiffFromJSON(string(aj), string(bj))
	joined := strings.Join(diff, " | ")
	benchWant(t, "key diff", joined, "n_gpu_layers", "99", "20")

	// A missing stored key must say so rather than return a reassuring
	// empty diff.
	benchWant(t, "absent key diff", strings.Join(benchKeyDiffFromJSON("", string(bj)), " "),
		"carries no stored comparability key")
}

// End-to-end through the command: two rows with different keys must be
// refused with the typed exit code and the differing field named.
func TestBenchCompareRefusesRowsWithDifferentKeys(t *testing.T) {
	testenv.Isolate(t, cliutil.StateDir, cliutil.DataDir)
	ctx := context.Background()
	s, err := mcOpenDomainStore(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	host := benchTestHost()
	k1 := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, host)
	k2 := buildComparabilityKey(strings.Replace(benchTestSeat, "-ngl 99", "-ngl 20", 1), "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, host)
	for i, k := range []benchComparabilityKey{k1, k2} {
		if _, err := s.DB().ExecContext(ctx,
			`INSERT INTO bench_runs (ts, model, kv_depth, pp_mean, pp_stddev, tg_mean, tg_stddev, runs, comparability_sha, comparability_key)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			mcNow(), "demo", 0, 100.0+float64(i), 1.0, 10.0+float64(i), 0.1, 3, k.SHA, benchKeyJSON(k)); err != nil {
			t.Fatalf("seeding row %d: %v", i, err)
		}
	}
	s.Close()

	out, err := runCLI(t, "bench", "compare", "demo", "--json")
	if err == nil {
		t.Fatal("comparing two rows with different comparability keys must not succeed")
	}
	if code := ExitCode(err); code != ExitNotComparable {
		t.Errorf("exit code = %d, want ExitNotComparable (%d); output: %s", code, ExitNotComparable, out)
	}
	benchWant(t, "refusal output", out, `"comparable": false`, "n_gpu_layers", "comparability_sha differs")
}

// Two rows from the SAME configuration must compare, and a change inside the
// noise floor must not be published as a regression.
func TestBenchCompareDiffsRowsWithMatchingKeys(t *testing.T) {
	testenv.Isolate(t, cliutil.StateDir, cliutil.DataDir)
	ctx := context.Background()
	s, err := mcOpenDomainStore(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	k := buildComparabilityKey(benchTestSeat, "b10356-0666ad2b2", "m.gguf", "Q4_K_M", 1, 2, benchTestHost())
	rows := []struct{ pp, ppSD, tg, tgSD float64 }{
		{100, 4, 10, 0.4},
		{98, 4, 5, 0.1},
	}
	for _, r := range rows {
		if _, err := s.DB().ExecContext(ctx,
			`INSERT INTO bench_runs (ts, model, kv_depth, pp_mean, pp_stddev, tg_mean, tg_stddev, runs, comparability_sha, comparability_key)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			mcNow(), "demo", 0, r.pp, r.ppSD, r.tg, r.tgSD, 3, k.SHA, benchKeyJSON(k)); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	s.Close()

	out, err := runCLI(t, "bench", "compare", "demo", "--json")
	if err != nil {
		t.Fatalf("matching keys must compare: %v (output %s)", err, out)
	}
	var rep benchCompareReport
	if uerr := json.Unmarshal([]byte(out), &rep); uerr != nil {
		t.Fatalf("output is not the report envelope: %v\n%s", uerr, out)
	}
	if !rep.Comparable {
		t.Fatalf("rows sharing a key were refused: %s", rep.Refusal)
	}
	if len(rep.Deltas) != 2 {
		t.Fatalf("expected pp and tg deltas, got %+v", rep.Deltas)
	}
	byMetric := map[string]benchDelta{}
	for _, d := range rep.Deltas {
		byMetric[d.Metric] = d
	}
	if pp := byMetric["pp_tokens_per_second"]; pp.Significant {
		t.Errorf("a 2%% pp change inside a 4%% spread was called significant: %s", pp.Note)
	}
	if tg := byMetric["tg_tokens_per_second"]; !tg.Significant {
		t.Errorf("a 50%% tg drop was not called significant: %s", tg.Note)
	}
}

func TestBenchCompareWithNoRowsIsTyped(t *testing.T) {
	testenv.Isolate(t, cliutil.StateDir, cliutil.DataDir)
	_, err := runCLI(t, "bench", "compare", "never-benched", "--json")
	if err == nil {
		t.Fatal("comparing a model with no rows must fail")
	}
	if code := ExitCode(err); code != ExitModelNotFound {
		t.Errorf("exit code = %d, want %d", code, ExitModelNotFound)
	}
}
