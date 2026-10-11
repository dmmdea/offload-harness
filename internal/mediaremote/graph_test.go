package mediaremote

// A graph on the fleet (P0 plan, S4; ADR 0082 amendment). A busy local lane places a whole-node run_graph on an idle
// node, and the node decides at the door whether it holds every file the graph names (its 503 moves the call on, like the
// image path's bounce). Two rules protect the cards: a whole-node graph runs in the default ComfyUI instance, which sees
// every card, so it is placed only on a node that has ONE card (never on the display card of a multi-GPU box), and a call
// can ask for a card of at least N GB (min_vram_gb), with a default floor for Wan 2.2 at 720p and above. Everything runs
// against real fleet nodes (their doors, their ACK check, their job store) and a delegator whose pipeline is a runner.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// withCards makes a node publish gpu_devices with one card of each total (GiB, as nvidia-smi reports them: a 16 GB card
// reads 15.92). No totals deletes the key: a node that publishes no per-card list.
func withCards(totals ...float64) func(map[string]any) {
	return func(m map[string]any) {
		if len(totals) == 0 {
			delete(m, "gpu_devices")
			return
		}
		var rows []map[string]any
		for i, g := range totals {
			rows = append(rows, map[string]any{"index": i, "uuid": fmt.Sprintf("GPU-0000000%d", i), "name": "test card", "vram_total_gb": g, "vram_free_gb": g - 1})
		}
		m["gpu_devices"] = rows
	}
}

const (
	krea2Graph = `{"3":{"class_type":"UNETLoader","inputs":{"unet_name":"krea2_turbo_bf16.safetensors"}},"4":{"class_type":"KSampler","inputs":{"model":["3",0]}}}`
	plainGraph = `{"1":{"class_type":"KSampler","inputs":{"seed":1}}}`
	// wanFilesGraph names two files with real, long names.
	wanFilesGraph = `{"3":{"class_type":"UNETLoader","inputs":{"unet_name":"wan/wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors"}},"4":{"class_type":"CLIPLoader","inputs":{"clip_name":"umt5_xxl_fp8_e4m3fn_scaled.safetensors"}}}`
)

// graphTreeWith is a ComfyUI tree holding exactly the given files (relative to the tree).
func graphTreeWith(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const krea2File = "models/diffusion_models/krea2_turbo_bf16.safetensors"

// startGraphNode starts a fleet node that runs graphs on a tree holding files, publishing one card per total.
func startGraphNode(t *testing.T, id string, files []string, cards ...float64) *node {
	t.Helper()
	tree := graphTreeWith(t, files...)
	return startNode(t, nodeOpts{id: id, version: fixtureVersion, healthEdit: withCards(cards...),
		cfg: func(c *config.Config) { c.ComfyDir = tree }})
}

// graphClient is a delegator that has the graph lane and the given fleet, with the clean placer and this release.
func graphClient(t *testing.T, nodes ...*node) config.Config {
	t.Helper()
	useVersion(t)
	prevHost := hostnameFn
	hostnameFn = func() (string, error) { return "delegator-host", nil }
	t.Cleanup(func() { hostnameFn = prevHost })
	resetPlacer()
	t.Cleanup(resetPlacer)
	freshQueue(t)
	freshRoster(t)
	stubLocalRoutes(t, configured("run_graph"))
	return clientCfg(t, nodes...)
}

func graphReqOf(t *testing.T, graph string, extra map[string]any) core.Request {
	t.Helper()
	p := map[string]any{
		"graph_path": writeFile(t, t.TempDir(), "g.json", []byte(graph)),
		"out_dir":    filepath.Join(t.TempDir(), "outs"),
	}
	for k, v := range extra {
		p[k] = v
	}
	return core.Request{Task: core.TaskRunGraph, Door: "offload_run_graph", Params: p, Resumable: true}
}

func graphWith(height int) string {
	return fmt.Sprintf(`{"1":{"class_type":"WanImageToVideo","inputs":{"width":720,"height":%d,"length":81}},"2":{"class_type":"KSampler","inputs":{"seed":1}}}`, height)
}

// payloadOf decodes the last dispatch a node received.
func graphPayload(t *testing.T, n *node) map[string]json.RawMessage {
	t.Helper()
	ps := n.posts()
	if len(ps) == 0 {
		t.Fatal("the node received no POST")
	}
	var wire struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(ps[len(ps)-1].body, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Payload
}

// THE S4 SHAPE. The local lane is held; an idle node with one card holds every file the graph names; the graph goes there
// and its outputs come back hash-checked into the caller's out_dir. The graph travels as it is (no family, no recipe: a
// graph's identity is that the node holds its files), the placement says why it left, and nothing ran here.
func TestRunGraphOverflowsWhenLocalHeld(t *testing.T) {
	n := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
	cfg := graphClient(t, n)
	lr := &laneRunner{verdict: heldLane()}
	req := graphReqOf(t, krea2Graph, map[string]any{"reserve_vram": "0.5"})
	outDir := req.Params["out_dir"].(string)

	res := Run(context.Background(), cfg, lr, req, "auto", nil)
	if !res.OK {
		t.Fatalf("the idle node that holds the file must take the graph: %+v", res)
	}
	if res.Meta.Node != "node-b" {
		t.Errorf("meta.node = %q", res.Meta.Node)
	}
	if !strings.HasPrefix(res.Meta.Placement, "remote: ") || !strings.Contains(res.Meta.Placement, "graph lane") || !strings.Contains(res.Meta.Placement, "bench render of the spring set") || !strings.Contains(res.Meta.Placement, "node-b") {
		t.Errorf("the placement must say where the graph ran and what held the local lane: %q", res.Meta.Placement)
	}
	probes, runs := lr.counts()
	if probes != 1 || runs != 0 {
		t.Errorf("the lane was asked once and nothing ran here: probes %d runs %d", probes, runs)
	}
	if len(lr.probed) != 1 || lr.probed[0].Task != core.TaskRunGraph {
		t.Errorf("the lane question is about the graph: %+v", lr.probed)
	}
	if ps := n.posts(); len(ps) != 1 || ps[0].path != "/fleet/dispatch" {
		t.Fatalf("one tokenless dispatch: %+v", ps)
	}
	p := graphPayload(t, n)
	if string(p["graph"]) != krea2Graph || string(p["reserve_vram"]) != `"0.5"` {
		t.Errorf("the graph travels inline as it is: %v", p)
	}
	for _, k := range []string{"family", "recipe_digest", "out_dir", "min_vram_gb", "devices"} {
		if _, has := p[k]; has {
			t.Errorf("%s must not travel with a graph: %v", k, p)
		}
	}
	m := decode(t, res)
	if b, err := os.ReadFile(filepath.Join(outDir, "graph-b.mp4")); err != nil || string(b) != "GB" {
		t.Errorf("the secondary output lands in out_dir, hash-checked: %q %v", b, err)
	}
	if _, unverified := m["unverified"]; unverified {
		t.Errorf("the node published the files' hashes, so the result is verified: %v", m)
	}
}

// A free lane runs here and the fleet is not even read.
func TestRunGraphWithAFreeLaneRunsLocalUnchanged(t *testing.T) {
	n := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
	cfg := graphClient(t, n)
	lr := &laneRunner{verdict: core.LaneVerdict{Free: true}}
	res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	if !res.OK || string(res.Data) != `{"local":true}` {
		t.Fatalf("the graph runs here: %+v", res)
	}
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Errorf("one question, one run: probes %d runs %d", probes, runs)
	}
	untouched(t, "the node", n)
}

// A8, as far as it can be run without a render. The first node of the roster does not hold the file the graph names: its
// own door refuses (503, naming the file) before any job exists, and the call goes on to the next node, which holds it.
func TestRunGraphOverflowMovesPastANodeThatLacksTheFile(t *testing.T) {
	lacks := startGraphNode(t, "node-a", []string{"models/vae/other.safetensors"}, 7.96)
	holds := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
	cfg := graphClient(t, lacks, holds)
	lr := &laneRunner{verdict: heldLane()}
	res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the node that holds the file must take it: %+v", res)
	}
	if ps := lacks.posts(); len(ps) != 1 {
		t.Errorf("the first node was asked once and refused at its door: %d POSTs", len(ps))
	}
	if ps := holds.posts(); len(ps) != 1 {
		t.Errorf("the second node was sent the graph: %d POSTs", len(ps))
	}
	if _, runs := lr.counts(); runs != 0 {
		t.Errorf("nothing ran here")
	}
	if len(lacks.runner.reqs) != 0 {
		t.Errorf("the refusing node ran nothing: %d requests", len(lacks.runner.reqs))
	}
}

// Nobody holds the file: the graph is a ticket that waits for the first lane that frees (or a node that comes to hold the file),
// and its answer names, per node, the files that node lacks, as the node's own door said them (not-capable for this ticket,
// not a full node).
func TestRunGraphNobodyHoldsTheFileWaitsAsATicketNamingIt(t *testing.T) {
	a := startGraphNode(t, "node-a", nil, 7.96)
	b := startGraphNode(t, "node-b", []string{"models/vae/other.safetensors"}, 15.92)
	cfg := graphClient(t, a, b)
	lr := newKeeperRunner(heldLane())
	req := graphReqOf(t, wanFilesGraph, nil)
	res := settle(t, cfg, lr, req, func(r core.Result) bool {
		return rowIs("node-a", "not-capable")(r) && rowIs("node-b", "not-capable")(r)
	})
	if probes, runs := lr.counts(); probes < 1 || runs != 0 {
		t.Fatalf("the graph waits as a ticket, asking its lane and running nothing here: probes %d runs %d (%+v)", probes, runs, res)
	}
	rows := answerOf(t, res).Cluster
	if len(rows) != 2 {
		t.Fatalf("one row per roster node: %+v", rows)
	}
	for _, r := range rows {
		for _, name := range []string{"wan/wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors", "umt5_xxl_fp8_e4m3fn_scaled.safetensors"} {
			if !strings.Contains(r.Why, name) {
				t.Errorf("the row names the files the node lacks (%s is missing from it): %+v", name, r)
			}
		}
	}
	if len(a.posts()) != 1 || len(b.posts()) != 1 {
		t.Errorf("each node was asked once, and a refusal is remembered by the ticket: %d / %d POSTs", len(a.posts()), len(b.posts()))
	}
}

// A whole-node graph runs in the default ComfyUI instance, which sees every card, so on a node with several cards it can
// land on the display card. It is placed only on a node with ONE card; a node that publishes no per-card list is not shown
// to have one, and is a named miss, never a pass.
func TestWholeNodeGraphNeverPlacedOnMultiGPUNode(t *testing.T) {
	multi := startGraphNode(t, "node-multi", []string{krea2File}, 15.92, 15.92, 15.92)
	blind := startGraphNode(t, "node-blind", []string{krea2File})
	cfg := graphClient(t, multi, blind)

	lr := newKeeperRunner(heldLane())
	res := runDoor(cfg, lr, graphReqOf(t, krea2Graph, nil), AtOnce)
	noPosts(t, "the three-card node", multi)
	noPosts(t, "the node with no card list", blind)
	if _, runs := lr.counts(); runs != 0 {
		t.Fatalf("with no single-card node the graph waits as a ticket: %+v", res)
	}
	rows := answerOf(t, res).Cluster
	if r, ok := rowFor(rows, "node-multi"); !ok || r.State != "not-capable" || !strings.Contains(r.Why, "3 cards") || !strings.Contains(r.Why, "display card") {
		t.Errorf("the row says why a three-card node is not used: %+v", r)
	}
	if r, ok := rowFor(rows, "node-blind"); !ok || r.State != "not-capable" || !strings.Contains(r.Why, "no per-card list") {
		t.Errorf("a node that does not say how many cards it has is not assumed to have one: %+v", r)
	}

	// The forced remote route is the same placement: a graph sent to a node by hand is still a whole-node graph.
	forced := Run(context.Background(), cfg, &laneRunner{}, graphReqOf(t, krea2Graph, nil), "remote", nil)
	if forced.OK {
		t.Fatalf("route remote must not place a whole-node graph on a multi-GPU node either: %+v", forced)
	}
	noPosts(t, "the three-card node", multi)
	if !strings.Contains(forced.Reason, "node-multi") || !strings.Contains(forced.Reason, "3 cards") {
		t.Errorf("the defer names the node and why: %s", forced.Reason)
	}

	// A node with one card takes it, on either route.
	one := startGraphNode(t, "node-one", []string{krea2File}, 7.96)
	cfg2 := graphClient(t, multi, one)
	if res := Run(context.Background(), cfg2, &laneRunner{verdict: heldLane()}, graphReqOf(t, krea2Graph, nil), "auto", nil); !res.OK || res.Meta.Node != "node-one" {
		t.Errorf("the single-card node takes the graph: %+v", res)
	}
	if res := Run(context.Background(), cfg2, &laneRunner{}, graphReqOf(t, krea2Graph, nil), "remote", nil); !res.OK || res.Meta.Node != "node-one" {
		t.Errorf("route remote picks the single-card node: %+v", res)
	}
}

// Wan 2.2 at 720p and above needs a 16 GB card: a call that names that family and carries such a height is kept off an
// 8 GB node even when it is first in the roster, a card the driver reports as 15.92 GiB is a 16 GB card, and the caller's own
// min_vram_gb, 0 included, wins over the default.
func TestMinVRAMKeepsWanOnA16GBCard(t *testing.T) {
	small := startGraphNode(t, "node-small", nil, 7.96)
	big := startGraphNode(t, "node-big", nil, 15.92)
	cfg := graphClient(t, small, big)
	wan := map[string]any{"model_family": "wan22"}

	res := Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, graphReqOf(t, graphWith(1280), wan), "auto", nil)
	if !res.OK || res.Meta.Node != "node-big" {
		t.Fatalf("Wan at 1280 high goes to the 16 GB card: %+v", res)
	}
	noPosts(t, "the 8 GB node", small)

	// The same on the forced route.
	resetPlacer()
	res = Run(context.Background(), cfg, &laneRunner{}, graphReqOf(t, graphWith(1280), wan), "remote", nil)
	if !res.OK || res.Meta.Node != "node-big" {
		t.Fatalf("route remote applies the same floor: %+v", res)
	}
	noPosts(t, "the 8 GB node", small)

	// Below 720 high the default does not apply, and the first node of the roster takes it.
	resetPlacer()
	res = Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, graphReqOf(t, graphWith(480), wan), "auto", nil)
	if !res.OK || res.Meta.Node != "node-small" {
		t.Fatalf("Wan at 480 high needs no floor: %+v", res)
	}

	// The caller's own number wins: 0 lifts the default, 24 shuts both nodes out and the answer says whose number it was.
	resetPlacer()
	res = Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, graphReqOf(t, graphWith(1280), map[string]any{"model_family": "wan22", "min_vram_gb": 0}), "auto", nil)
	if !res.OK || res.Meta.Node != "node-small" {
		t.Fatalf("min_vram_gb 0 lifts the default floor: %+v", res)
	}
	resetPlacer()
	lr := newKeeperRunner(heldLane())
	res = runDoor(cfg, lr, graphReqOf(t, graphWith(480), map[string]any{"min_vram_gb": "24"}), AtOnce)
	if _, runs := lr.counts(); runs != 0 {
		t.Fatalf("no node has 24 GB: the graph waits as a ticket: %+v", res)
	}
	for _, name := range []string{"node-small", "node-big"} {
		if r, ok := rowFor(answerOf(t, res).Cluster, name); !ok || r.State != "not-capable" || !strings.Contains(r.Why, "min_vram_gb 24") {
			t.Errorf("%s: the row names the caller's floor: %+v", name, r)
		}
	}

	// The default's row says it is a chosen number, not a measured one.
	resetPlacer()
	lr2 := newKeeperRunner(heldLane())
	only := graphClient(t, small)
	res = runDoor(only, lr2, graphReqOf(t, graphWith(1280), wan), AtOnce)
	if r, ok := rowFor(answerOf(t, res).Cluster, "node-small"); !ok || !strings.Contains(r.Why, "chosen") || !strings.Contains(r.Why, "wan22") || !strings.Contains(r.Why, "7.96") {
		t.Errorf("the default floor is named as chosen, with the card it found: %+v", r)
	}
}

// The floor reads the LARGEST card of the node, and a card counts as the size it rounds to.
func TestMinVRAMReadsTheLargestCardAndTheNominalSize(t *testing.T) {
	devices := func(totals ...float64) delegate.NodeView {
		var v delegate.NodeView
		for i, g := range totals {
			v.Devices = append(v.Devices, gpuprobe.Device{Index: i, TotalGiB: g})
		}
		return v
	}
	for _, c := range []struct {
		name   string
		floor  float64
		view   delegate.NodeView
		refuse bool
	}{
		{"a 16 GB card reports 15.92 GiB", 16, devices(15.92), false},
		{"a 16 GB card reports 15.93 GiB", 16, devices(15.93), false},
		{"a 12 GB card is not a 16 GB card", 16, devices(11.99), true},
		{"an 8 GB card is not a 16 GB card", 16, devices(7.96), true},
		{"the largest card counts, not the first", 16, devices(7.96, 15.92), false},
		{"nor the sum", 16, devices(7.96, 7.96), true},
		{"a 24 GB card", 24, devices(23.99), false},
		{"a floor of zero is no floor", 0, devices(7.96), false},
		{"no card list: unknown is never enough", 16, devices(), true},
	} {
		n := need{graph: &graphNeed{minVRAM: c.floor, minVRAMWhy: "min_vram_gb"}}
		if got := n.refuses(c.view) != ""; got != c.refuse {
			t.Errorf("%s: refused=%v (%q), want %v", c.name, got, n.refuses(c.view), c.refuse)
		}
	}
}

// What the default table keys on: the family the call names and the tallest height in its graph.
func TestTheDefaultFloorKeysOnTheFamilyAndTheGraphsHeight(t *testing.T) {
	floor := func(graph string, extra map[string]any) (float64, string) {
		req := graphReqOf(t, graph, extra)
		g := graphNeedOf(req)
		return g.minVRAM, g.minVRAMWhy
	}
	for _, c := range []struct {
		name  string
		graph string
		extra map[string]any
		want  float64
	}{
		{"wan22 at 720", graphWith(720), map[string]any{"model_family": "wan22"}, 16},
		{"wan22 at 1280", graphWith(1280), map[string]any{"model_family": "wan22"}, 16},
		{"wan22 at 719", graphWith(719), map[string]any{"model_family": "wan22"}, 0},
		{"wan2.2 spelled the other way", graphWith(1280), map[string]any{"model_family": "Wan2.2"}, 16},
		{"wan", graphWith(1280), map[string]any{"model_family": " wan "}, 16},
		{"another family", graphWith(1280), map[string]any{"model_family": "ltx25"}, 0},
		{"no family named: the harness does not guess what a graph is", graphWith(1280), nil, 0},
		{"no height in the graph", plainGraph, map[string]any{"model_family": "wan22"}, 0},
		{"the tallest height of any node", `{"1":{"inputs":{"height":480}},"2":{"inputs":{"height":1080.0}},"3":{"inputs":{"height":["1",0]}}}`, map[string]any{"model_family": "wan22"}, 16},
		{"the caller's number wins", graphWith(1280), map[string]any{"model_family": "wan22", "min_vram_gb": 24}, 24},
		{"the caller's zero lifts it", graphWith(1280), map[string]any{"model_family": "wan22", "min_vram_gb": 0}, 0},
		{"a number in a string (the CLI)", graphWith(480), map[string]any{"min_vram_gb": "12"}, 12},
		{"a JSON number", graphWith(480), map[string]any{"min_vram_gb": float64(12)}, 12},
		{"a blank numeral is no number given, so the default applies", graphWith(1280), map[string]any{"model_family": "wan22", "min_vram_gb": " "}, 16},
	} {
		got, why := floor(c.graph, c.extra)
		if got != c.want {
			t.Errorf("%s: floor %v, want %v", c.name, got, c.want)
		}
		if c.want == 16 && (!strings.Contains(why, "chosen") || strings.Contains(why, "measured:")) {
			t.Errorf("%s: the default is labelled as a chosen number: %q", c.name, why)
		}
	}
	g := graphNeedOf(graphReqOf(t, plainGraph, nil))
	if !g.wholeNode {
		t.Error("a graph with no devices is a whole-node graph")
	}
}

// min_vram_gb is a number of GB, 0 or more.
func TestMinVRAMMustBeANonNegativeNumber(t *testing.T) {
	for _, bad := range []any{-1, "abc", "-4", true} {
		_, err := plan(graphReqOf(t, plainGraph, map[string]any{"min_vram_gb": bad}))
		var ce *contractError
		if err == nil || !asContract(err, &ce) || !strings.Contains(err.Error(), "min_vram_gb") {
			t.Errorf("min_vram_gb %v must be a contract refusal naming it, got %v", bad, err)
		}
	}
	for _, ok := range []any{0, 16, "16", 15.5, "0", "", " "} {
		if _, err := plan(graphReqOf(t, plainGraph, map[string]any{"min_vram_gb": ok})); err != nil {
			t.Errorf("min_vram_gb %v is a number: %v", ok, err)
		}
	}
}

func asContract(err error, target **contractError) bool {
	ce, ok := err.(*contractError)
	if ok {
		*target = ce
	}
	return ok
}

// A graph with devices (a card named by the operator) and one run under an inherited lease are never offered to the fleet nor
// made a ticket: the lane is not even asked about them. (A graph that resumes a place in the local line USED to be on this list;
// as a ticket it brings its place along: TestAGraphThatArrivesWithAPlaceBringsItIntoTheTicket.)
func TestGraphTicketNeverStartsForWhatStaysHere(t *testing.T) {
	n := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
	cfg := graphClient(t, n)
	for name, extra := range map[string]map[string]any{
		"declared devices":    {"devices": []string{"0"}},
		"declared device any": {"devices": []any{"GPU-aaaa"}},
	} {
		lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
		res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, extra), "auto", nil)
		if probes, runs := lr.counts(); probes != 0 || runs != 1 {
			t.Errorf("%s: the lane is not asked and the call runs here: probes %d runs %d (%+v)", name, probes, runs, res)
		}
	}
	untouched(t, "the node", n)

	t.Setenv("GPU_LEASE_DIR", t.TempDir())
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 0 || runs != 1 {
		t.Errorf("under an inherited lease: probes %d runs %d", probes, runs)
	}
	untouched(t, "the node", n)
}

// The node must run this release (its file check ships with it), hold no lease, and serve run-graph with its route
// configured: each is a named miss, never a POST.
func TestGraphOverflowCandidatesNeedTheReleaseAnIdleNodeAndTheRoute(t *testing.T) {
	old := startNode(t, nodeOpts{id: "node-old", version: "0.1.0", healthEdit: withCards(7.96),
		cfg: func(c *config.Config) { c.ComfyDir = graphTreeWith(t, krea2File) }})
	busy := startNode(t, nodeOpts{id: "node-busy", version: fixtureVersion, lease: leased(), healthEdit: withCards(7.96),
		cfg: func(c *config.Config) { c.ComfyDir = graphTreeWith(t, krea2File) }})
	noRoute := startNode(t, nodeOpts{id: "node-noroute", version: fixtureVersion, healthEdit: withCards(7.96),
		cfg:    func(c *config.Config) { c.ComfyDir = graphTreeWith(t, krea2File); c.RunGraphScript = "" },
		routes: bindOnly})
	// A node that still lists run-graph but says its route is not CONFIGURED (a weight went missing under it).
	missing := startNode(t, nodeOpts{id: "node-missing", version: fixtureVersion,
		healthEdit: func(m map[string]any) {
			withCards(7.96)(m)
			m["media_routes"] = []map[string]any{{"route": "run_graph", "engine": "comfyui", "state": "BOUND-BUT-MISSING"}}
		},
		cfg: func(c *config.Config) { c.ComfyDir = graphTreeWith(t, krea2File) }})
	cfg := graphClient(t, old, busy, noRoute, missing)
	lr := newKeeperRunner(heldLane())
	res := runDoor(cfg, lr, graphReqOf(t, krea2Graph, nil), AtOnce)
	if _, runs := lr.counts(); runs != 0 {
		t.Fatalf("no node can take it, so the graph waits as a ticket: %+v", res)
	}
	for _, n := range []*node{old, busy, noRoute, missing} {
		noPosts(t, n.srv.URL, n)
	}
	rows := answerOf(t, res).Cluster
	if r, ok := rowFor(rows, "node-missing"); !ok || r.State != "not-capable" || !strings.Contains(r.Why, "BOUND-BUT-MISSING") {
		t.Errorf("a node whose run_graph route is not configured: %+v", r)
	}
	if r, ok := rowFor(rows, "node-old"); !ok || r.State != "not-capable" || !strings.Contains(r.Why, "harness 0.1.0") {
		t.Errorf("a node on another release: %+v", r)
	}
	if r, ok := rowFor(rows, "node-busy"); !ok || r.State != "busy" {
		t.Errorf("a node that holds a lease: %+v", r)
	}
	if r, ok := rowFor(rows, "node-noroute"); !ok || r.State != "not-capable" || !strings.Contains(r.Why, "run-graph") {
		t.Errorf("a node that does not serve run-graph: %+v", r)
	}
}

// A box with no delegate_remotes takes the path it took before graphs overflowed: the runner is called once with the
// request untouched, the lane is never asked about, no node is read, and nothing is attributed.
func TestNoFleetBoxIsByteIdenticalForRunGraph(t *testing.T) {
	useVersion(t)
	stubLocalRoutes(t, configured("run_graph"))
	var rosterReads atomic.Int32
	prev := probeRoster
	probeRoster = func(ctx context.Context, remotes []string, token string, timeout time.Duration) []rosterprobe.Reading {
		rosterReads.Add(1)
		return prev(ctx, remotes, token, timeout)
	}
	t.Cleanup(func() { probeRoster = prev })

	cfg := config.Config{MediaDir: t.TempDir()}
	rig := newPairRig(t, true)
	lr := &laneRunner{verdict: heldLane(), rig: rig, local: queuedLocal()}
	req := graphReqOf(t, krea2Graph, nil)
	want := queuedLocal()
	got := Run(context.Background(), cfg, lr, req, "auto", nil)
	if probes, runs := lr.counts(); probes != 0 || runs != 1 {
		t.Fatalf("a box with no fleet asks nothing and runs the call once: probes %d runs %d", probes, runs)
	}
	if string(got.Data) != string(want.Data) || got.Reason != want.Reason || !reflect.DeepEqual(got.Meta, want.Meta) || got.DeferClass != want.DeferClass || got.OK != want.OK {
		t.Errorf("the result must be the runner's own, untouched:\n got %+v\nwant %+v", got, want)
	}
	if strings.Contains(string(got.Data), "cluster") {
		t.Errorf("no fleet, no cluster block: %s", got.Data)
	}
	if rosterReads.Load() != 0 || rig.frameCount() != 0 || len(rig.rows()) != 0 {
		t.Errorf("nothing was read or attributed: roster reads %d, frames %d, rows %d", rosterReads.Load(), rig.frameCount(), len(rig.rows()))
	}
	// route local on a box WITH a fleet is the same.
	n := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
	cfg.DelegateRemotes = []string{n.srv.URL}
	lr2 := &laneRunner{verdict: heldLane()}
	if res := Run(context.Background(), cfg, lr2, req, "local", nil); !res.OK {
		t.Fatal(res)
	}
	if probes, runs := lr2.counts(); probes != 0 || runs != 1 {
		t.Errorf("route local asks nothing: probes %d runs %d", probes, runs)
	}
	untouched(t, "the node", n)
}

// The release proof is not only an overflow rule. A node's file check ships with the release, so a node on another one is
// not shown to make it, and that holds on every remote placement of a graph: route remote and a thin client's auto call
// reach pickNodeFor, not the overflow's matcher, and read the same version. A node that publishes no version is on no release
// this machine can name.
func TestAGraphOnAnyRemoteRouteNeedsANodeOnThisRelease(t *testing.T) {
	older := startNode(t, nodeOpts{id: "node-old", version: "0.1.0", healthEdit: withCards(7.96),
		cfg: func(c *config.Config) { c.ComfyDir = graphTreeWith(t, krea2File) }})
	unversioned := startNode(t, nodeOpts{id: "node-none",
		healthEdit: func(m map[string]any) { withCards(7.96)(m); delete(m, "harness_version") },
		cfg:        func(c *config.Config) { c.ComfyDir = graphTreeWith(t, krea2File) }})

	for _, route := range []string{"remote", "auto-thin-client"} {
		cfg := graphClient(t, older, unversioned)
		mode := route
		if route == "auto-thin-client" {
			stubLocalRoutes(t, configured()) // this machine has no run_graph route: no queue to wait in, so the call leaves
			mode = "auto"
		}
		res := Run(context.Background(), cfg, &laneRunner{}, graphReqOf(t, krea2Graph, nil), mode, nil)
		if res.OK || !res.Deferred {
			t.Fatalf("%s: a graph is not sent to a node on another release: %s", route, brief(res))
		}
		for _, want := range []string{"node-old", "harness 0.1.0", "node-none", "(none published)", localVersion} {
			if !strings.Contains(res.Reason, want) {
				t.Errorf("%s: the refusal names every miss and the release it wanted (%q is missing): %s", route, want, res.Reason)
			}
		}
		noPosts(t, "the node on another release", older)
		noPosts(t, "the node that publishes no release", unversioned)
	}

	// A node on this release takes it on the same routes (the control: the check refuses the version, not the route).
	same := startGraphNode(t, "node-same", []string{krea2File}, 7.96)
	cfg := graphClient(t, older, same)
	if res := Run(context.Background(), cfg, &laneRunner{}, graphReqOf(t, krea2Graph, nil), "remote", nil); !res.OK || res.Meta.Node != "node-same" {
		t.Errorf("route remote skips the node on another release and takes the one on this release: %s", brief(res))
	}
}
