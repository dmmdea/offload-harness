package seatload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real vLLM 0.29 exposition (the ampere-16 27B reference seat, 2026-09-29 23:35, four
// requests running, KV 0.69, 29 preemptions), trimmed to the series the
// reader uses.
func TestParseEngineMetricsReadsARealVLLMExposition(t *testing.T) {
	body, err := os.ReadFile("testdata/vllm-0.29-metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	var act Activity
	if err := parseEngineMetrics(body, &act); err != nil {
		t.Fatal(err)
	}
	if act.ActSource != "vllm-metrics" || act.Running != 4 || act.Waiting != 0 {
		t.Fatalf("source/running/waiting = %s/%d/%d, want vllm-metrics/4/0", act.ActSource, act.Running, act.Waiting)
	}
	if act.Preemptions != 29 || act.GenTokens != 86465 || act.PromptTokens != 1504762 {
		t.Fatalf("preemptions/gen/prompt = %v/%v/%v", act.Preemptions, act.GenTokens, act.PromptTokens)
	}
	if !strings.HasPrefix(act.Fingerprint, "v|23305|86465|1504762|29|0.69444") {
		t.Fatalf("fingerprint = %q", act.Fingerprint)
	}
	if act.TokenFingerprint != "vt|86465|1504762" || act.KVGaugeMissing {
		t.Fatalf("token fingerprint = %q, kv missing = %v", act.TokenFingerprint, act.KVGaugeMissing)
	}
}

// The fingerprint is WORK: an arrival (a new waiting request) must not change
// it — a wedged engine core whose front end still accepts requests would
// otherwise look alive forever — while any counter or the KV gauge moving
// must.
func TestFingerprintMovesWithWorkNeverWithArrivals(t *testing.T) {
	base := "vllm:num_requests_running 1\nvllm:num_requests_waiting %W\nvllm:iteration_tokens_total_count %S\n" +
		"vllm:generation_tokens_total 500\nvllm:prompt_tokens_total 9000\nvllm:kv_cache_usage_perc %K\n"
	read := func(waiting, steps, kv string) Activity {
		var a Activity
		s := strings.NewReplacer("%W", waiting, "%S", steps, "%K", kv).Replace(base)
		if err := parseEngineMetrics([]byte(s), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	idle := read("0", "100", "0.10")
	if read("7", "100", "0.10").Fingerprint != idle.Fingerprint {
		t.Fatal("seven arrivals changed the fingerprint: a front end accepting requests is not engine work")
	}
	if read("0", "101", "0.10").Fingerprint == idle.Fingerprint {
		t.Fatal("an engine step did not change the fingerprint")
	}
	// A solo chunked prefill emits no token and no iteration stats; only the
	// KV blocks it allocates chunk by chunk show the engine working.
	if read("0", "100", "0.13").Fingerprint == idle.Fingerprint {
		t.Fatal("KV growth during a solo prefill did not change the fingerprint")
	}
}

// An aborted request (the front end's own event — our killed siblings) must
// not move the work fingerprint: on a wedged engine it would restart every
// other run's flat clock. A preempt/recompute thrash moves the work
// fingerprint but never the TOKEN fingerprint, which is what the monitor's
// token bound watches.
func TestAbortsAndThrashDoNotLookLikeProduction(t *testing.T) {
	base := "vllm:iteration_tokens_total_count %S\nvllm:generation_tokens_total %G\nvllm:prompt_tokens_total 9000\n" +
		"vllm:num_preemptions_total %P\nvllm:kv_cache_usage_perc %K\nvllm:request_success_total{finished_reason=\"abort\"} %A\n"
	read := func(steps, gen, pre, kv, abort string) Activity {
		var a Activity
		s := strings.NewReplacer("%S", steps, "%G", gen, "%P", pre, "%K", kv, "%A", abort).Replace(base)
		if err := parseEngineMetrics([]byte(s), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	idle := read("100", "500", "3", "0.40", "0")
	if read("100", "500", "3", "0.40", "5").Fingerprint != idle.Fingerprint {
		t.Fatal("five aborts moved the work fingerprint")
	}
	thrash := read("140", "500", "9", "0.55", "0")
	if thrash.Fingerprint == idle.Fingerprint {
		t.Fatal("engine steps and preemptions must move the work fingerprint")
	}
	if thrash.TokenFingerprint != idle.TokenFingerprint {
		t.Fatal("a thrash that produced no token moved the token fingerprint")
	}
	if read("141", "501", "9", "0.55", "0").TokenFingerprint == idle.TokenFingerprint {
		t.Fatal("a generated token must move the token fingerprint")
	}
}

// A vLLM exposition with no KV-usage gauge is named: a solo prefill (no token,
// no iteration stats) cannot move the fingerprint there.
func TestMissingKVGaugeIsNamed(t *testing.T) {
	var a Activity
	if err := parseEngineMetrics([]byte("vllm:iteration_tokens_total_count 1\nvllm:generation_tokens_total 2\nvllm:prompt_tokens_total 3\n"), &a); err != nil {
		t.Fatal(err)
	}
	a.Loaded = true
	if !a.KVGaugeMissing || !strings.Contains(a.Summary(), "no KV-usage gauge") {
		t.Fatalf("kv missing = %v, summary = %q", a.KVGaugeMissing, a.Summary())
	}
}

func TestParseEngineMetricsReadsLlamaServer(t *testing.T) {
	body := "# HELP llamacpp:prompt_tokens_total x\n# TYPE llamacpp:prompt_tokens_total counter\nllamacpp:prompt_tokens_total 4096\n" +
		"llamacpp:tokens_predicted_total 812\nllamacpp:n_decode_total 77\nllamacpp:requests_processing 2\nllamacpp:requests_deferred 1\n"
	var a Activity
	if err := parseEngineMetrics([]byte(body), &a); err != nil {
		t.Fatal(err)
	}
	if a.ActSource != "llamacpp-metrics" || a.Running != 2 || a.Waiting != 1 || a.Fingerprint != "l|77|812|4096" || a.TokenFingerprint != "lt|812|4096" {
		t.Fatalf("activity = %+v", a)
	}
	var b Activity
	if err := parseEngineMetrics([]byte(strings.Replace(body, "n_decode_total 77", "n_decode_total 78", 1)), &b); err != nil {
		t.Fatal(err)
	}
	if b.Fingerprint == a.Fingerprint {
		t.Fatal("a llama_decode() call (a prompt batch included) must change the fingerprint")
	}
}

// An exposition with no engine work counter is an error, never an idle engine.
func TestParseEngineMetricsRefusesAnExpositionWithoutWorkCounters(t *testing.T) {
	var a Activity
	if err := parseEngineMetrics([]byte("process_cpu_seconds_total 12\nvllm:num_requests_running 3\n"), &a); err == nil {
		t.Fatalf("no work counter must be an error, got fingerprint %q", a.Fingerprint)
	}
}

// The REAL shape (a b11120 llama-server's /slots, captured 2026-09-30):
// `next_token` is a one-element ARRAY, and the slot reports its prompt
// processing. The README's object shape is accepted too.
func TestParseSlotsActivityReadsARealLlamaServerBody(t *testing.T) {
	body, err := os.ReadFile("testdata/llamacpp-b11120-slots.json")
	if err != nil {
		t.Fatal(err)
	}
	var a Activity
	if err := parseSlotsActivity(body, &a); err != nil {
		t.Fatalf("a real /slots body must parse: %v", err)
	}
	if a.ActSource != "slots" || a.Waiting != -1 || !strings.HasPrefix(a.Fingerprint, "s|0:3:false:0:0|1:2:false:0:0") {
		t.Fatalf("activity = %+v", a)
	}
	a.Loaded = true
	if !strings.Contains(a.Summary(), "queue not visible") {
		t.Fatalf("summary = %q: /slots cannot see a queue and must not claim 0 waiting", a.Summary())
	}
}

func TestParseSlotsActivityShapesAndProgress(t *testing.T) {
	arr := func(decoded, prompt int) string {
		return `[{"id":0,"id_task":135,"is_processing":true,"n_prompt_tokens":900,"n_prompt_tokens_processed":` + itoa(prompt) +
			`,"next_token":[{"has_next_token":true,"n_decoded":` + itoa(decoded) + `}]}]`
	}
	var a, b, c Activity
	if err := parseSlotsActivity([]byte(arr(0, 256)), &a); err != nil {
		t.Fatal(err)
	}
	if err := parseSlotsActivity([]byte(arr(0, 512)), &b); err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint == b.Fingerprint || a.TokenFingerprint == b.TokenFingerprint {
		t.Fatal("a prompt batch processed (256 -> 512) must move both fingerprints: that is the solo-prefill signal")
	}
	if a.Running != 1 {
		t.Fatalf("running = %d, want the processing slot counted", a.Running)
	}
	if err := parseSlotsActivity([]byte(`[{"id":0,"id_task":1,"is_processing":true,"next_token":{"n_decoded":12}}]`), &c); err != nil || !strings.Contains(c.Fingerprint, ":12:") {
		t.Fatalf("the object shape must still parse: %v %q", err, c.Fingerprint)
	}
	for _, bad := range []string{`[]`, `null`, `{"error":"x"}`, `[{"id":0,"next_token":"garbage"}]`} {
		var d Activity
		if err := parseSlotsActivity([]byte(bad), &d); err == nil {
			t.Fatalf("%s must be an error, never a fingerprint (got %q)", bad, d.Fingerprint)
		}
	}
}

func TestLiveRates(t *testing.T) {
	t0 := time.Unix(1000, 0)
	prev := Activity{GenTokens: 100, PromptTokens: 1000, At: t0}
	cur := Activity{GenTokens: 154, PromptTokens: 4614, At: t0.Add(6 * time.Second)}
	g, p, ok := LiveRates(prev, cur)
	if !ok || g != 9 || p != 602.3333333333334 {
		t.Fatalf("rates = %v/%v/%v", g, p, ok)
	}
	// An engine restart between readings (counters went back) is not a rate.
	if _, _, ok := LiveRates(cur, Activity{GenTokens: 3, PromptTokens: 40, At: t0.Add(9 * time.Second)}); ok {
		t.Fatal("a counter going backwards must not yield a rate")
	}
	if _, _, ok := LiveRates(prev, Activity{GenTokens: -1, PromptTokens: -1, At: t0.Add(time.Second)}); ok {
		t.Fatal("a /slots reading has no counters and must not yield a rate")
	}
}

// End to end through a llama-swap stand-in: the engine is read at the seat's
// OWN address, never through /upstream, and an unloaded or loading seat is
// never asked at all.
func TestReadActivityReadsTheSeatNeverTheUpstream(t *testing.T) {
	f := &fakeSwap{id: "qwen-27b", alias: "agent-pool", roster: true}
	var mu sync.Mutex
	steps := 100
	mux := http.NewServeMux()
	inner := f.handler()
	mux.Handle("/", inner)
	mux.HandleFunc("/direct/qwen-27b/metrics", func(w http.ResponseWriter, r *http.Request) {
		f.metricsHits.Add(1)
		if !f.loaded.Load() {
			f.upstreamHitsWhileUnloaded.Add(1)
		}
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write([]byte("vllm:num_requests_running 2\nvllm:num_requests_waiting 1\nvllm:iteration_tokens_total_count " +
			itoa(steps) + "\nvllm:generation_tokens_total 10\nvllm:prompt_tokens_total 20\nvllm:kv_cache_usage_perc 0.1\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()

	// Not loaded: no fingerprint, no error, the engine never asked.
	a, err := ReadActivity(ctx, srv.Client(), srv.URL, "agent-pool")
	if err != nil || a.Loaded || a.Fingerprint != "" || f.metricsHits.Load() != 0 {
		t.Fatalf("unloaded: act=%+v err=%v metricsHits=%d", a, err, f.metricsHits.Load())
	}
	// Loading: listed, no fingerprint, the engine never asked.
	f.loaded.Store(true)
	f.starting.Store(true)
	a, err = ReadActivity(ctx, srv.Client(), srv.URL, "agent-pool")
	if err != nil || !a.Starting || a.Fingerprint != "" || f.metricsHits.Load() != 0 {
		t.Fatalf("starting: act=%+v err=%v metricsHits=%d", a, err, f.metricsHits.Load())
	}
	f.starting.Store(false)
	a1, err := ReadActivity(ctx, srv.Client(), srv.URL, "agent-pool")
	if err != nil || a1.Fingerprint == "" || a1.Running != 2 || a1.Waiting != 1 {
		t.Fatalf("ready: act=%+v err=%v", a1, err)
	}
	mu.Lock()
	steps = 101
	mu.Unlock()
	a2, err := ReadActivity(ctx, srv.Client(), srv.URL, "agent-pool")
	if err != nil || a2.Fingerprint == a1.Fingerprint {
		t.Fatalf("an engine step between readings must change the fingerprint: %q vs %q (%v)", a1.Fingerprint, a2.Fingerprint, err)
	}
	if f.upstreamHits.Load() != 0 || f.upstreamHitsWhileUnloaded.Load() != 0 {
		t.Fatalf("upstream hits = %d, hits while unloaded = %d: the reader must never touch /upstream or an unloaded seat",
			f.upstreamHits.Load(), f.upstreamHitsWhileUnloaded.Load())
	}
}

// An alias-bound seat whose roster cannot be read, with other models running,
// is "cannot tell" — an error — never "not loaded": a busy seat would
// otherwise read as absent and its runs would be judged by the old rule with a
// false reason.
func TestReadActivityCallsAnAmbiguousSeatUnreadable(t *testing.T) {
	f := &fakeSwap{id: "qwen-27b", alias: "agent-pool", roster: false}
	f.loaded.Store(true)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	_, err := ReadActivity(context.Background(), srv.Client(), srv.URL, "agent-pool")
	if err == nil || !strings.Contains(err.Error(), "roster could not be read") {
		t.Fatalf("an ambiguous seat must be an error naming the roster, got %v", err)
	}
}

// A llama-server without --metrics answers 501; /slots is read instead.
func TestReadActivityFallsBackToSlots(t *testing.T) {
	f := &fakeSwap{id: "gemma", alias: "offload", roster: true}
	f.loaded.Store(true)
	f.metricsStatus.Store(http.StatusNotImplemented)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	a, err := ReadActivity(context.Background(), srv.Client(), srv.URL, "offload")
	if err != nil {
		t.Fatal(err)
	}
	if a.ActSource != "slots" || a.Running != 1 || !strings.HasPrefix(a.Fingerprint, "s|") {
		t.Fatalf("activity = %+v", a)
	}
	// A 500 is "cannot tell", never a fallback and never idle.
	f.metricsStatus.Store(http.StatusInternalServerError)
	if _, err := ReadActivity(context.Background(), srv.Client(), srv.URL, "offload"); err == nil {
		t.Fatal("a 500 from /metrics must be an error")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
