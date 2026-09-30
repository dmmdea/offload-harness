// Text lane tests (0.154.0), mirroring vision_task_test.go. The invariants:
//
//  1. Advertisement == admission: "text" (and text_tasks) appear in health exactly
//     when POST /fleet/text admits: the tier declared a text task and the listener
//     is safely reachable. No text_tasks, no lane: it ships dark.
//  2. Only classify and extract are ever admitted; summarize and triage are refused
//     at ack time whatever text_tasks says, and a task outside the node's own set is
//     a 400 naming that set.
//  3. The caller reads back the FULL core.Result the node's pipeline produced,
//     defers included, and the job poll rides the bearer gate.
package fleetnode

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// textCfg declares both text tasks on top of imageCfg.
func textCfg(token string) config.Config {
	c := imageCfg()
	c.TextTasks = []string{"classify", "extract"}
	c.FleetAuthToken = token
	return c
}

func textBody(jobID, task, input string, params map[string]any) string {
	m := map[string]any{"job_id": jobID, "task": task, "input": input}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func classifyParams() map[string]any {
	return map[string]any{"labels": []string{"billing", "support"}}
}

func extractParams() map[string]any {
	return map[string]any{"schema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}}
}

func TestTextLaneAdvertisementMatchesAdmission(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		loopback   bool
		header     string
		advertised bool
		wantStatus int
	}{
		{"loopback, no token", "", true, "", true, http.StatusAccepted},
		{"non-loopback, no token", "", false, "", false, http.StatusForbidden},
		{"non-loopback, token, no header", "s3cret", false, "", true, http.StatusUnauthorized},
		{"non-loopback, token, wrong header", "s3cret", false, "Bearer nope", true, http.StatusUnauthorized},
		{"non-loopback, token, right header", "s3cret", false, "Bearer s3cret", true, http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &visionRunner{res: core.Result{OK: true, Data: json.RawMessage(`{"label":"billing","confidence":0.9}`)}}
			s, _ := newTestServer(t, textCfg(tc.token), r, authOpts(tc.loopback))
			h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
			listed := strings.Contains(string(mustJSON(t, h["supported_task_types"])), `"text"`)
			if listed != tc.advertised {
				t.Fatalf("text advertised = %v, want %v (%v)", listed, tc.advertised, h["supported_task_types"])
			}
			if _, has := h["text_tasks"]; has != tc.advertised {
				t.Fatalf("text_tasks present = %v, want %v", has, tc.advertised)
			}
			hdr := map[string]string{}
			if tc.header != "" {
				hdr["Authorization"] = tc.header
			}
			rec := do(t, s, http.MethodPost, "/fleet/text", textBody("tj-1", "classify", "an invoice is overdue", classifyParams()), hdr)
			if rec.Code != tc.wantStatus {
				t.Fatalf("dispatch status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestTextLaneShipsDark: a node whose tier declares no text task advertises nothing and
// refuses the route, whatever else it serves (here: a bound vision model).
func TestTextLaneShipsDark(t *testing.T) {
	cfg := visionCfg("") // vision bound, no text_tasks
	s, _ := newTestServer(t, cfg, &visionRunner{}, authOpts(true))
	h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if _, has := h["text_tasks"]; has {
		t.Fatal("text_tasks must be absent when no text task is declared")
	}
	if strings.Contains(string(mustJSON(t, h["supported_task_types"])), `"text"`) {
		t.Fatalf("text advertised with no text_tasks: %v", h["supported_task_types"])
	}
	rec := do(t, s, http.MethodPost, "/fleet/text", textBody("tj-2", "classify", "an invoice is overdue", classifyParams()), nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "unsupported task_type")
}

// TestTextJobCarriesTheFullResultAndTheRequestMirrorsTheMCPHandlers: a defer comes back
// verbatim as a done job's data; the request the node's runner saw is exactly what
// offload_classify / offload_extract build.
func TestTextJobCarriesTheFullResultAndTheRequestMirrorsTheMCPHandlers(t *testing.T) {
	deferred := core.Deferf("schema: missing property 'confidence'", `{"label":"billing"}`, core.Meta{Model: "npu-2b", Retries: 1})
	r := &visionRunner{res: deferred}
	s, _ := newTestServer(t, textCfg("tok"), r, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer tok"}

	rec := do(t, s, http.MethodPost, "/fleet/text", textBody("tj-3", "classify", "an invoice is overdue", classifyParams()), auth)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	m := pollDone(t, s, "tj-3", auth)
	if m["state"] != "done" {
		t.Fatalf("state = %v, want done (a defer is a done job whose data says deferred)", m["state"])
	}
	var res core.Result
	if err := json.Unmarshal(mustJSON(t, m["data"]), &res); err != nil {
		t.Fatalf("data is not a core.Result: %v", err)
	}
	if !res.Deferred || res.Reason != deferred.Reason || res.Meta.Model != "npu-2b" || res.Meta.Retries != 1 {
		t.Fatalf("result = %+v, want the runner's defer verbatim", res)
	}
	req, _ := r.last()
	labels, _ := req.Params["labels"].([]string)
	if req.Task != core.TaskClassify || req.Input != "an invoice is overdue" || len(labels) != 2 || labels[0] != "billing" {
		t.Fatalf("classify request = %+v", req)
	}

	if rec := do(t, s, http.MethodPost, "/fleet/text", textBody("tj-4", "extract", "Ada Lovelace", extractParams()), auth); rec.Code != http.StatusAccepted {
		t.Fatalf("extract dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	pollDone(t, s, "tj-4", auth)
	req, _ = r.last()
	if _, ok := req.Params["schema"].(map[string]any); req.Task != core.TaskExtract || !ok {
		t.Fatalf("extract request = %+v", req)
	}
}

func TestTextJobPollIsTokenGated(t *testing.T) {
	r := &visionRunner{res: core.Result{OK: true, Data: json.RawMessage(`{"label":"billing","confidence":0.9}`)}}
	s, _ := newTestServer(t, textCfg("tok"), r, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer tok"}
	if rec := do(t, s, http.MethodPost, "/fleet/text", textBody("tj-5", "classify", "an invoice is overdue", classifyParams()), auth); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	wantErrorShape(t, do(t, s, http.MethodGet, "/fleet/jobs/tj-5", "", nil), http.StatusUnauthorized, "unauthorized")
	if m := pollDone(t, s, "tj-5", auth); m["state"] != "done" {
		t.Fatalf("state = %v", m["state"])
	}
}

// TestTextTasksRefuseAtAck: summarize and triage are refused whatever text_tasks lists (even
// when an operator hand-writes them), a task outside the node's set is a 400 naming the set,
// and nothing refused reaches the runner or leaves a job behind.
func TestTextTasksRefuseAtAck(t *testing.T) {
	cfg := textCfg("")
	cfg.TextTasks = []string{"classify"} // extract not declared
	r := &visionRunner{res: core.Result{OK: true, Data: json.RawMessage(`{}`)}}
	s, _ := newTestServer(t, cfg, r, authOpts(true))
	wide := textCfg("")
	wide.TextTasks = []string{"classify", "extract", "summarize", "triage"} // a hand-widened config
	sw, _ := newTestServer(t, wide, r, authOpts(true))

	for _, tc := range []struct {
		name string
		srv  *Server
		body string
		want string
	}{
		{"extract outside the node's set", s, textBody("tk-1", "extract", "Ada", extractParams()), `"extract" is not served by this node's text lane (text_tasks: classify)`},
		{"summarize", s, textBody("tk-2", "summarize", "some long text to summarize", map[string]any{"max_points": 3}), "never served on the text lane"},
		{"triage", s, textBody("tk-3", "triage", "some text", map[string]any{"question": "urgent?"}), "never served on the text lane"},
		{"summarize on a hand-widened node", sw, textBody("tk-4", "summarize", "some long text to summarize", nil), "never served on the text lane"},
		{"triage on a hand-widened node", sw, textBody("tk-5", "triage", "some text", nil), "never served on the text lane"},
		{"a vision task", s, textBody("tk-6", "vqa", "x", nil), "not one of classify, extract"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErrorShape(t, do(t, tc.srv, http.MethodPost, "/fleet/text", tc.body, nil), http.StatusBadRequest, tc.want)
		})
	}
	if _, ran := r.last(); ran {
		t.Fatal("a refused task must never reach the runner")
	}
	if rec := do(t, s, http.MethodGet, "/fleet/jobs/tk-1", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a task refused at ack left a job behind: %d", rec.Code)
	}
}

func TestTextPayloadValidation(t *testing.T) {
	r := &visionRunner{}
	s, _ := newTestServer(t, textCfg(""), r, authOpts(true))
	for _, tc := range []struct{ name, body, want string }{
		{"missing job id", textBody("", "classify", "x y z", classifyParams()), "job_id required"},
		{"empty input", textBody("tv-1", "classify", "  ", classifyParams()), "input is required"},
		{"one label", textBody("tv-2", "classify", "x", map[string]any{"labels": []string{"a"}}), "at least 2 labels"},
		{"no labels", textBody("tv-3", "classify", "x", nil), "requires params.labels"},
		{"stray classify param", textBody("tv-4", "classify", "x", map[string]any{"labels": []string{"a", "b"}, "schema": map[string]any{}}), "only params.labels"},
		{"no schema", textBody("tv-5", "extract", "x", nil), "requires params.schema"},
		{"stray extract param", textBody("tv-6", "extract", "x", map[string]any{"schema": map[string]any{"a": 1}, "labels": []string{"a"}}), "only params.schema"},
		{"unknown field", `{"job_id":"tv-7","task":"classify","input":"x","engine":"npu"}`, "malformed text body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErrorShape(t, do(t, s, http.MethodPost, "/fleet/text", tc.body, nil), http.StatusBadRequest, tc.want)
		})
	}
	if _, ran := r.last(); ran {
		t.Fatal("a refused payload must never reach the runner")
	}
	// The body cap is dispatch's 1 MiB.
	big := textBody("tv-8", "classify", strings.Repeat("a", TextBodyCap), classifyParams())
	wantErrorShape(t, do(t, s, http.MethodPost, "/fleet/text", big, nil), http.StatusBadRequest, "request body too large")
}

// TestTextIsConcurrencyCapped: the lane runs on the node's shared llama-swap endpoint.
func TestTextIsConcurrencyCapped(t *testing.T) {
	s, _ := newTestServer(t, textCfg(""), &visionRunner{}, authOpts(true))
	if !s.concurrencyCapped(TextTask) {
		t.Fatal("text must be capped: it contends for the text endpoint")
	}
}

// TestHealthPublishesTextTasksOnlyWhenSet: additive and omitempty, so a node that declares
// nothing (and every node that predates the field) publishes a byte-identical payload.
func TestHealthPublishesTextTasksOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tasks  []string
		wantIn bool
	}{
		{"unset", nil, false},
		{"empty", []string{}, false},
		{"set", []string{"classify", "extract"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := textCfg("")
			cfg.TextTasks = tc.tasks
			s, _ := newTestServer(t, cfg, &visionRunner{}, authOpts(true))
			h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
			got, has := h["text_tasks"]
			if has != tc.wantIn {
				t.Fatalf("text_tasks present = %v, want %v", has, tc.wantIn)
			}
			if tc.wantIn && string(mustJSON(t, got)) != `["classify","extract"]` {
				t.Fatalf("text_tasks = %s", mustJSON(t, got))
			}
		})
	}
}
