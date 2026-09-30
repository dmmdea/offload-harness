package seatload

// Engine activity (ADR 0061, 0.143.0): is the seat's ENGINE doing work right
// now, for anyone? The liveness monitor asks this before it calls a silent
// request stalled. Until 0.143.0 a request's own silence was the only
// evidence, so a request waiting its turn behind siblings, sharing a
// thermally throttled card, or preempted and recomputed by vLLM read exactly
// like a hung engine: on 2026-09-29 96 stall kills, 41 finished jobs thrown
// away in their re-pack and four decode "stalls" in the same second on one
// seat — 85 % of the day's agent jobs failed while the engines were busy
// producing the whole time (the ampere-16 reference seat at 23:35: 4 running, 29 preemptions, +14
// engine steps in 6 s, the A2 thermally throttled to 1072 of 1770 MHz).
//
// The reading is the same two-step read Inflight makes — /running first, the
// engine only when llama-swap lists the seat loaded, always at the seat's OWN
// address, never /upstream (which loads models and resets the idle timer) —
// and it yields a FINGERPRINT: a string that changes whenever the engine takes
// a step for any request, and never merely because a new request arrived.
//
//   - vLLM /metrics: the engine-step count (iteration_tokens_total_count),
//     generated and prompt token counters, preemptions, finished requests,
//     and the KV cache usage gauge. The counters move once per step with
//     output; the KV usage moves chunk by chunk through a long SOLO prefill
//     (vLLM credits prompt tokens only when the prefill finishes, and a step
//     that produced no token emits no iteration stats, so without the gauge a
//     20k-token prefill on an idle seat would read flat).
//   - llama-server /metrics (--metrics): n_decode_total counts every
//     llama_decode() call, prompt batches included, plus the prompt and
//     predicted token counters.
//   - llama-server /slots (no --metrics, the fleet default until 0.143.0):
//     each slot's task id and n_decoded. It moves while a slot generates or a
//     new task starts; a slot's prompt processing is invisible here, so a
//     prefill on a /slots-only seat still rests on the prefill allowance.
//
// Request gauges (running, waiting, processing, deferred) are reported but
// NEVER part of the fingerprint: a wedged engine core whose HTTP front end
// still accepts requests grows its waiting count forever, and counting that
// as work would hold a dead seat's runs until their ceilings.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Activity is one reading of a seat's engine.
type Activity struct {
	// Reading is the /running half (Loaded, Starting, Stopping, Proxy, …).
	// Starting or !Loaded means the engine was not asked.
	Reading
	// Fingerprint changes whenever the engine takes a step for any request.
	// "" when the engine was not asked or no counter could be read.
	Fingerprint string
	// TokenFingerprint changes only when the engine PRODUCES: generated or
	// prompt tokens credited (vLLM, llama-server --metrics), or a slot's
	// decoded / prompt-processed counts (/slots). An engine that keeps stepping
	// without producing — a preempt-and-recompute thrash moves the step count,
	// the KV gauge and the preemption counter, never a token — is caught by the
	// monitor's token bound on this, not by the work fingerprint.
	TokenFingerprint string
	// Running and Waiting are the engine's own request gauges (vLLM running
	// and waiting; llama-server processing and deferred; /slots processing).
	// Waiting is -1 when the source cannot see a queue (/slots lists no
	// deferred requests).
	Running, Waiting int
	// KVGaugeMissing: a vLLM exposition carried neither KV-usage gauge, so a
	// solo prefill (no token, no iteration stats) cannot move the fingerprint.
	// Named in Summary so a reader sees why a prefill looked flat.
	KVGaugeMissing bool
	// GenTokens and PromptTokens are the engine's lifetime counters, -1 when
	// the source has none (/slots). Two readings apart give the throughput
	// the seat is achieving NOW — throttled, shared or not (LiveRates).
	GenTokens, PromptTokens float64
	// Preemptions is vLLM's num_preemptions_total, -1 when unknown.
	Preemptions float64
	// ActSource names what answered: "vllm-metrics", "llamacpp-metrics" or
	// "slots".
	ActSource string
	// At is when the engine answered.
	At time.Time
}

// Summary is the reading in one short clause for a reason or a status line.
func (a Activity) Summary() string {
	switch {
	case !a.Loaded:
		return "seat not loaded"
	case a.Starting:
		return "seat " + strings.TrimPrefix(a.Source, "running-state:")
	case a.Fingerprint == "":
		return "engine unreadable"
	}
	var s string
	if a.Waiting < 0 {
		s = fmt.Sprintf("%s: %d processing (queue not visible)", a.ActSource, a.Running)
	} else {
		s = fmt.Sprintf("%s: %d running, %d waiting", a.ActSource, a.Running, a.Waiting)
	}
	if a.Preemptions > 0 {
		s += fmt.Sprintf(", %.0f preemptions so far", a.Preemptions)
	}
	if a.KVGaugeMissing {
		s += ", no KV-usage gauge (a solo prefill reads flat)"
	}
	return s
}

// LiveRates is the throughput the engine achieved between two readings of
// the same seat, in tokens per second of wall: generated tokens and prompt
// tokens (vLLM credits a prompt when its prefill finishes). ok is false when
// either reading lacks the counters, the readings are out of order, or a
// counter went backwards (an engine restart between them).
func LiveRates(prev, cur Activity) (genTokS, promptTokS float64, ok bool) {
	dt := cur.At.Sub(prev.At).Seconds()
	if dt <= 0 || prev.GenTokens < 0 || cur.GenTokens < 0 || prev.PromptTokens < 0 || cur.PromptTokens < 0 {
		return 0, 0, false
	}
	dg, dp := cur.GenTokens-prev.GenTokens, cur.PromptTokens-prev.PromptTokens
	if dg < 0 || dp < 0 {
		return 0, 0, false
	}
	return dg / dt, dp / dt, true
}

// ReadActivity reads the seat named `seat` (id or alias) behind the llama-swap
// at endpoint. A seat that is not loaded, or is loading/unloading, comes back
// with no fingerprint and no error: that is llama-swap's answer, and the
// caller's cold-load hold owns it. A loaded seat whose engine cannot be read
// is an error — "cannot tell" is never "idle" and never "busy".
func ReadActivity(ctx context.Context, client *http.Client, endpoint, seat string) (Activity, error) {
	base := strings.TrimRight(endpoint, "/")
	rd, done, err := running(ctx, client, endpoint, seat)
	act := Activity{Reading: rd, GenTokens: -1, PromptTokens: -1, Preemptions: -1}
	if err != nil {
		return act, err
	}
	if done && rd.Ambiguous {
		// Not listed by the bare name, the roster unreadable and /running not
		// empty: the seat may be loaded under a name this read could not
		// resolve. That is "cannot tell", never "not loaded" — a busy
		// alias-bound seat would otherwise read as absent.
		return act, fmt.Errorf("seat activity: %s is not listed by name and the roster could not be read (%v); %d other model(s) are running, one may be this seat", seat, rd.RosterErr, rd.RunningOthers)
	}
	if done {
		return act, nil
	}
	seatBase, remote, err := seatURL(base, rd.Proxy)
	if err != nil {
		return act, fmt.Errorf("seat activity: %w", err)
	}
	status, body, err := getBody(ctx, client, seatBase+"/metrics")
	if err != nil {
		if remote && ctx.Err() == nil {
			return act, fmt.Errorf("seat activity at %s: %w: %v", seatBase, ErrRemoteSeatUnreachable, err)
		}
		return act, fmt.Errorf("seat activity: %w", err)
	}
	switch status {
	case http.StatusOK:
		if perr := parseEngineMetrics(body, &act); perr != nil {
			return act, fmt.Errorf("seat activity: %w", perr)
		}
	case http.StatusNotImplemented, http.StatusNotFound:
		// llama-server without --metrics: /slots is on by default.
		sst, sbody, serr := getBody(ctx, client, seatBase+"/slots")
		if serr != nil {
			return act, fmt.Errorf("seat activity: /metrics status %d and /slots: %w", status, serr)
		}
		if sst != http.StatusOK {
			return act, fmt.Errorf("seat activity: /metrics status %d and /slots status %d", status, sst)
		}
		if perr := parseSlotsActivity(sbody, &act); perr != nil {
			return act, fmt.Errorf("seat activity: /slots: %w", perr)
		}
	default:
		return act, fmt.Errorf("seat activity: /metrics status %d", status)
	}
	act.At = time.Now()
	return act, nil
}

// getBody is one bounded GET at the seat's own address.
func getBody(ctx context.Context, client *http.Client, u string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, err
}

// metricSum is one metric's value summed over its label sets (vLLM labels
// every series with the model and engine index; a multi-engine server has one
// series per engine).
type metricSums map[string]float64

// parseExposition sums every sample of every metric in a Prometheus text
// exposition by its exact name (the part before `{` or the first space).
func parseExposition(body []byte) metricSums {
	out := metricSums{}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		// `name{labels} value [timestamp]` or `name value [timestamp]`; label
		// values may hold spaces, so the value is read after the LAST '}'.
		name, rest := line, ""
		if i := strings.IndexByte(line, '{'); i >= 0 {
			name = line[:i]
			if j := strings.LastIndexByte(line, '}'); j > i {
				rest = line[j+1:]
			}
		} else if i := strings.IndexByte(line, ' '); i >= 0 {
			name, rest = line[:i], line[i+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out[name] += v
	}
	return out
}

// has reports whether any listed metric is present.
func (m metricSums) has(names ...string) bool {
	for _, n := range names {
		if _, ok := m[n]; ok {
			return true
		}
	}
	return false
}

// parseEngineMetrics fills the activity from a vLLM or llama-server
// exposition. An exposition carrying neither engine's work counters is an
// error: a fingerprint built from nothing would read as a permanently idle
// engine and file a false hang.
func parseEngineMetrics(body []byte, act *Activity) error {
	m := parseExposition(body)
	switch {
	case m.has("vllm:iteration_tokens_total_count", "vllm:generation_tokens_total", "vllm:prompt_tokens_total"):
		act.ActSource = "vllm-metrics"
		act.Running = int(m["vllm:num_requests_running"] + 0.5)
		act.Waiting = int(m["vllm:num_requests_waiting"] + 0.5)
		act.GenTokens = m["vllm:generation_tokens_total"]
		act.PromptTokens = m["vllm:prompt_tokens_total"]
		if v, ok := m["vllm:num_preemptions_total"]; ok {
			act.Preemptions = v
		}
		kv := m["vllm:kv_cache_usage_perc"]
		switch {
		case m.has("vllm:kv_cache_usage_perc"):
		case m.has("vllm:gpu_cache_usage_perc"):
			kv = m["vllm:gpu_cache_usage_perc"] // pre-V1 name
		default:
			act.KVGaugeMissing = true
		}
		// request_success_total is NOT part of it: it sums every finished
		// reason, `abort` included, and an abort is a front-end event (our own
		// killed requests), not engine work — on a wedged engine each sibling's
		// kill would restart the others' flat clocks.
		act.Fingerprint = fmt.Sprintf("v|%.0f|%.0f|%.0f|%.0f|%.5f",
			m["vllm:iteration_tokens_total_count"], act.GenTokens, act.PromptTokens,
			m["vllm:num_preemptions_total"], kv)
		act.TokenFingerprint = fmt.Sprintf("vt|%.0f|%.0f", act.GenTokens, act.PromptTokens)
	case m.has("llamacpp:n_decode_total", "llamacpp:tokens_predicted_total", "llamacpp:prompt_tokens_total"):
		act.ActSource = "llamacpp-metrics"
		act.Running = int(m["llamacpp:requests_processing"] + 0.5)
		act.Waiting = int(m["llamacpp:requests_deferred"] + 0.5)
		act.GenTokens = m["llamacpp:tokens_predicted_total"]
		act.PromptTokens = m["llamacpp:prompt_tokens_total"]
		act.Fingerprint = fmt.Sprintf("l|%.0f|%.0f|%.0f",
			m["llamacpp:n_decode_total"], act.GenTokens, act.PromptTokens)
		act.TokenFingerprint = fmt.Sprintf("lt|%.0f|%.0f", act.GenTokens, act.PromptTokens)
	default:
		return fmt.Errorf("the exposition carries no engine work counter (neither vLLM's iteration/token counters nor llama-server's n_decode/token counters)")
	}
	return nil
}

// parseSlotsActivity fills the activity from a llama-server /slots body: the
// processing count, and a fingerprint of every slot's task id and decoded
// count. A body that is not an array is an error.
//
// llama-server emits `next_token` as a ONE-ELEMENT ARRAY (`[{"n_decoded":…}]`,
// measured on b11120; the README still shows an object), so it is decoded
// raw and both shapes are accepted. Builds that report
// `n_prompt_tokens_processed` (b11120 does) make a slot's PROMPT processing
// visible too, so a prefill on a /slots-only seat moves the fingerprint.
func parseSlotsActivity(body []byte, act *Activity) error {
	var slots []struct {
		ID              int             `json:"id"`
		IDTask          int             `json:"id_task"`
		Processing      bool            `json:"is_processing"`
		PromptTokens    int             `json:"n_prompt_tokens"`
		PromptProcessed int             `json:"n_prompt_tokens_processed"`
		NextToken       json.RawMessage `json:"next_token"`
	}
	if err := json.Unmarshal(body, &slots); err != nil {
		return err
	}
	if len(slots) == 0 {
		// A llama-server always has at least one slot: an empty (or null) list
		// is not an idle engine, it is a reading of nothing.
		return fmt.Errorf("the /slots list is empty")
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })
	var work, tok strings.Builder
	work.WriteString("s")
	tok.WriteString("st")
	for _, s := range slots {
		if s.Processing {
			act.Running++
		}
		decoded, err := nDecoded(s.NextToken)
		if err != nil {
			return fmt.Errorf("slot %d next_token: %w", s.ID, err)
		}
		fmt.Fprintf(&work, "|%d:%d:%t:%d:%d", s.ID, s.IDTask, s.Processing, decoded, s.PromptProcessed)
		fmt.Fprintf(&tok, "|%d:%d:%d:%d", s.ID, s.IDTask, decoded, s.PromptProcessed)
	}
	act.ActSource = "slots"
	act.Waiting = -1 // /slots lists no deferred requests
	act.Fingerprint = work.String()
	act.TokenFingerprint = tok.String()
	return nil
}

// nDecoded reads a slot's decoded count from `next_token` in either shape
// llama-server has used: an object, or an array of one object. An absent
// field (a slot that never had a task) is 0.
func nDecoded(raw json.RawMessage) (int, error) {
	type tok struct {
		NDecoded int `json:"n_decoded"`
	}
	t := strings.TrimSpace(string(raw))
	switch {
	case t == "" || t == "null":
		return 0, nil
	case strings.HasPrefix(t, "["):
		var arr []tok
		if err := json.Unmarshal(raw, &arr); err != nil {
			return 0, err
		}
		n := 0
		for _, a := range arr {
			n += a.NDecoded
		}
		return n, nil
	default:
		var one tok
		if err := json.Unmarshal(raw, &one); err != nil {
			return 0, err
		}
		return one.NDecoded, nil
	}
}
