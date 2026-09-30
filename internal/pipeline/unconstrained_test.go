// unconstrained_test.go pins the grammar-free path for a seat whose runtime cannot constrain
// decoding (config unconstrained_seats; the RKLLM runtime on an RK3588 NPU). The fake seat does
// what that runtime does: HTTP 400 constrained_decoding_unsupported for any grammar, json_schema,
// response_format or structured_outputs field, and it ignores logprobs.
package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

const npuSeat = "npu-2b"

// npuFake is the seat. replies are served in order (the last one repeats); a body naming a
// constraint is refused with the runtime's own 400 and counted in refused.
type npuFake struct {
	mu      sync.Mutex
	replies []string
	bodies  []map[string]any
	refused int
}

func (f *npuFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": npuSeat}}})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.bodies = append(f.bodies, body)
		g, _ := body["grammar"].(string)
		rf, _ := body["response_format"].(map[string]any)
		if g != "" || body["json_schema"] != nil || body["structured_outputs"] != nil || (rf != nil && rf["type"] != "text") {
			f.refused++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"constrained decoding is not supported by this runtime","type":"invalid_request_error","code":"constrained_decoding_unsupported"}}`))
			return
		}
		i := len(f.bodies) - 1
		if i >= len(f.replies) {
			i = len(f.replies) - 1
		}
		_, _ = w.Write(fakeChat{content: f.replies[i], finishReason: "stop", promptTokens: 60}.marshal())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *npuFake) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func npuPipeline(t *testing.T, srvURL string, retries int, declare bool) *Pipeline {
	t.Helper()
	cfg := config.Default()
	cfg.Endpoint = srvURL
	cfg.Model = npuSeat
	cfg.TriageModel = npuSeat
	cfg.EscalationModel = ""
	cfg.ReasoningModel = ""
	cfg.MaxRetries = retries
	cfg.ThresholdsPath, cfg.RouterWeightsPath, cfg.TierOverridesPath, cfg.ConfHeadLabelsPath, cfg.CachePath = "", "", "", "", ""
	if declare {
		cfg.UnconstrainedSeats = []string{npuSeat}
	}
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg.LedgerPath = ledgerPath
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	return New(cfg, llamaclient.New(srvURL, cfg.CompletionPath, "", 10*time.Second), nil, led)
}

const npuText = "Invoice 4471 from Northwind Traders is overdue by 10 days; please advise on the late fee and the payment plan."

func npuClassify() core.Request {
	return core.Request{Task: core.TaskClassify, Input: npuText,
		Params: map[string]any{"labels": []string{"billing", "support", "sales"}}}
}

func npuExtract() core.Request {
	return core.Request{Task: core.TaskExtract, Input: npuText, Params: map[string]any{"schema": map[string]any{
		"type":       "object",
		"properties": map[string]any{"vendor": map[string]any{"type": "string"}, "days_overdue": map[string]any{"type": "integer"}},
		"required":   []any{"vendor", "days_overdue"},
	}}}
}

func assertNoConstraintOnTheWire(t *testing.T, f *npuFake) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("no request reached the seat")
	}
	for i, b := range f.bodies {
		for _, k := range []string{"grammar", "json_schema", "structured_outputs", "response_format", "logprobs", "top_logprobs"} {
			if v, ok := b[k]; ok && v != nil && v != "" && v != false && v != float64(0) {
				t.Errorf("body %d to the unconstrained seat carries %q = %v", i, k, v)
			}
		}
	}
}

// (a) classify and extract succeed through the prompt-carried shape, and nothing the runtime
// refuses is sent.
func TestUnconstrainedSeatServesClassifyAndExtractWithoutAConstraint(t *testing.T) {
	f := &npuFake{replies: []string{`{"label":"billing","confidence":0.95}`}}
	srv := f.server(t)
	p := npuPipeline(t, srv.URL, 0, true)

	res := p.Run(context.Background(), npuClassify())
	if !res.OK {
		t.Fatalf("classify deferred: %s", res.Reason)
	}
	var cls struct {
		Label string `json:"label"`
	}
	_ = json.Unmarshal(res.Data, &cls)
	if cls.Label != "billing" {
		t.Errorf("classify data = %s", res.Data)
	}
	// The shape must be in the prompt, since the grammar is not.
	f.mu.Lock()
	first, _ := json.Marshal(f.bodies[0]["messages"])
	f.mu.Unlock()
	if !strings.Contains(string(first), "OUTPUT FORMAT") || !strings.Contains(string(first), `exactly one of \"billing\", \"support\", \"sales\"`) {
		t.Errorf("the request must carry the exact-shape instruction: %s", first)
	}
	// The row carries the fingerprint of the prompt that was SENT (shape instruction included), not
	// of the grammar-bearing build Run stamped before the rung was chosen.
	sent, _ := tasks.BuildFor(npuClassify(), tasks.Caps{Unconstrained: true})
	normal, _ := tasks.Build(npuClassify())
	if want := promptPrefixFingerprint(sent.System, userPreambleOf(sent.User, npuText)); res.Meta.PromptPrefixSHA256 != want {
		t.Errorf("PromptPrefixSHA256 = %q, want the sent prompt's %q", res.Meta.PromptPrefixSHA256, want)
	}
	if res.Meta.PromptPrefixSHA256 == promptPrefixFingerprint(normal.System, userPreambleOf(normal.User, npuText)) {
		t.Errorf("the row carries the grammar prompt's fingerprint, which was not the prompt sent")
	}
	// No margin is recorded: no logprobs came back and none were asked for.
	if res.Meta.Margin != 0 || res.Meta.MarginScale != "" {
		t.Errorf("the margin gate is inert on an unconstrained seat, got margin %v scale %q", res.Meta.Margin, res.Meta.MarginScale)
	}

	f.mu.Lock()
	f.replies = []string{`{"vendor":"Northwind Traders","days_overdue":10}`}
	f.mu.Unlock()
	res = p.Run(context.Background(), npuExtract())
	if !res.OK {
		t.Fatalf("extract deferred: %s", res.Reason)
	}
	assertNoConstraintOnTheWire(t, f)
	if f.refused != 0 {
		t.Errorf("%d constrained bodies reached the seat", f.refused)
	}
}

// The control: the same fake, with the seat NOT declared, is what production did before this
// change: a grammar goes out, the runtime refuses it, and the call defers as an infra error.
func TestUndeclaredSeatStillSendsAGrammarAndDefersOnTheRuntimes400(t *testing.T) {
	f := &npuFake{replies: []string{`{"label":"billing","confidence":0.95}`}}
	srv := f.server(t)
	p := npuPipeline(t, srv.URL, 0, false)
	res := p.Run(context.Background(), npuClassify())
	if res.OK || !strings.Contains(res.Reason, "constrained_decoding_unsupported") {
		t.Fatalf("an undeclared seat must still defer on the 400, got ok=%v reason=%q", res.OK, res.Reason)
	}
}

// (b) a missing required field is retried once (Retries counts it), then the call defers with the
// validator's words naming what failed.
func TestUnconstrainedMissingFieldRetriesThenDefersNamingIt(t *testing.T) {
	f := &npuFake{replies: []string{`{"label":"billing"}`}}
	srv := f.server(t)
	p := npuPipeline(t, srv.URL, 1, true)
	res := p.Run(context.Background(), npuClassify())
	if res.OK {
		t.Fatalf("a reply missing a required field must never be accepted: %s", res.Data)
	}
	if n := f.requests(); n != 2 {
		t.Errorf("want 1 call + 1 correction retry = 2 requests, got %d", n)
	}
	if !strings.Contains(res.Reason, "schema") || !strings.Contains(res.Reason, "confidence") {
		t.Errorf("the defer must name what failed, got %q", res.Reason)
	}
	if res.Meta.Retries != 1 {
		t.Errorf("Retries = %d, want 1", res.Meta.Retries)
	}
	// The retry's prompt carries the correction.
	f.mu.Lock()
	second, _ := json.Marshal(f.bodies[1]["messages"])
	f.mu.Unlock()
	if !strings.Contains(string(second), "previous reply was rejected") {
		t.Errorf("the retry must say what was rejected: %s", second)
	}

	// A retry that fixes it succeeds and records the retry.
	f2 := &npuFake{replies: []string{`{"label":"billing"}`, `{"label":"billing","confidence":0.95}`}}
	srv2 := f2.server(t)
	p2 := npuPipeline(t, srv2.URL, 1, true)
	res = p2.Run(context.Background(), npuClassify())
	if !res.OK || res.Meta.Retries != 1 {
		t.Fatalf("the corrected retry must succeed with Retries=1, got ok=%v retries=%d reason=%q", res.OK, res.Meta.Retries, res.Reason)
	}
}

// (c) and (d): whatever a small model says, only the schema's object is accepted.
func TestUnconstrainedRejectsOffSchemaReplies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		req   core.Request
		reply string
		want  string
	}{
		{"classify label outside the set", npuClassify(), `{"label":"refund","confidence":0.95}`, "label"},
		{"classify foreign object", npuClassify(), `{"foo":1}`, "label"},
		{"classify summarize-shaped object", npuClassify(), `{"summary":"x"}`, "label"},
		{"classify extra key", npuClassify(), `{"label":"billing","confidence":0.9,"why":"late"}`, "why"},
		{"classify confidence as a string", npuClassify(), `{"label":"billing","confidence":"high"}`, "confidence"},
		{"extract foreign object", npuExtract(), `{"foo":1}`, "vendor"},
		{"extract extra key", npuExtract(), `{"vendor":"Northwind Traders","days_overdue":10,"fee":5}`, "fee"},
		{"extract wrong type", npuExtract(), `{"vendor":"Northwind Traders","days_overdue":"ten"}`, "days_overdue"},
		{"not json at all", npuClassify(), `I think it is billing.`, "unparseable"},
	} {
		f := &npuFake{replies: []string{tc.reply}}
		srv := f.server(t)
		p := npuPipeline(t, srv.URL, 0, true)
		res := p.Run(context.Background(), tc.req)
		if res.OK {
			t.Errorf("%s: an off-schema reply was accepted: %s", tc.name, res.Data)
			continue
		}
		if !strings.Contains(res.Reason, tc.want) {
			t.Errorf("%s: the defer should name %q, got %q", tc.name, tc.want, res.Reason)
		}
	}
}

// (e) code-fenced JSON and leading prose are accepted when the object inside is valid.
func TestUnconstrainedAcceptsFencedAndPrefacedJSONWhenValid(t *testing.T) {
	for name, reply := range map[string]string{
		"fenced":   "```json\n{\"label\":\"billing\",\"confidence\":0.9}\n```",
		"prefaced": "Sure! Here is the answer:\n{\"label\":\"billing\",\"confidence\":0.9}",
	} {
		f := &npuFake{replies: []string{reply}}
		srv := f.server(t)
		p := npuPipeline(t, srv.URL, 0, true)
		res := p.Run(context.Background(), npuClassify())
		if !res.OK {
			t.Errorf("%s: a valid object must be accepted, got %q", name, res.Reason)
		}
	}
	// ...and a fenced INVALID object is still refused.
	f := &npuFake{replies: []string{"```json\n{\"label\":\"refund\",\"confidence\":0.9}\n```"}}
	srv := f.server(t)
	if res := npuPipeline(t, srv.URL, 0, true).Run(context.Background(), npuClassify()); res.OK {
		t.Errorf("a fenced object with a label outside the set must be refused: %s", res.Data)
	}
}

// Grounding still applies to extract: a well-formed reply whose values are not in the source is
// refused, exactly as on a grammar seat.
func TestUnconstrainedExtractStillGrounds(t *testing.T) {
	f := &npuFake{replies: []string{`{"vendor":"Contoso Ltd","days_overdue":10}`}}
	srv := f.server(t)
	res := npuPipeline(t, srv.URL, 0, true).Run(context.Background(), npuExtract())
	if res.OK {
		t.Fatalf("an ungrounded extract must not be accepted: %s", res.Data)
	}
	if !strings.Contains(res.Reason, "ungrounded") {
		t.Errorf("reason = %q", res.Reason)
	}
}

// Classify's self-reported confidence gate is unchanged: a low-confidence answer is not accepted.
func TestUnconstrainedClassifySelfConfidenceGateStillApplies(t *testing.T) {
	f := &npuFake{replies: []string{`{"label":"billing","confidence":0.05}`}}
	srv := f.server(t)
	res := npuPipeline(t, srv.URL, 0, true).Run(context.Background(), npuClassify())
	if res.OK || !strings.Contains(res.Reason, "low confidence") {
		t.Errorf("a self-reported low confidence must still defer, got ok=%v reason=%q", res.OK, res.Reason)
	}
}

// (f) a seat that takes a grammar is untouched by the existence of an unconstrained seat on the
// same box: its body carries exactly Build's system, user and grammar.
func TestGrammarSeatBodyIsByteIdenticalWhenAnotherSeatIsUnconstrained(t *testing.T) {
	const cpp = "gemma4-e4b"
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = w.Write(fakeChat{content: `{"label":"billing","confidence":0.95}`, finishReason: "stop", promptTokens: 60}.marshal())
	}))
	defer srv.Close()
	p := npuPipeline(t, srv.URL, 0, true) // npu-2b declared unconstrained
	p.cfg.Model, p.cfg.TriageModel = cpp, cpp
	res := p.Run(context.Background(), npuClassify())
	if !res.OK {
		t.Fatalf("grammar seat deferred: %s", res.Reason)
	}
	want, _ := tasks.Build(npuClassify())
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no request reached the grammar seat")
	}
	if g, _ := bodies[0]["grammar"].(string); g != want.Grammar {
		t.Errorf("the grammar seat's grammar changed")
	}
	msgs, _ := json.Marshal(bodies[0]["messages"])
	wantMsgs, _ := json.Marshal([]map[string]string{{"role": "system", "content": want.System}, {"role": "user", "content": want.User}})
	if string(msgs) != string(wantMsgs) {
		t.Errorf("the grammar seat's prompt changed:\n got  %s\n want %s", msgs, wantMsgs)
	}
	if strings.Contains(string(msgs), "OUTPUT FORMAT") {
		t.Errorf("the shape instruction leaked into a grammar seat")
	}
}

// (g) summarize and triage on an unconstrained seat run through the same validated path locally
// (the fleet text lane refuses them; see internal/fleetnode). A reply that is not their object is
// refused, one that is, is accepted; on the wire there is still no constraint.
func TestUnconstrainedSummarizeAndTriageRunThroughTheValidatedPath(t *testing.T) {
	sum := core.Request{Task: core.TaskSummarize, Input: npuText, Params: map[string]any{"max_points": 2}}
	tri := core.Request{Task: core.TaskTriage, Input: npuText, Params: map[string]any{"question": "Is the invoice overdue?"}}

	f := &npuFake{replies: []string{`{"summary":"Invoice 4471 is overdue.","bullets":["ten days late"]}`}}
	srv := f.server(t)
	if res := npuPipeline(t, srv.URL, 0, true).Run(context.Background(), sum); !res.OK {
		t.Errorf("a valid summarize object must be accepted locally: %s", res.Reason)
	}
	assertNoConstraintOnTheWire(t, f)

	f = &npuFake{replies: []string{`{"label":"billing","confidence":0.95}`}}
	srv = f.server(t)
	if res := npuPipeline(t, srv.URL, 0, true).Run(context.Background(), sum); res.OK {
		t.Errorf("a classify-shaped reply to summarize must be refused: %s", res.Data)
	}

	f = &npuFake{replies: []string{`{"decision":"maybe","reason":"x"}`}}
	srv = f.server(t)
	if res := npuPipeline(t, srv.URL, 0, true).Run(context.Background(), tri); res.OK {
		t.Errorf("a triage decision outside yes/no/unsure must be refused: %s", res.Data)
	}
	f = &npuFake{replies: []string{`{"decision":"yes","reason":"ten days late"}`}}
	srv = f.server(t)
	if res := npuPipeline(t, srv.URL, 0, true).Run(context.Background(), tri); !res.OK {
		t.Errorf("a valid triage object must be accepted locally: %s", res.Reason)
	}
}

// The declared-seat predicate is case-insensitive and exact, like DeclaresVLLMSeat.
func TestDeclaresUnconstrainedSeat(t *testing.T) {
	c := config.Config{UnconstrainedSeats: []string{"NPU-2B", " alias "}}
	for id, want := range map[string]bool{"npu-2b": true, "alias": true, "npu": false, "": false, "other": false} {
		if got := c.DeclaresUnconstrainedSeat(id); got != want {
			t.Errorf("DeclaresUnconstrainedSeat(%q) = %v, want %v", id, got, want)
		}
	}
	if (config.Config{}).DeclaresUnconstrainedSeat("x") {
		t.Error("a box that declares none must treat every seat as constrained")
	}
}

// The terminal reasoning attempt obeys the same rule: a declared seat gets no think-wrapped grammar,
// the shape rides in the prompt, and the reply is strictly validated.
func TestUnconstrainedReasoningAttemptSendsNoGrammarAndValidates(t *testing.T) {
	for reply, wantOK := range map[string]bool{
		`{"label":"billing","confidence":0.95}`: true,
		`{"label":"refund","confidence":0.95}`:  false,
	} {
		f := &npuFake{replies: []string{reply}}
		srv := f.server(t)
		p := npuPipeline(t, srv.URL, 0, true)
		req := npuClassify()
		built, err := tasks.Build(req)
		if err != nil {
			t.Fatal(err)
		}
		res, ok := p.attemptReasoningOn(context.Background(), npuSeat, req, built, "k", core.Meta{}, time.Now(), len(req.Input))
		if ok != wantOK || res.OK != wantOK {
			t.Errorf("reply %s: ok=%v, want %v (reason %q)", reply, ok, wantOK, res.Reason)
		}
		assertNoConstraintOnTheWire(t, f)
	}
}
