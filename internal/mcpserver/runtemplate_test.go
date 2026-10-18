package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// offload_run_template: one tool with an action enum (list, get, readiness, run). list, get and readiness read the node's own
// installed templates package through render/run-template.mjs; run converts the template on the machine's ComfyUI and goes through
// the run-graph path. The node side is a seam (Server.templateExec), so these tests read what the door asks of it and what it does
// with the answer; the gate itself (which template may run) is tested where it lives, in render/template-manifest.test.mjs.

// tplHarness is a server whose node seam answers from a table and whose pipeline records what it is handed.
type tplHarness struct {
	s        *Server
	calls    [][]string
	requests []core.Request
	// seen is what the pipeline hook read of the temp files the door made, at the moment the run was dispatched.
	manifestAtRun  string
	overridesAtRun string
}

func newTplHarness(t *testing.T, replies map[string]string) *tplHarness {
	t.Helper()
	h := &tplHarness{s: New(nil)}
	h.s.templateExec = func(_ context.Context, args []string) ([]byte, error) {
		h.calls = append(h.calls, append([]string(nil), args...))
		reply, ok := replies[args[0]]
		if !ok {
			return nil, errors.New("no canned reply for " + args[0])
		}
		if reply == "EXEC-ERROR" {
			return nil, errors.New("node exited 1: boom")
		}
		return []byte(reply), nil
	}
	h.s.runHook = func(_ context.Context, r core.Request) core.Result {
		h.requests = append(h.requests, r)
		if p, _ := r.Params["manifest_path"].(string); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				h.manifestAtRun = string(b)
			}
		}
		if p, _ := r.Params["overrides_path"].(string); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				h.overridesAtRun = string(b)
			}
		}
		return core.Result{OK: true, Data: json.RawMessage(`{"outputs":{"9":[{"path":"out.png","type":"output","kind":"image","width":8,"height":8}]},"image_path":"out.png","unverified_models":[]}`)}
	}
	return h
}

func (h *tplHarness) call(t *testing.T, args string) string {
	t.Helper()
	res, err := h.s.handleRunTemplate(context.Background(), callReq(args))
	if err != nil {
		t.Fatal(err)
	}
	return textOf(res)
}

const resolveOK = `{"ok":true,"outcome":"run","template":"tt_open","license":{"class":"permissive"},"reads_input":false,"input_files":[],"local_only":false,"fleet_note":"this machine runs a template on its own lane in this release; the fleet door takes an API-format graph","min_comfyui_version":null,"runtime_manifest":{"schema_version":1,"workflow":"tt_open","comfyui_min_version":"","node_packs":[],"models":[{"path":"models/vae/v.safetensors","source_url":"","sha256":null,"trust":"tofu"}]}}`

const resolveInput = `{"ok":true,"outcome":"run","template":"tt_input","license":{"class":"permissive"},"reads_input":true,"input_files":[{"node_type":"LoadImage","name":"ref.png"}],"local_only":true,"fleet_note":"this machine did not offer the template to the fleet: it reads input files (ref.png), and a node can match an input file only by name, so a same-named file of other bytes would run in its place","min_comfyui_version":null,"runtime_manifest":{"schema_version":1,"workflow":"tt_input","comfyui_min_version":"","node_packs":[],"models":[]}}`

// ---------------------------------------------------------------------------------------------------------------------
// The tool is advertised
// ---------------------------------------------------------------------------------------------------------------------

func TestRunTemplateToolIsAdvertisedWithAnActionEnum(t *testing.T) {
	var found map[string]any
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name != "offload_run_template" {
			continue
		}
		b, _ := json.Marshal(tool.InputSchema)
		_ = json.Unmarshal(b, &found)
	}
	if found == nil {
		t.Fatal("offload_run_template is not in tools/list")
	}
	props, _ := found["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	if got := asStrings(action["enum"]); !reflect.DeepEqual(got, []string{"list", "get", "readiness", "run"}) {
		t.Errorf("action enum = %v, want list, get, readiness, run", got)
	}
	if got := asStrings(found["required"]); !reflect.DeepEqual(got, []string{"action"}) {
		t.Errorf("required = %v, want [action]", got)
	}
	for _, name := range []string{"template", "kind", "license_class", "runnable", "reads_input", "limit", "offset", "overrides", "seed", "out_dir", "reserve_vram", "waiter_token"} {
		if _, ok := props[name]; !ok {
			t.Errorf("no %s parameter", name)
		}
	}
	for _, name := range []string{"route", "remotes", "graph_path", "graph_json", "manifest_path", "manifest_json", "devices"} {
		if _, ok := props[name]; ok {
			t.Errorf("%s must not be a parameter: a template is placed on this machine's lane, and the manifest is the harness's own", name)
		}
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// list, get, readiness
// ---------------------------------------------------------------------------------------------------------------------

func TestRunTemplateListPassesTheFiltersToTheNodeAndTheAnswerBack(t *testing.T) {
	h := newTplHarness(t, map[string]string{"list": `{"ok":true,"templates":[]}`})
	got := h.call(t, `{"action":"list","kind":"local","tag":"Image","text":"fox","license_class":"permissive","runnable":false,"reads_input":true,"limit":10,"offset":20}`)
	want := []string{"list", "--kind", "local", "--tag", "Image", "--text", "fox", "--license-class", "permissive", "--runnable", "false", "--reads-input", "true", "--limit", "10", "--offset", "20"}
	if len(h.calls) != 1 || !reflect.DeepEqual(h.calls[0], want) {
		t.Errorf("node call = %v, want %v", h.calls, want)
	}
	if got != `{"ok":true,"templates":[]}` {
		t.Errorf("the node's answer is the tool's answer: %q", got)
	}
	if len(h.requests) != 0 {
		t.Errorf("list ran nothing on the pipeline: %v", h.requests)
	}
}

func TestRunTemplateListWithNoFilterAsksForNoFilter(t *testing.T) {
	h := newTplHarness(t, map[string]string{"list": `{"ok":true}`})
	h.call(t, `{"action":"list"}`)
	if len(h.calls) != 1 || !reflect.DeepEqual(h.calls[0], []string{"list"}) {
		t.Errorf("node call = %v, want [list]", h.calls)
	}
}

func TestRunTemplateGetAndReadinessNameTheTemplate(t *testing.T) {
	h := newTplHarness(t, map[string]string{"get": `{"ok":true}`, "readiness": `{"ok":true,"ready":false}`})
	h.call(t, `{"action":"get","template":"tt_open"}`)
	h.call(t, `{"action":"readiness","template":"tt_open"}`)
	if want := [][]string{{"get", "tt_open"}, {"readiness", "tt_open"}}; !reflect.DeepEqual(h.calls, want) {
		t.Errorf("node calls = %v, want %v", h.calls, want)
	}
}

func TestRunTemplateGetAndReadinessAndRunNeedATemplate(t *testing.T) {
	for _, action := range []string{"get", "readiness", "run"} {
		h := newTplHarness(t, nil)
		out := h.call(t, `{"action":"`+action+`"}`)
		if !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "template") {
			t.Errorf("%s with no template: want a refusal naming template: %s", action, out)
		}
		if len(h.calls) != 0 || len(h.requests) != 0 {
			t.Errorf("%s with no template reached the node or the pipeline: %v %v", action, h.calls, h.requests)
		}
	}
}

func TestRunTemplateRefusesAnUnknownActionAndAMalformedTemplateName(t *testing.T) {
	h := newTplHarness(t, nil)
	if out := h.call(t, `{"action":"frobnicate"}`); !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "list, get, readiness or run") {
		t.Errorf("unknown action: %s", out)
	}
	if out := h.call(t, `{}`); !strings.Contains(out, "list, get, readiness or run") {
		t.Errorf("no action: %s", out)
	}
	for _, name := range []string{"../x", "a/b", "-x", "a b", ""} {
		out := h.call(t, `{"action":"get","template":`+quote(name)+`}`)
		if !strings.Contains(out, `"deferred":true`) {
			t.Errorf("template %q must be refused at the door: %s", name, out)
		}
	}
	if len(h.calls) != 0 {
		t.Errorf("nothing of that reached the node: %v", h.calls)
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestRunTemplateNodeFailureAndGarbageAreTypedDefers(t *testing.T) {
	h := newTplHarness(t, map[string]string{"get": "EXEC-ERROR", "readiness": "not json at all", "list": `["an","array"]`})
	for _, args := range []string{`{"action":"get","template":"t"}`, `{"action":"readiness","template":"t"}`, `{"action":"list"}`} {
		out := h.call(t, args)
		if !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "run-template") {
			t.Errorf("%s: want a deferral that names the run-template step: %s", args, out)
		}
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------------------------------------------------

func TestRunTemplateAPaidTemplateIsAnswered_NotDispatched(t *testing.T) {
	reply := `{"ok":false,"outcome":"defer","deferred":true,"class":"api_paid","code":"TEMPLATE_API_PAID","template":"api_x","detail":"a paid partner-node template spends money; the harness defers it and never falls back to cloud"}`
	h := newTplHarness(t, map[string]string{"resolve": reply})
	out := h.call(t, `{"action":"run","template":"api_x"}`)
	if out != reply {
		t.Errorf("the typed answer is the tool's answer: %s", out)
	}
	if len(h.requests) != 0 {
		t.Errorf("a paid template reached the pipeline: %v", h.requests)
	}
	if want := []string{"resolve", "api_x"}; len(h.calls) != 1 || !reflect.DeepEqual(h.calls[0], want) {
		t.Errorf("node calls = %v, want %v", h.calls, want)
	}
}

func TestRunTemplateRefusalsAreAnsweredNotDispatched(t *testing.T) {
	for _, class := range []string{"license_unknown", "license_conditional", "license_non_commercial", "flux_barred", "custom_nodes", "models_missing", "embedding_unverified", "inputs_missing", "comfyui_too_old", "unknown_template"} {
		reply := `{"ok":false,"outcome":"refuse","refused":true,"class":"` + class + `","code":"TEMPLATE_X","template":"t"}`
		h := newTplHarness(t, map[string]string{"resolve": reply})
		if out := h.call(t, `{"action":"run","template":"t"}`); out != reply {
			t.Errorf("%s: %s", class, out)
		}
		if len(h.requests) != 0 {
			t.Errorf("%s reached the pipeline", class)
		}
	}
}

func TestRunTemplateDispatchesThroughTheRunGraphPath(t *testing.T) {
	h := newTplHarness(t, map[string]string{"resolve": resolveOK})
	out := h.call(t, `{"action":"run","template":"tt_open","out_dir":"D:/out","reserve_vram":"1.5"}`)
	if len(h.requests) != 1 {
		t.Fatalf("requests = %v, want exactly one", h.requests)
	}
	r := h.requests[0]
	if r.Task != core.TaskRunGraph || r.Door != "offload_run_template" {
		t.Errorf("task %q door %q: a template run is a run-graph task through its own door", r.Task, r.Door)
	}
	if r.Params["template"] != "tt_open" || r.Params["out_dir"] != "D:/out" || r.Params["reserve_vram"] != "1.5" {
		t.Errorf("params = %v", r.Params)
	}
	for _, k := range []string{"graph_path", "overrides_path", "seed", "min_vram_gb", "devices", "route"} {
		if _, has := r.Params[k]; has {
			t.Errorf("param %s must be absent: %v", k, r.Params)
		}
	}
	var man map[string]any
	if err := json.Unmarshal([]byte(h.manifestAtRun), &man); err != nil || man["workflow"] != "tt_open" {
		t.Fatalf("the manifest the pipeline was handed (%q) is the runtime manifest of the resolve answer: %v", h.manifestAtRun, err)
	}
	models, _ := man["models"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["trust"] != "tofu" || models[0].(map[string]any)["source_url"] != "" {
		t.Errorf("manifest models = %v", man["models"])
	}
	if p, _ := r.Params["manifest_path"].(string); p == "" {
		t.Error("no manifest_path")
	} else if _, err := os.Stat(p); err == nil {
		t.Errorf("the manifest temp file %s outlived the call", p)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("answer is not JSON: %v: %s", err, out)
	}
	data, _ := got["result"].(map[string]any)
	if data["image_path"] != "out.png" || data["template"] != "tt_open" || data["license_class"] != "permissive" {
		t.Errorf("the run's result carries the template and its licence class beside the outputs: %v", data)
	}
	if data["fleet_note"] == nil || data["local_only"] != false {
		t.Errorf("the result says where it ran and why: %v", data)
	}
}

func TestRunTemplateCarriesSeedAndOverrides(t *testing.T) {
	h := newTplHarness(t, map[string]string{"resolve": resolveOK})
	h.call(t, `{"action":"run","template":"tt_open","seed":7,"overrides":{"6:2.text":"a blue fox","3.steps":30}}`)
	if len(h.requests) != 1 {
		t.Fatalf("requests = %v", h.requests)
	}
	r := h.requests[0]
	if r.Params["seed"] != "7" {
		t.Errorf("seed = %#v, want the string 7", r.Params["seed"])
	}
	var ov map[string]any
	if err := json.Unmarshal([]byte(h.overridesAtRun), &ov); err != nil || ov["6:2.text"] != "a blue fox" || ov["3.steps"] != float64(30) {
		t.Errorf("overrides file = %q (%v)", h.overridesAtRun, err)
	}
	if p, _ := r.Params["overrides_path"].(string); p == "" {
		t.Error("no overrides_path")
	} else if _, err := os.Stat(p); err == nil {
		t.Errorf("the overrides temp file %s outlived the call", p)
	}
}

func TestRunTemplateThreadsTheWaiterTokenSoAQueuedPlaceCanBeResumed(t *testing.T) {
	h := newTplHarness(t, map[string]string{"resolve": resolveOK})
	h.call(t, `{"action":"run","template":"tt_open","waiter_token":"tk-abc"}`)
	if len(h.requests) != 1 || h.requests[0].Params["waiter_token"] != "tk-abc" {
		t.Errorf("requests = %v", h.requests)
	}
	h = newTplHarness(t, map[string]string{"resolve": resolveOK})
	h.call(t, `{"action":"run","template":"tt_open"}`)
	if len(h.requests) != 1 {
		t.Fatalf("requests = %v", h.requests)
	}
	if _, has := h.requests[0].Params["waiter_token"]; has {
		t.Errorf("no token was given: %v", h.requests[0].Params)
	}
}

func TestRunTemplateAMalformedOverridesOrSeedIsRefusedAtTheDoor(t *testing.T) {
	for _, bad := range []string{`"overrides":[1,2]`, `"overrides":"x"`, `"seed":"abc"`, `"seed":-1`, `"seed":1.5`} {
		h := newTplHarness(t, map[string]string{"resolve": resolveOK})
		out := h.call(t, `{"action":"run","template":"tt_open",`+bad+`}`)
		field := "seed"
		if strings.HasPrefix(bad, `"overrides"`) {
			field = "overrides"
		}
		if !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, field) {
			t.Errorf("%s: want a refusal naming %s: %s", bad, field, out)
		}
		if len(h.requests) != 0 {
			t.Errorf("%s reached the pipeline", bad)
		}
	}
}

// A template that reads input files is a local-only ticket: it is never sent to the fleet. Every v1 run is on this machine's lane,
// and the door says why in the sentence of the node's own answer.
func TestRunTemplateThatReadsInputFilesStaysLocalAndSaysWhy(t *testing.T) {
	h := newTplHarness(t, map[string]string{"resolve": resolveInput})
	out := h.call(t, `{"action":"run","template":"tt_input"}`)
	if len(h.requests) != 1 {
		t.Fatalf("requests = %v", h.requests)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	data, _ := got["result"].(map[string]any)
	if data["local_only"] != true || !strings.Contains(data["fleet_note"].(string), "reads input files (ref.png)") {
		t.Errorf("result = %v", data)
	}
	if _, has := h.requests[0].Params["route"]; has {
		t.Errorf("a template is never routed: %v", h.requests[0].Params)
	}
}

func TestRunTemplateAnyRouteBeyondLocalIsRefusedWithTheReason(t *testing.T) {
	for _, extra := range []string{`"route":"auto"`, `"route":"remote"`, `"remotes":["http://x"]`, `"node":"b"`} {
		h := newTplHarness(t, map[string]string{"resolve": resolveInput})
		out := h.call(t, `{"action":"run","template":"tt_input",`+extra+`}`)
		if !strings.Contains(out, `"class":"fleet_not_offered"`) || !strings.Contains(out, "reads input files (ref.png)") {
			t.Errorf("%s: want the fleet_not_offered refusal with the input-file reason: %s", extra, out)
		}
		if len(h.requests) != 0 {
			t.Errorf("%s reached the pipeline", extra)
		}
	}
	h := newTplHarness(t, map[string]string{"resolve": resolveOK})
	if out := h.call(t, `{"action":"run","template":"tt_open","route":"remote"}`); !strings.Contains(out, `"class":"fleet_not_offered"`) {
		t.Errorf("a template with no input file is not placed on the fleet in this release either: %s", out)
	}
	h = newTplHarness(t, map[string]string{"resolve": resolveOK})
	h.call(t, `{"action":"run","template":"tt_open","route":"local"}`)
	if len(h.requests) != 1 {
		t.Errorf("route local is what a run is: %v", h.requests)
	}
}

func TestRunTemplateAPipelineDeferralIsReturnedAsItIs(t *testing.T) {
	h := newTplHarness(t, map[string]string{"resolve": resolveOK})
	h.s.runHook = func(context.Context, core.Request) core.Result {
		return core.Deferf("MODEL_SHA_MISMATCH: trust on first use: the file's sha256 is not the recorded one", "", core.Meta{})
	}
	out := h.call(t, `{"action":"run","template":"tt_open"}`)
	if !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "MODEL_SHA_MISMATCH") {
		t.Errorf("the typed defer reaches the caller: %s", out)
	}
}

func TestRunTemplateNoNodeScriptIsATypedDeferNotACrash(t *testing.T) {
	// The production seam (templateExec nil) on a server with no pipeline and no comfy_dir.
	s := New(nil)
	res, err := s.handleRunTemplate(context.Background(), callReq(`{"action":"list"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out := textOf(res); !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "comfy_dir") {
		t.Errorf("want a deferral that names comfy_dir: %s", out)
	}
}

// The tool is called over the protocol as a client calls it: the schema the server advertises accepts the arguments a caller sends
// (filters, an integer seed, an overrides object of strings, numbers and booleans), and they reach the handler.
func TestRunTemplateThroughTheProtocolAcceptsItsOwnSchema(t *testing.T) {
	h := newTplHarness(t, map[string]string{"list": `{"ok":true,"templates":[]}`, "resolve": resolveOK})
	if _, text := callOverTransport(t, h.s, "offload_run_template", `{"action":"list","kind":"local","runnable":false,"limit":5,"offset":0,"reads_input":true}`); !strings.Contains(text, `"templates"`) {
		t.Fatalf("list over the protocol: %s", text)
	}
	if want := []string{"list", "--kind", "local", "--runnable", "false", "--reads-input", "true", "--limit", "5", "--offset", "0"}; !reflect.DeepEqual(h.calls[0], want) {
		t.Errorf("node call = %v, want %v", h.calls[0], want)
	}
	callOverTransport(t, h.s, "offload_run_template", `{"action":"run","template":"tt_open","seed":7,"overrides":{"6.text":"a blue fox","3.steps":30,"1.on":true},"out_dir":"D:/o"}`)
	if len(h.requests) != 1 || h.requests[0].Params["seed"] != "7" || h.requests[0].Params["template"] != "tt_open" {
		t.Fatalf("run over the protocol: %v", h.requests)
	}
	var ov map[string]any
	if err := json.Unmarshal([]byte(h.overridesAtRun), &ov); err != nil || ov["1.on"] != true || ov["3.steps"] != float64(30) {
		t.Errorf("overrides = %q (%v)", h.overridesAtRun, err)
	}
}

// A template run is dispatched on route local, whatever fleet the box is configured with: route auto with a fleet and a busy lane plans
// from graph_path, which a template run does not have. The mutant that sent "auto" survived every other test because the harness server
// has no fleet; this one does, and a reachable-looking roster that is never contacted is the proof.
func TestRunTemplateDispatchesOnRouteLocalEvenWithAFleetConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.DelegateRemotes = []string{"http://192.0.2.10:18811"} // documentation address: nothing answers
	h := newTplHarness(t, map[string]string{"resolve": resolveOK})
	h.s.p = pipeline.New(cfg, nil, nil, nil)
	out := h.call(t, `{"action":"run","template":"tt_open"}`)
	if len(h.requests) != 1 {
		t.Fatalf("the run reached the run seam %d times, want 1 (a route other than local goes to the fleet planner, which has no graph_path to plan from): %s", len(h.requests), out)
	}
	if strings.Contains(out, "graph_path or graph_json required") || strings.Contains(out, "192.0.2.10") {
		t.Errorf("the fleet was consulted: %s", out)
	}
}
