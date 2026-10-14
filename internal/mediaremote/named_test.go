package mediaremote

// A call that NAMES a node (ADR 0082 Amendment 3): `node` on a video, animation or audio call is a fleet node id (the node_id its
// health publishes, in any case) or one of the delegate_remotes URLs. The named node is the ONLY candidate: no other node and
// no local run is ever tried. A node that cannot take the call is refused by a typed word before anything is sent, and a node
// that is merely out of reach or text-leased is an ordinary capacity defer, because it may take the call later.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// ---- fixtures --------------------------------------------------------------------------------------

// mrow is one media_recipes row as a node publishes it.
func mrow(lane, name string, def bool, files ...any) map[string]any {
	var fs []any
	for i := 0; i+1 < len(files); i += 2 {
		fs = append(fs, map[string]any{"role": "w", "name": files[i], "bytes": files[i+1]})
	}
	return map[string]any{"lane": lane, "name": name, "default": def, "engine": "comfyui", "digest": "", "files": fs}
}

// publishes sets the node's media_recipes to rows (none at all: the key is absent, as on a node that predates it).
func publishes(rows ...map[string]any) func(map[string]any) {
	return func(m map[string]any) {
		if len(rows) == 0 {
			delete(m, "media_recipes")
			return
		}
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, r)
		}
		m["media_recipes"] = out
	}
}

var wanOK = mrow("video-gen", "wan22", true, "high.gguf", 11, "low.gguf", 22)

func namedClient(t *testing.T, nodes ...*node) config.Config {
	t.Helper()
	cfg := clientCfg(t, nodes...)
	cfg.FleetNodeID = "client-box"
	return cfg
}

func named(node string, extra map[string]any) core.Request {
	p := map[string]any{"node": node}
	for k, v := range extra {
		p[k] = v
	}
	return core.Request{Task: core.TaskGenerateVideo, Input: "a slow pan", Params: p}
}

func namedMusic(node string, extra map[string]any) core.Request {
	p := map[string]any{"node": node, "kind": "music"}
	for k, v := range extra {
		p[k] = v
	}
	return core.Request{Task: core.TaskGenerateAudio, Input: "a calm piano", Params: p}
}

func withLocalVideoLane(t *testing.T) {
	t.Helper()
	prev := routesFn
	routesFn = func(config.Config) []mediacap.Route {
		return []mediacap.Route{{Name: "generate_video", Engine: "test", State: mediacap.Configured}}
	}
	localRoutes.Reset()
	t.Cleanup(func() { routesFn = prev; localRoutes.Reset() })
}

// refusal reads the typed data a refused named call carries.
func refusal(t *testing.T, res core.Result) map[string]any {
	t.Helper()
	if res.OK || !res.Deferred {
		t.Fatalf("want a deferred answer, got %+v", res)
	}
	var m map[string]any
	if len(res.Data) == 0 || json.Unmarshal(res.Data, &m) != nil {
		t.Fatalf("a refusal must carry typed data, got %q (reason %s)", res.Data, res.Reason)
	}
	return m
}

func strs(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			out = append(out, x.(string))
		}
	}
	return out
}

func hasAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

// ---- who is named ----------------------------------------------------------------------------------

// The node is named by its id (any case) or by its roster URL, the call runs there, and only there.
func TestANamedNodeTakesTheCallByIdOrByURLAndNoOtherDoes(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a", healthEdit: publishes(wanOK)})
	b := startNode(t, nodeOpts{id: "render-b", healthEdit: publishes(wanOK)})
	cfg := namedClient(t, a, b)
	local := &recordingRunner{}
	for _, who := range []string{"render-b", "RENDER-B", " render-b ", b.srv.URL} {
		before := len(b.posts())
		res := Run(context.Background(), cfg, local, named(who, nil), "", nil)
		if !res.OK || res.Meta.Node != "render-b" {
			t.Fatalf("node %q: %+v", who, res)
		}
		if len(b.posts()) != before+1 {
			t.Errorf("node %q: the call must reach render-b once", who)
		}
	}
	if len(a.posts()) != 0 || local.count() != 0 {
		t.Errorf("only the named node may be tried: render-a got %d posts, the local runner %d runs", len(a.posts()), local.count())
	}
	res := Run(context.Background(), cfg, local, named("render-b", nil), "remote", nil)
	if !res.OK || !strings.Contains(res.Meta.Placement, "render-b") {
		t.Errorf("route remote with a node: %+v", res)
	}
}

// A blank node is no node: the call is the one that existed before the parameter did.
func TestABlankNodeIsNotNamed(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	cfg := namedClient(t, a)
	for _, blank := range []string{"", "   "} {
		local := &recordingRunner{}
		res := Run(context.Background(), cfg, local, named(blank, nil), "auto", nil)
		if !res.OK || res.Meta.Placement != "remote: no video lane on this machine" {
			t.Errorf("blank node %q must leave the placement alone: %+v", blank, res)
		}
	}
}

// Naming a node means leaving this machine: a box that has the lane itself still sends the call to the node, and without the
// node the same call runs here (the control that proves the lane exists).
func TestANamedNodeBypassesALocalLane(t *testing.T) {
	withLocalVideoLane(t)
	a := startNode(t, nodeOpts{id: "render-a", healthEdit: publishes(wanOK)})
	cfg := namedClient(t, a)
	cfg.VideoGenScript = writeFile(t, t.TempDir(), "comfy-video.mjs", []byte("//"))
	local := &recordingRunner{}
	if res := Run(context.Background(), cfg, local, video(nil), "auto", nil); !res.OK || local.count() != 1 {
		t.Fatalf("control: with the lane here and no node, the call runs here: %+v", res)
	}
	res := Run(context.Background(), cfg, local, named("render-a", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "render-a" || local.count() != 1 {
		t.Fatalf("a named node must take the call even when this machine has the lane: %+v (local runs %d)", res, local.count())
	}
}

func TestANodeWithRouteLocalIsAContractRefusalAndRunsNothing(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	cfg := namedClient(t, a)
	local := &recordingRunner{}
	res := Run(context.Background(), cfg, local, named("render-a", nil), "local", nil)
	if res.OK || res.DeferClass != core.DeferClassContract || !hasAll(res.Reason, "node", "route local") {
		t.Fatalf("%+v", res)
	}
	if local.count() != 0 || len(a.requests()) != 0 {
		t.Errorf("a contradiction runs nothing: local %d, node requests %d", local.count(), len(a.requests()))
	}
}

// node is carried by video, animation and audio calls. An image or a graph that carries one is refused by name, never run
// somewhere else with the word dropped.
func TestNodeOnAnImageOrAGraphCallIsRefusedByName(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	cfg := namedClient(t, a)
	local := &recordingRunner{}
	for name, req := range map[string]core.Request{
		"image": {Task: core.TaskGenerateImage, Input: "a door", Params: map[string]any{"node": "render-a"}},
		"graph": {Task: core.TaskRunGraph, Params: map[string]any{"node": "render-a", "graph_path": writeFile(t, t.TempDir(), "g.json", []byte(`{"1":{"class_type":"K"}}`))}},
	} {
		res := Run(context.Background(), cfg, local, req, "auto", nil)
		if res.OK || res.DeferClass != core.DeferClassContract || !hasAll(res.Reason, "node", "video", "animation", "audio") {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if local.count() != 0 || len(a.requests()) != 0 {
		t.Errorf("a refused call runs nothing: local %d, node requests %d", local.count(), len(a.requests()))
	}
}

func TestNodeMustBeAString(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	res := Run(context.Background(), namedClient(t, a), &recordingRunner{}, video(map[string]any{"node": 5}), "auto", nil)
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "node") {
		t.Fatalf("%+v", res)
	}
}

// ---- the typed refusals ----------------------------------------------------------------------------

// An unknown name is the caller's mistake: a contract refusal that lists the roster, before anything is sent.
func TestAnUnknownNodeIsAContractRefusalListingTheRoster(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	b := startNode(t, nodeOpts{id: "render-b"})
	res := Run(context.Background(), namedClient(t, a, b), &recordingRunner{}, named("render-z", nil), "", nil)
	data := refusal(t, res)
	if res.DeferClass != core.DeferClassContract || data["refusal"] != "node_unknown" || data["node"] != "render-z" || data["lane"] != "video-gen" {
		t.Fatalf("%+v data %v", res, data)
	}
	if got := strings.Join(strs(data["roster"]), ","); got != "render-a,render-b" {
		t.Errorf("roster = %q, want the ids of the nodes that answered, in roster order", got)
	}
	if len(a.posts())+len(b.posts()) != 0 {
		t.Error("nothing is sent for an unknown node")
	}
	if !strings.Contains(res.Reason, "render-a") || !strings.Contains(res.Reason, "render-b") {
		t.Errorf("the reason names the roster: %s", res.Reason)
	}
}

// A name that no node answered to may belong to a node that is down: that is not "unknown", it is a capacity defer naming what
// did not answer. And a URL that is not in delegate_remotes is the same contract refusal the remotes list gives.
func TestAnIdNoReachableNodeHasMayBeADownNodeAndIsACapacityDefer(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	cfg := namedClient(t, a)
	cfg.DelegateRemotes = append(cfg.DelegateRemotes, downURL)
	res := Run(context.Background(), cfg, &recordingRunner{}, named("render-z", nil), "", nil)
	if res.OK || res.DeferClass != core.DeferClassCapacity || !strings.Contains(res.Reason, downURL) {
		t.Fatalf("an id nobody answered to while an entry is down is a capacity defer naming it: %+v", res)
	}
	if strings.Contains(string(res.Data), "node_unknown") {
		t.Errorf("not unknown: %s", res.Data)
	}
	res = Run(context.Background(), cfg, &recordingRunner{}, named(downURL, nil), "", nil)
	if res.OK || res.DeferClass != core.DeferClassCapacity {
		t.Errorf("a named node that is down may come back: %+v", res)
	}
	if len(a.posts()) != 0 {
		t.Error("the live node must not be tried in its place")
	}
	res = Run(context.Background(), namedClient(t, a), &recordingRunner{}, named("http://192.0.2.77:18811", nil), "", nil)
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "delegate_remotes") {
		t.Errorf("a URL outside delegate_remotes: %+v", res)
	}
}

// A call may narrow the fleet with remotes and never extend it: a node outside this call's remotes is not on its roster.
func TestANodeOutsideTheCallsRemotesIsUnknown(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	b := startNode(t, nodeOpts{id: "render-b"})
	res := Run(context.Background(), namedClient(t, a, b), &recordingRunner{}, named("render-b", nil), "remote", []string{a.srv.URL})
	data := refusal(t, res)
	if res.DeferClass != core.DeferClassContract || data["refusal"] != "node_unknown" || strings.Join(strs(data["roster"]), ",") != "render-a" {
		t.Fatalf("%+v data %v", res, data)
	}
	if len(a.posts())+len(b.posts()) != 0 {
		t.Error("nothing is sent")
	}
}

// A URL typed with a credential in front of its host is redacted wherever the call prints or files it: the refusal's reason and data,
// and the placement, the way every roster entry is.
func TestAUrlNamedWithACredentialIsNeverEchoed(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	cfg := namedClient(t, a)
	for name, res := range map[string]core.Result{
		"unknown":     Run(context.Background(), cfg, &recordingRunner{}, named("http://operator:hunter2@192.0.2.9:18811/", nil), "", nil),
		"route local": Run(context.Background(), cfg, &recordingRunner{}, named("http://operator:hunter2@192.0.2.9:18811/", nil), "local", nil),
	} {
		if res.OK || strings.Contains(res.Reason+string(res.Data)+res.Meta.Placement, "hunter2") {
			t.Errorf("%s: the credential is echoed: %+v", name, res)
		}
		if !strings.Contains(res.Reason, "192.0.2.9") {
			t.Errorf("%s: the refusal still names the node it refused: %s", name, res.Reason)
		}
	}
}

func TestTheNodeThatIsThisMachineIsRefused(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	cfg := namedClient(t, a)
	cfg.FleetNodeID = "Render-A"
	for _, who := range []string{"render-a", a.srv.URL} {
		res := Run(context.Background(), cfg, &recordingRunner{}, named(who, nil), "", nil)
		data := refusal(t, res)
		if res.DeferClass != core.DeferClassContract || data["refusal"] != "node_is_self" {
			t.Errorf("node %q: %+v data %v", who, res, data)
		}
	}
	if len(a.posts()) != 0 {
		t.Error("this machine's own node is never sent a call from this machine")
	}
}

// A node that does not serve the lane, or whose route is not CONFIGURED, is refused before any POST. The route is the node's own
// verdict: a node whose weights were deleted reports BOUND-BUT-MISSING, and the refusal names the files from its rows.
func TestANamedNodeWithoutTheRouteIsRefusedBeforeAnyPOST(t *testing.T) {
	noRoute := startNode(t, nodeOpts{id: "render-a", cfg: func(c *config.Config) { c.VideoGenScript = "" }, healthEdit: publishes()})
	res := Run(context.Background(), namedClient(t, noRoute), &recordingRunner{}, named("render-a", nil), "", nil)
	data := refusal(t, res)
	if res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass != core.ErrClassNoNodeConfigured || data["refusal"] != "node_lacks_route" || data["lane"] != "video-gen" {
		t.Fatalf("a node that does not serve video-gen: %+v data %v", res, data)
	}
	if len(noRoute.posts()) != 0 {
		t.Error("nothing is sent to a node that cannot take the call")
	}

	gone := startNode(t, nodeOpts{id: "render-b",
		routes: func(config.Config) []mediacap.Route {
			return []mediacap.Route{{Name: "generate_video", Engine: "test", State: mediacap.BoundButMissing}}
		},
		healthEdit: publishes(mrow("video-gen", "wan22", true, "high.gguf", -1, "low.gguf", 22, "vae.safetensors", -1)),
	})
	res = Run(context.Background(), namedClient(t, gone), &recordingRunner{}, named("render-b", nil), "", nil)
	data = refusal(t, res)
	if data["refusal"] != "node_lacks_route" || data["family"] != "wan22" {
		t.Fatalf("a node whose weights are gone: %+v data %v", res, data)
	}
	if got := strings.Join(strs(data["missing"]), ","); got != "high.gguf,vae.safetensors" {
		t.Errorf("missing = %q, want the files the node's own row says it lacks", got)
	}
	if !hasAll(res.Reason, "BOUND-BUT-MISSING", "high.gguf") || len(gone.posts()) != 0 {
		t.Errorf("reason %q, posts %d", res.Reason, len(gone.posts()))
	}
}

// A node that does not list the lane's task is refused even when its route reads CONFIGURED, and an older node that publishes no routes
// is refused for the task alone: the task list is the node's own statement of what it serves.
func TestANamedNodeThatDoesNotListTheTaskIsRefused(t *testing.T) {
	dropTask := func(m map[string]any) {
		var kept []any
		for _, x := range m["supported_task_types"].([]any) {
			if x != "video-gen" {
				kept = append(kept, x)
			}
		}
		m["supported_task_types"] = kept
	}
	configured := startNode(t, nodeOpts{id: "render-a", healthEdit: dropTask})
	older := startNode(t, nodeOpts{id: "render-b", healthEdit: func(m map[string]any) { dropTask(m); delete(m, "media_routes") }})
	cfg := namedClient(t, configured, older)
	for _, id := range []string{"render-a", "render-b"} {
		res := Run(context.Background(), cfg, &recordingRunner{}, named(id, nil), "", nil)
		data := refusal(t, res)
		if data["refusal"] != "node_lacks_route" || !strings.Contains(res.Reason, "does not serve video-gen") {
			t.Errorf("%s: %+v data %v", id, res, data)
		}
	}
	if len(configured.posts())+len(older.posts()) != 0 {
		t.Error("nothing is sent")
	}
}

// Input files travel through the media-job door. A named node that has not opened it cannot take a still, and says so; the same
// node takes a call with no input file, and a node with the door open takes the still.
func TestANamedNodeWithoutTheMediaJobDoorRefusesAStill(t *testing.T) {
	closed := startNode(t, nodeOpts{id: "render-a", healthEdit: publishes(wanOK)})
	open := startNode(t, nodeOpts{id: "render-b", mediaInputs: true, healthEdit: publishes(wanOK)})
	cfg := namedClient(t, closed, open)
	still := writeFile(t, t.TempDir(), "s.png", png)
	res := Run(context.Background(), cfg, &recordingRunner{}, named("render-a", map[string]any{"still": still}), "", nil)
	data := refusal(t, res)
	if res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass != core.ErrClassNoNodeConfigured || data["refusal"] != "node_lacks_media_job" {
		t.Fatalf("%+v data %v", res, data)
	}
	if !strings.Contains(res.Reason, "media-job") || len(closed.posts()) != 0 {
		t.Errorf("reason %q, posts %d", res.Reason, len(closed.posts()))
	}
	if res := Run(context.Background(), cfg, &recordingRunner{}, named("render-a", nil), "", nil); !res.OK {
		t.Errorf("the same node takes a call with no input file: %+v", res)
	}
	if res := Run(context.Background(), cfg, &recordingRunner{}, named("render-b", map[string]any{"still": still}), "", nil); !res.OK || res.Meta.Node != "render-b" {
		t.Errorf("a node with the door open takes the still: %+v", res)
	}
}

// ---- the weight: node_lacks_model ------------------------------------------------------------------

func TestANamedNodeLackingTheModelIsRefusedBeforeAnyPOSTNamingTheFiles(t *testing.T) {
	b := startNode(t, nodeOpts{id: "render-b", healthEdit: publishes(
		wanOK,
		mrow("video-gen", "ltx25", false, "ltx-transformer.safetensors", 100, "ltx-vae.safetensors", -1, "ltx-audio-vae.safetensors", -1),
	)})
	cfg := namedClient(t, b)
	res := Run(context.Background(), cfg, &recordingRunner{}, named("render-b", map[string]any{"model": "ltx25"}), "", nil)
	data := refusal(t, res)
	if res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass != core.ErrClassNoNodeConfigured ||
		data["refusal"] != "node_lacks_model" || data["family"] != "ltx25" || data["node"] != "render-b" || data["lane"] != "video-gen" {
		t.Fatalf("%+v data %v", res, data)
	}
	if got := strings.Join(strs(data["missing"]), ","); got != "ltx-vae.safetensors,ltx-audio-vae.safetensors" {
		t.Errorf("missing = %q", got)
	}
	if len(b.posts()) != 0 {
		t.Error("a refusal sends nothing and creates no job on the node")
	}
	// A family the node publishes no row for at all.
	res = Run(context.Background(), cfg, &recordingRunner{}, named("render-b", map[string]any{"model": "hunyuan"}), "", nil)
	data = refusal(t, res)
	if data["refusal"] != "node_lacks_model" || data["family"] != "hunyuan" || len(strs(data["missing"])) != 0 {
		t.Errorf("no row for the family: %+v data %v", res, data)
	}
	if len(b.posts()) != 0 {
		t.Error("nothing sent")
	}
}

// With no model the node's DEFAULT row renders: a default whose file is missing is refused, and one that is whole runs and the
// answer says which family and which files.
func TestWithNoModelTheNodesDefaultRowDecidesAndTheAnswerSaysWhichRendered(t *testing.T) {
	broken := startNode(t, nodeOpts{id: "render-a", healthEdit: publishes(mrow("video-gen", "wan22", true, "high.gguf", 11, "low.gguf", -1))})
	res := Run(context.Background(), namedClient(t, broken), &recordingRunner{}, named("render-a", nil), "", nil)
	data := refusal(t, res)
	if data["refusal"] != "node_lacks_model" || strings.Join(strs(data["missing"]), ",") != "low.gguf" || len(broken.posts()) != 0 {
		t.Fatalf("a default row with a missing file: %+v data %v", res, data)
	}

	// The default row is the one marked default, wherever it is listed.
	whole := startNode(t, nodeOpts{id: "render-b", healthEdit: publishes(mrow("video-gen", "ltx25", false, "x.safetensors", 1), wanOK)})
	res = Run(context.Background(), namedClient(t, whole), &recordingRunner{}, named("render-b", nil), "", nil)
	if !res.OK || len(whole.posts()) != 1 {
		t.Fatalf("%+v", res)
	}
	got := decode(t, res)
	if got["rendered_family"] != "wan22" {
		t.Errorf("rendered_family = %v, want the default row's name", got["rendered_family"])
	}
	if files := strs(got["rendered_files"]); strings.Join(files, ",") != "high.gguf,low.gguf" {
		t.Errorf("rendered_files = %v", got["rendered_files"])
	}
	if got["video_path"] == nil {
		t.Errorf("the node's own answer must survive: %v", got)
	}
}

// The model is folded the way the node folds it: the runner's closed set renders itself, every other spelling (the documented
// "wan" included) renders Wan 2.2, and ace has no row to judge.
func TestTheModelFoldsTheWayTheNodeFoldsIt(t *testing.T) {
	b := startNode(t, nodeOpts{id: "render-b", healthEdit: publishes(wanOK, mrow("video-gen", "ltx25", false, "x.safetensors", 1))})
	cfg := namedClient(t, b)
	for model, want := range map[string]string{"wan": "wan22", "wan22": "wan22", "ltx25": "ltx25", "ace": ""} {
		res := Run(context.Background(), cfg, &recordingRunner{}, named("render-b", map[string]any{"model": model}), "", nil)
		if !res.OK {
			t.Errorf("model %q: %+v", model, res)
			continue
		}
		if got := decode(t, res)["rendered_family"]; (want == "" && got != nil) || (want != "" && got != want) {
			t.Errorf("model %q: rendered_family = %v, want %q", model, got, want)
		}
	}
}

// An sd.cpp family is a row under its own name and a model is matched against it exactly, before any folding: "fastwan" is not
// Wan 2.2 on a node that publishes it.
func TestAnSdcppFamilyIsMatchedByItsOwnName(t *testing.T) {
	sd := func(name string, files ...any) map[string]any {
		r := mrow("video-gen", name, false, files...)
		r["engine"] = "sdcpp"
		return r
	}
	b := startNode(t, nodeOpts{id: "render-b", healthEdit: publishes(wanOK, sd("fastwan", "fastwan.gguf", 7))})
	res := Run(context.Background(), namedClient(t, b), &recordingRunner{}, named("render-b", map[string]any{"model": "fastwan"}), "", nil)
	if !res.OK || decode(t, res)["rendered_family"] != "fastwan" {
		t.Fatalf("%+v", res)
	}
	broken := startNode(t, nodeOpts{id: "render-c", healthEdit: publishes(wanOK, sd("fastwan", "fastwan.gguf", -1, "vae.safetensors", 3))})
	res = Run(context.Background(), namedClient(t, broken), &recordingRunner{}, named("render-c", map[string]any{"model": "fastwan"}), "", nil)
	data := refusal(t, res)
	if data["refusal"] != "node_lacks_model" || data["family"] != "fastwan" || strings.Join(strs(data["missing"]), ",") != "fastwan.gguf" {
		t.Errorf("%+v data %v", res, data)
	}
}

// A lane the node publishes no row for is unknown, not refused: an older node, a node whose animate lane is sd.cpp (rows only
// for video), voice (no row in this release) and voice=endpoint all go on to the node's own door.
func TestALaneWithoutARowIsUnknownAndProceeds(t *testing.T) {
	older := startNode(t, nodeOpts{id: "render-a", healthEdit: publishes()})
	if res := Run(context.Background(), namedClient(t, older), &recordingRunner{}, named("render-a", nil), "", nil); !res.OK {
		t.Errorf("an older node: %+v", res)
	}
	if got := decode(t, Run(context.Background(), namedClient(t, older), &recordingRunner{}, named("render-a", nil), "", nil)); got["rendered_family"] != nil {
		t.Errorf("no row, no claim about what rendered: %v", got)
	}
	if res := Run(context.Background(), namedClient(t, older), &recordingRunner{}, named("render-a", map[string]any{"model": "ltx25"}), "", nil); !res.OK {
		t.Errorf("an older node is not judged for a model either: %+v", res)
	}

	videoOnly := startNode(t, nodeOpts{id: "render-b", mediaInputs: true, healthEdit: publishes(mrow("video-gen", "wan22", true, "high.gguf", -1))})
	cfg := namedClient(t, videoOnly)
	ref, driver := writeFile(t, t.TempDir(), "r.png", png), writeFile(t, t.TempDir(), "d.mp4", mp4)
	anim := core.Request{Task: core.TaskAnimateCharacter, Input: "a fox", Params: map[string]any{"node": "render-b", "ref": ref, "driver": driver}}
	if res := Run(context.Background(), cfg, &recordingRunner{}, anim, "", nil); !res.OK {
		t.Errorf("a node with video rows and none for animate is unknown for animate: %+v", res)
	}
	musicOnly := startNode(t, nodeOpts{id: "render-c", cfg: func(c *config.Config) { c.TTSEndpoint = "http://192.0.2.5:8000" },
		healthEdit: publishes(mrow("audio-gen", "acestep", true, "unet.safetensors", -1))})
	cfg = namedClient(t, musicOnly)
	for _, p := range []map[string]any{{"node": "render-c"}, {"node": "render-c", "voice": "generalist"}, {"node": "render-c", "voice": "endpoint"}} {
		voice := core.Request{Task: core.TaskGenerateAudio, Input: "hola", Params: p}
		if res := Run(context.Background(), cfg, &recordingRunner{}, voice, "", nil); !res.OK {
			t.Errorf("voice %v is never refused for a music row: %+v", p, res)
		}
	}
	res := Run(context.Background(), cfg, &recordingRunner{}, namedMusic("render-c", nil), "", nil)
	if data := refusal(t, res); data["refusal"] != "node_lacks_model" || data["lane"] != "audio-gen" || data["family"] != "acestep" {
		t.Errorf("music with a missing ACE-Step file: %+v data %v", res, data)
	}
}

func TestAnimateLacksTheModelWhenItsRowHasAMissingFile(t *testing.T) {
	b := startNode(t, nodeOpts{id: "render-b", mediaInputs: true, healthEdit: publishes(mrow("animate", "wan-animate2", true, "unet.safetensors", 1, "vae.safetensors", -1))})
	ref, driver := writeFile(t, t.TempDir(), "r.png", png), writeFile(t, t.TempDir(), "d.mp4", mp4)
	anim := core.Request{Task: core.TaskAnimateCharacter, Input: "a fox", Params: map[string]any{"node": "render-b", "ref": ref, "driver": driver}}
	res := Run(context.Background(), namedClient(t, b), &recordingRunner{}, anim, "", nil)
	data := refusal(t, res)
	if data["refusal"] != "node_lacks_model" || data["lane"] != "animate" || strings.Join(strs(data["missing"]), ",") != "vae.safetensors" || len(b.posts()) != 0 {
		t.Fatalf("%+v data %v", res, data)
	}
}

// ---- no fallback -----------------------------------------------------------------------------------

// The named node refuses, another node could take the call, this machine could too: neither is tried.
func TestANamedNodeNeverFallsBackToAnotherNodeOrToThisMachine(t *testing.T) {
	withLocalVideoLane(t)
	capable := startNode(t, nodeOpts{id: "render-a", healthEdit: publishes(wanOK)})
	lacking := startNode(t, nodeOpts{id: "render-b", healthEdit: publishes(mrow("video-gen", "wan22", true, "high.gguf", -1))})
	cfg := namedClient(t, capable, lacking)
	cfg.VideoGenScript = writeFile(t, t.TempDir(), "comfy-video.mjs", []byte("//"))
	local := &recordingRunner{}
	res := Run(context.Background(), cfg, local, named("render-b", nil), "", nil)
	if res.OK || refusal(t, res)["refusal"] != "node_lacks_model" {
		t.Fatalf("%+v", res)
	}
	if len(capable.posts()) != 0 || len(lacking.posts()) != 0 || local.count() != 0 {
		t.Errorf("no fallback: capable %d posts, lacking %d posts, local runs %d", len(capable.posts()), len(lacking.posts()), local.count())
	}
}

// A node holding a text lease, or one that does not answer, is not refused: it may take the call later.
func TestATextLeasedNamedNodeIsACapacityDeferNotARefusal(t *testing.T) {
	leased := startNode(t, nodeOpts{id: "render-a", healthEdit: func(m map[string]any) {
		publishes(wanOK)(m)
		m["lease"] = map[string]any{"held": true, "class": "text", "pid": 7, "reason": "chat", "remaining_sec": 60}
	}})
	res := Run(context.Background(), namedClient(t, leased), &recordingRunner{}, named("render-a", nil), "", nil)
	if res.OK || res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass == core.ErrClassNoNodeConfigured || !strings.Contains(res.Reason, "text lease") {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(string(res.Data), `"refusal"`) || len(leased.posts()) != 0 {
		t.Errorf("a lease is not a refusal and nothing is sent: data %s posts %d", res.Data, len(leased.posts()))
	}
}

// ---- min_vram_gb on video and animation ------------------------------------------------------------

// The caller's min_vram_gb filters the remote candidates of a video or animation call, as it does a graph's: the largest card of
// the node must be at least that many GB (a 16 GB card reads 15.92 GiB). 0 lifts nothing because nothing is asked.
func TestMinVRAMGBFiltersTheNodesOfAVideoCall(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"}) // one 15.92 GiB card
	cfg := namedClient(t, a)
	for floor, ok := range map[any]bool{16: true, 16.0: true, "16": true, 0: true, 24: false, "24": false} {
		res := Run(context.Background(), cfg, &recordingRunner{}, video(map[string]any{"min_vram_gb": floor}), "remote", nil)
		if res.OK != ok {
			t.Errorf("min_vram_gb %v: ok=%v, want %v (%s)", floor, res.OK, ok, res.Reason)
		}
		if !ok && !hasAll(res.Reason, "render-a", "24 GB") {
			t.Errorf("the miss names the node and the floor: %s", res.Reason)
		}
	}
}

func TestMinVRAMGBFiltersTheNodesOfAnAnimationCall(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a", mediaInputs: true})
	cfg := namedClient(t, a)
	ref, driver := writeFile(t, t.TempDir(), "r.png", png), writeFile(t, t.TempDir(), "d.mp4", mp4)
	for floor, ok := range map[any]bool{16: true, 32: false} {
		anim := core.Request{Task: core.TaskAnimateCharacter, Input: "a fox", Params: map[string]any{"ref": ref, "driver": driver, "min_vram_gb": floor}}
		if res := Run(context.Background(), cfg, &recordingRunner{}, anim, "remote", nil); res.OK != ok {
			t.Errorf("min_vram_gb %v: ok=%v, want %v (%s)", floor, res.OK, ok, res.Reason)
		}
	}
}

// A min_vram_gb that is not a number of GB is the caller's mistake on every route: refused by name before the lane is asked.
func TestAMalformedMinVRAMGBIsRefusedByNameOnEveryRouteForVideo(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a"})
	cfg := namedClient(t, a)
	for _, route := range []string{"", "local", "auto", "remote"} {
		for _, bad := range []any{"sixteen", -1, true} {
			local := &recordingRunner{}
			res := Run(context.Background(), cfg, local, video(map[string]any{"min_vram_gb": bad}), route, nil)
			if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, "min_vram_gb") || local.count() != 0 {
				t.Errorf("route %q min_vram_gb %v: %+v (local runs %d)", route, bad, res, local.count())
			}
		}
	}
}

// Named and too small: the node is refused by a word of its own, before any POST.
func TestANamedNodeBelowMinVRAMGBIsRefusedBeforeAnyPOST(t *testing.T) {
	a := startNode(t, nodeOpts{id: "render-a", healthEdit: publishes(wanOK)})
	res := Run(context.Background(), namedClient(t, a), &recordingRunner{}, named("render-a", map[string]any{"min_vram_gb": 24}), "", nil)
	data := refusal(t, res)
	if res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass != core.ErrClassNoNodeConfigured || data["refusal"] != "node_below_min_vram" || len(a.posts()) != 0 {
		t.Fatalf("%+v data %v", res, data)
	}
	if !hasAll(res.Reason, "24 GB") {
		t.Errorf("the reason names the floor: %s", res.Reason)
	}
	if res := Run(context.Background(), namedClient(t, a), &recordingRunner{}, named("render-a", map[string]any{"min_vram_gb": 16}), "", nil); !res.OK {
		t.Errorf("a 16 GB card meets 16: %+v", res)
	}
}

// ---- nothing changes without a node ----------------------------------------------------------------

// The rows are read only for a call that names a node. A forced call with a model, or a thin client's auto, goes where it always
// went, whatever the nodes' rows say.
func TestACallWithoutANodeNeverReadsTheRows(t *testing.T) {
	b := startNode(t, nodeOpts{id: "render-b", healthEdit: publishes(mrow("video-gen", "wan22", true, "high.gguf", -1))})
	cfg := namedClient(t, b)
	for _, route := range []string{"remote", "auto"} {
		res := Run(context.Background(), cfg, &recordingRunner{}, video(map[string]any{"model": "ltx25"}), route, nil)
		if !res.OK || res.Meta.Node != "render-b" {
			t.Errorf("route %q: a call without a node is placed as it always was: %+v", route, res)
		}
		if data := decode(t, res); data["rendered_family"] != nil || data["rendered_files"] != nil {
			t.Errorf("route %q: only a named call is annotated: %v", route, data)
		}
	}
}
