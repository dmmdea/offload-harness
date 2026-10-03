package textremote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fakeNode is a fleet node that advertises the given tasks and text_tasks, records the text
// dispatch it receives, and answers the job with a core.Result.
type fakeNode struct {
	node    string
	tasks   []string
	ttasks  []string // health text_tasks; nil = not published
	leased  bool
	result  core.Result
	refuse  int // non-zero: answer the dispatch with this status
	mu      sync.Mutex
	payload map[string]any
	auth    string
	hdr     http.Header // header of the last dispatch (the attribution headers ride it)
	srv     *httptest.Server
}

func newFakeNode(t *testing.T, node string, tasks, ttasks []string, res core.Result) *fakeNode {
	f := &fakeNode{node: node, tasks: tasks, ttasks: ttasks, result: res}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		h := map[string]any{"node_id": node, "supported_task_types": tasks, "queue_depth": 0}
		if f.leased {
			h["lease"] = map[string]any{"held": true, "class": "text", "busy": true}
		}
		if f.ttasks != nil {
			h["text_tasks"] = f.ttasks
		}
		_ = json.NewEncoder(w).Encode(h)
	})
	mux.HandleFunc("POST /fleet/text", func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.payload = p
		f.auth = r.Header.Get("Authorization")
		f.hdr = r.Header.Clone()
		f.mu.Unlock()
		if f.refuse != 0 {
			w.WriteHeader(f.refuse)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "refused for the test"})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": p["job_id"], "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		data, _ := json.Marshal(f.result)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "done", "data": json.RawMessage(data)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNode) dispatchHeader() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hdr.Clone()
}

func (f *fakeNode) dispatched() (map[string]any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payload, f.auth
}

type localRunner struct {
	mu   sync.Mutex
	runs []core.Request
	res  core.Result
}

func (l *localRunner) Run(ctx context.Context, req core.Request) core.Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = append(l.runs, req)
	return l.res
}

func (l *localRunner) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.runs)
}

func setBusy(t *testing.T, busy bool) {
	t.Helper()
	prev := localBusy
	localBusy = func(config.Config) bool { return busy }
	t.Cleanup(func() { localBusy = prev })
}

func classifyReq() core.Request {
	return core.Request{Task: core.TaskClassify, Input: "the invoice is overdue by ten days",
		Params: map[string]any{"labels": []string{"billing", "support"}}}
}

func extractReq() core.Request {
	return core.Request{Task: core.TaskExtract, Input: "Ada Lovelace",
		Params: map[string]any{"schema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}}}
}

var remoteOK = core.Result{OK: true, Data: json.RawMessage(`{"label":"billing","confidence":0.93}`), Meta: core.Meta{Model: "npu-2b", LatencyMs: 4321}}

var localOK = core.Result{OK: true, Data: json.RawMessage(`{"label":"support","confidence":0.8}`)}

// route=local (and "") is byte-identical to a direct run: the runner's result, unstamped, and the
// wire untouched, even with the card busy and an eligible node listed.
func TestDefaultRouteIsLocalAndNeverTouchesTheWire(t *testing.T) {
	setBusy(t, true)
	node := newFakeNode(t, "rk-node", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: localOK}
	want := local.Run(context.Background(), classifyReq())
	for _, route := range []string{"", "local", "LOCAL"} {
		res := Run(context.Background(), cfg, local, classifyReq(), route)
		got, _ := json.Marshal(res)
		exp, _ := json.Marshal(want)
		if string(got) != string(exp) {
			t.Fatalf("route %q: %s, want byte-identical %s", route, got, exp)
		}
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatalf("local route dispatched to the node: %v", p)
	}
}

// remote ships the text and params, returns the node's result stamped with node and placement,
// and never runs the local cascade.
func TestRemoteShipsTheTextAndReturnsTheNodesResult(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "rk-node", []string{"text", "agent"}, []string{"classify", "extract"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL + "/"}
	cfg.FleetAuthToken = "tok"
	local := &localRunner{}

	res := Run(context.Background(), cfg, local, classifyReq(), "remote")
	if !res.OK || string(res.Data) != string(remoteOK.Data) {
		t.Fatalf("result = %+v, want the node's", res)
	}
	if res.Meta.Node != "rk-node" || res.Meta.Placement != "remote: forced" || res.Meta.Model != "npu-2b" {
		t.Fatalf("meta = %+v, want node + placement stamped over the node's meta", res.Meta)
	}
	if local.count() != 0 {
		t.Fatal("route remote must never run the local cascade")
	}
	p, auth := node.dispatched()
	if auth != "Bearer tok" {
		t.Fatalf("auth = %q, want the fleet token", auth)
	}
	params, _ := p["params"].(map[string]any)
	if p["task"] != "classify" || p["input"] != "the invoice is overdue by ten days" || params["labels"] == nil {
		t.Fatalf("payload = %v, want task + input + labels", p)
	}
	if id, _ := p["job_id"].(string); !strings.HasPrefix(id, "text-") {
		t.Fatalf("job_id = %q, want a text- id", id)
	}

	node.result = core.Result{OK: true, Data: json.RawMessage(`{"name":"Ada Lovelace"}`)}
	res = Run(context.Background(), cfg, local, extractReq(), "remote")
	if !res.OK {
		t.Fatalf("extract: %+v", res)
	}
	p, _ = node.dispatched()
	if params, _ := p["params"].(map[string]any); p["task"] != "extract" || params["schema"] == nil {
		t.Fatalf("extract payload = %v", p)
	}
}

// remote with no eligible node defers with the class the contract promises, naming what was
// probed, and never falls back to local: no lane (an older node, or a dark tier), a lane that does
// not list the task, a leased card, and no remotes at all.
func TestRemoteDefersNamingTheReasonWhenNoNodeIsEligible(t *testing.T) {
	setBusy(t, false)
	older := newFakeNode(t, "older-node", []string{"agent", "image-gen", "vision"}, nil, remoteOK)
	dark := newFakeNode(t, "dark-node", []string{"text"}, nil, remoteOK) // lists the lane, declares no task
	narrow := newFakeNode(t, "narrow-node", []string{"text"}, []string{"classify"}, remoteOK)
	leased := newFakeNode(t, "busy-node", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	leased.leased = true
	local := &localRunner{}

	cfg := config.Default()
	cfg.DelegateRemotes = []string{older.srv.URL, dark.srv.URL, narrow.srv.URL, leased.srv.URL}
	res := Run(context.Background(), cfg, local, extractReq(), "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result = %+v, want a capacity defer", res)
	}
	for _, want := range []string{"older-node", "no text lane", "dark-node", "does not serve extract", "text_tasks [classify]", "busy-node", "card leased"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("reason %q does not name %q", res.Reason, want)
		}
	}

	none := config.Default()
	res = Run(context.Background(), none, local, classifyReq(), "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassConfig || !strings.Contains(res.Reason, "delegate_remotes") {
		t.Fatalf("result = %+v, want a config defer naming delegate_remotes", res)
	}
	if local.count() != 0 {
		t.Fatal("route remote must never run the local cascade")
	}
	for _, n := range []*fakeNode{older, dark, narrow, leased} {
		if p, _ := n.dispatched(); p != nil {
			t.Fatalf("%s received a dispatch it is not eligible for: %v", n.node, p)
		}
	}
}

// pickNode skips a node lacking the lane or the task, and picks the one that has both.
func TestPickNodeSkipsNodesLackingTextOrTheTask(t *testing.T) {
	setBusy(t, false)
	older := newFakeNode(t, "older-node", []string{"vision"}, nil, remoteOK)
	narrow := newFakeNode(t, "narrow-node", []string{"text"}, []string{"classify"}, remoteOK)
	full := newFakeNode(t, "full-node", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{older.srv.URL, narrow.srv.URL, full.srv.URL}

	base, node, err := pickNode(context.Background(), cfg, "extract")
	if err != nil || node != "full-node" || base != full.srv.URL {
		t.Fatalf("extract: base %q node %q err %v, want full-node", base, node, err)
	}
	_, node, err = pickNode(context.Background(), cfg, "classify")
	if err != nil || (node != "narrow-node" && node != "full-node") {
		t.Fatalf("classify: node %q err %v, want a node that lists it", node, err)
	}
	if _, _, err = pickNode(context.Background(), cfg, "summarize"); err == nil {
		t.Fatal("summarize must never be placeable: no node lists it")
	}
	// An older node alone is never picked, for any task.
	only := config.Default()
	only.DelegateRemotes = []string{older.srv.URL}
	for _, task := range []string{"classify", "extract"} {
		if _, _, err := pickNode(context.Background(), only, task); err == nil {
			t.Fatalf("an older node (no text_tasks) was picked for %s", task)
		}
	}
}

func TestRemoteRefusalMapsToAClass(t *testing.T) {
	setBusy(t, false)
	for status, class := range map[int]string{503: core.DeferClassCapacity, 401: core.DeferClassInfrastructure, 400: core.DeferClassConfig} {
		node := newFakeNode(t, "rk-node", []string{"text"}, []string{"classify"}, remoteOK)
		node.refuse = status
		cfg := config.Default()
		cfg.DelegateRemotes = []string{node.srv.URL}
		res := Run(context.Background(), cfg, &localRunner{}, classifyReq(), "remote")
		if !res.Deferred || res.DeferClass != class || !strings.Contains(res.Reason, fmt.Sprintf("status %d", status)) {
			t.Fatalf("status %d: result = %+v, want a %s defer", status, res, class)
		}
	}
}

// auto on an idle card stays local, even with an eligible node on the roster.
func TestAutoRunsLocalOnAnIdleCard(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "rk-node", []string{"text"}, []string{"classify"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: localOK}
	res := Run(context.Background(), cfg, local, classifyReq(), "auto")
	if string(res.Data) != string(localOK.Data) || res.Meta.Placement != "local: gpu idle" {
		t.Fatalf("result = %+v, want the local result stamped idle", res)
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatal("auto without a held lease must not dispatch")
	}
}

// auto on a held lease goes to an eligible node; with none it still runs local, saying why.
func TestAutoGoesRemoteOnABusyCardAndFallsBackLocal(t *testing.T) {
	setBusy(t, true)
	node := newFakeNode(t, "rk-node", []string{"text"}, []string{"classify"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: localOK}

	res := Run(context.Background(), cfg, local, classifyReq(), "auto")
	if string(res.Data) != string(remoteOK.Data) || res.Meta.Placement != "remote: local gpu busy" || res.Meta.Node != "rk-node" {
		t.Fatalf("result = %+v, want the node's result stamped busy", res)
	}
	if local.count() != 0 {
		t.Fatal("busy + eligible node must not run local")
	}

	// A fleet where nothing advertises the lane (today's shipped state): queued-local wins.
	dark := newFakeNode(t, "dark-node", []string{"agent"}, nil, remoteOK)
	cfg.DelegateRemotes = []string{dark.srv.URL}
	res = Run(context.Background(), cfg, local, classifyReq(), "auto")
	if string(res.Data) != string(localOK.Data) || !strings.HasPrefix(res.Meta.Placement, "local: gpu busy, ") || !strings.Contains(res.Meta.Placement, "no text lane") {
		t.Fatalf("result = %+v, want the local result with the fallback reason", res)
	}
}

func TestWaitSurvivesATransientPollFailure(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "rk-node", []string{"text"}, []string{"classify"}, remoteOK)
	var flaky int32
	mux := http.NewServeMux()
	mux.Handle("/", node.srv.Config.Handler)
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&flaky, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		node.srv.Config.Handler.ServeHTTP(w, r)
	})
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{front.URL}
	res := Run(context.Background(), cfg, &localRunner{}, classifyReq(), "remote")
	if !res.OK || string(res.Data) != string(remoteOK.Data) {
		t.Fatalf("result = %+v, want the node's result after two failed polls", res)
	}
}

func TestWaitGivesUpOnAVanishedJob(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "rk-node", []string{"text"}, []string{"classify"}, remoteOK)
	mux := http.NewServeMux()
	mux.Handle("/", node.srv.Config.Handler)
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "unknown job"})
	})
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{front.URL}
	res := Run(context.Background(), cfg, &localRunner{}, classifyReq(), "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "denies holding") {
		t.Fatalf("result = %+v, want an infrastructure defer for the vanished job", res)
	}
}

// A task the lane cannot carry, or a body over the node's cap, is a contract defer before any node
// is probed.
func TestUnroutableRequestsAreContractDefers(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "rk-node", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	sum := core.Request{Task: core.TaskSummarize, Input: "some text", Params: map[string]any{}}
	res := Run(context.Background(), cfg, &localRunner{}, sum, "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "classify and extract") {
		t.Fatalf("summarize: %+v, want a contract defer", res)
	}
	big := classifyReq()
	big.Input = strings.Repeat("a", maxPayload)
	res = Run(context.Background(), cfg, &localRunner{}, big, "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "body cap") {
		t.Fatalf("oversize: %+v, want a contract defer naming the cap", res)
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatal("an unroutable request must never be dispatched")
	}
}

func TestUnrecognizedRouteIsAContractDefer(t *testing.T) {
	local := &localRunner{}
	res := Run(context.Background(), config.Default(), local, classifyReq(), "cloud")
	if !res.Deferred || res.DeferClass != core.DeferClassContract || local.count() != 0 {
		t.Fatalf("result = %+v (local ran %d), want a contract defer and no run", res, local.count())
	}
}

// The delegator does not take a node's OK result on trust: an answer outside the request's label
// set or 0..1 confidence, or an extract with a key outside the schema (or not one object), becomes
// a defer naming the node and the reason; a conforming answer, and a node's own defer, pass.
func TestNodeResultPostCheck(t *testing.T) {
	setBusy(t, false)
	ok := func(data string) core.Result { return core.Result{OK: true, Data: json.RawMessage(data)} }
	cases := []struct {
		name   string
		req    core.Request
		res    core.Result
		reason string // "" = accepted as is
	}{
		{"classify ok", classifyReq(), ok(`{"label":"billing","confidence":0.93}`), ""},
		{"classify label outside the set", classifyReq(), ok(`{"label":"refund","confidence":0.93}`), `label "refund"`},
		{"classify confidence 7", classifyReq(), ok(`{"label":"billing","confidence":7}`), "outside 0..1"},
		{"classify confidence -1", classifyReq(), ok(`{"label":"billing","confidence":-1}`), "outside 0..1"},
		{"classify no confidence", classifyReq(), ok(`{"label":"billing"}`), "no numeric confidence"},
		{"classify array", classifyReq(), ok(`[{"label":"billing","confidence":0.9}]`), "not one JSON object"},
		{"extract ok", extractReq(), ok(`{"name":"Ada Lovelace"}`), ""},
		{"extract key outside the schema", extractReq(), ok(`{"name":"Ada","age":36}`), `key "age"`},
		{"extract array", extractReq(), ok(`[{"name":"Ada"}]`), "not one JSON object"},
		{"a node's own defer passes through", classifyReq(), core.Deferf("seat down", "", core.Meta{}), ""},
	}
	for _, tc := range cases {
		node := newFakeNode(t, "rk-node", []string{"text"}, []string{"classify", "extract"}, tc.res)
		cfg := config.Default()
		cfg.DelegateRemotes = []string{node.srv.URL}
		res := Run(context.Background(), cfg, &localRunner{}, tc.req, "remote")
		if tc.reason == "" {
			if res.OK != tc.res.OK || res.Reason != tc.res.Reason {
				t.Errorf("%s: want the node's result unchanged, got %+v", tc.name, res)
			}
			continue
		}
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, "rk-node") || !strings.Contains(res.Reason, tc.reason) {
			t.Errorf("%s: want a defer naming the node and %q, got ok=%v deferred=%v reason=%q", tc.name, tc.reason, res.OK, res.Deferred, res.Reason)
		}
	}
}
