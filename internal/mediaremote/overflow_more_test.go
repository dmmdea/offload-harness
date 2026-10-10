package mediaremote

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The family travels under the NODE'S name for the recipe, never the caller's, and the digest of THIS machine's recipe
// travels with it: a name binds different files on different nodes, and the digest is what the node checks.
func TestOverflowRewritesFamilyToTheNodesName(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)
	lr := &laneRunner{verdict: heldLane()}
	if res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil); !res.OK {
		t.Fatal(res)
	}
	_, payload := posted(t, n)
	if payload["family"] != "qwen-image-2.1" {
		t.Errorf("the node is asked for ITS family, not the caller's %q: %v", "qwen-image-2.1-fast", payload["family"])
	}
	if payload["recipe_digest"] != mustRecipe(t, cfg, "qwen-image-2.1-fast").Digest() {
		t.Errorf("the digest of this machine's recipe travels: %v", payload["recipe_digest"])
	}
	if got := n.runner.last().Params["family"]; got != "qwen-image-2.1" {
		t.Errorf("the node's pipeline resolved family %v", got)
	}
}

// Only an image job overflows in this release. A graph or a clip whose local lane is busy is not asked about, not
// placed, and the fleet is not read: their identity (the files a graph loads, a video family's recipe) is not defined yet.
func TestOnlyImageJobsOverflow(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", nil, nodeOpts{})
	cfg := overflowClient(t, "krea2", nil, n)
	stubLocalRoutes(t, configured("generate_image", "run_graph", "generate_video", "animate_character", "generate_audio:voice"))
	graph := writeFile(t, t.TempDir(), "g.json", []byte(`{"1":{}}`))
	for name, req := range map[string]core.Request{
		"a graph":     {Task: core.TaskRunGraph, Params: map[string]any{"graph_path": graph}},
		"a clip":      video(nil),
		"a character": {Task: core.TaskAnimateCharacter, Input: "p", Params: map[string]any{"ref": graph, "driver": graph}},
		"a voice":     {Task: core.TaskGenerateAudio, Input: "hello", Params: map[string]any{"voice": "generalist"}},
	} {
		lr := &laneRunner{verdict: heldLane()}
		if res := Run(context.Background(), cfg, lr, req, "auto", nil); !res.OK {
			t.Fatalf("%s: %+v", name, res)
		}
		if probes, runs := lr.counts(); probes != 0 || runs != 1 {
			t.Errorf("%s: runs here without a question: probes %d runs %d", name, probes, runs)
		}
	}
	untouched(t, "the node", n)
}

// Two calls of one process that both read a node idle must not both send it a job: the second would wait out the
// node's whole window in the node's queue and come back as a bounce. The node a call has a job on is held for it, and
// the second call goes on to its own queue and says why.
func TestConcurrentCallsDoNotStackOnOneNode(t *testing.T) {
	hold := make(chan struct{})
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{hold: hold})
	t.Cleanup(func() { close(hold) })
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)

	first := make(chan core.Result, 1)
	go func() {
		first <- Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(n.posts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(n.posts()) != 1 {
		t.Fatal("the first call never reached the node")
	}

	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	// A bound on the second call, so that a node that WAS sent a second job (the node holds both until it is released
	// below) ends this test in seconds instead of hanging it.
	second, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	res := Run(second, cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("the second call waits in its own queue: probes %d runs %d (%+v)", probes, runs, res)
	}
	if len(n.posts()) != 1 {
		t.Errorf("the node was sent a second job while it had one: %d POSTs", len(n.posts()))
	}
	row, ok := rowFor(clusterOf(t, res), "node-b")
	if !ok || row.State != "busy" || !strings.Contains(row.Why, "another call of this machine") {
		t.Errorf("the answer says why: %+v", row)
	}

	// Once the first call is done the node is free for the next one.
	hold <- struct{}{}
	select {
	case r := <-first:
		if !r.OK {
			t.Fatalf("the first call: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first call never ended")
	}
	if !placer.hold(n.srv.URL) {
		t.Error("the node must be free for the next call once the first is done")
	}
	placer.release(n.srv.URL)
}

// The only node bounces the call (it accepted the job, another job holds its card) and nothing else admits, so the call
// runs here. The bounced attempt left exactly one card, closed quiet (it ran nothing), and NO asker ledger row: the
// pipeline's own run writes the call's row, and a remote attempt that held nothing must not write a second one.
func TestBounceThenNothingAdmitsLeavesOneQuietCardAndNoAskerRow(t *testing.T) {
	a := startImageNode(t, "node-a", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")},
		nodeOpts{deferAs: "gpu busy: card(s) held by a media-class lease", deferClass: core.ErrClassGPUBusy})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a)
	cfg.PairWorkloadsEnabled = true
	rig := newPairRig(t, true, "node-a")
	lr := &laneRunner{verdict: heldLane(), rig: rig, local: queuedLocal()}

	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("nothing else admits, so the call runs here once: probes %d runs %d", probes, runs)
	}
	row, ok := rowFor(clusterOf(t, res), "node-a")
	if !ok || row.State != "bounced" || !strings.Contains(row.Why, "passed it back") {
		t.Fatalf("the answer says the node had the call and passed it back: %+v", row)
	}
	cards := rig.cards() // fails unless every frame belongs to ONE card
	if cards["queued"] == nil || cards["completed"] == nil || cards["failed"] != nil || cards["completed"]["startedAt"] != nil {
		t.Errorf("the bounced node's card closes quiet, never started: %v", cards)
	}
	if rows := rig.rows(); len(rows) != 0 {
		t.Errorf("a remote attempt that held nothing writes no asker row: %+v", rows)
	}
	if got := len(a.posts()); got != 1 {
		t.Errorf("the node was sent the call once: %d", got)
	}
}

// A roster that lists the machine ITSELF (a loopback or tailnet address of its own fleet-serve) must not make the busy
// lane its own overflow: that entry matches the recipe perfectly, ranks first when it holds no lease the node can see
// (a place in line, a host-RAM hold), and would send the call back into the same lane to wait its window and bounce.
// A node's id is fleet_node_id, else the OS hostname, on both sides; the entry is named in cluster[] and never tried.
func TestTheMachineItselfIsNeverACandidate(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	self := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	other := startImageNode(t, "node-c", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, self, other)
	cfg.FleetNodeID = "Node-B" // the case differs on purpose: hostnames are case-insensitive

	lr := &laneRunner{verdict: heldLane()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-c" {
		t.Fatalf("the other node serves it: %+v", res)
	}
	noPosts(t, "this machine's own node", self)

	// Alone, the call stays in its own queue and the answer names the entry.
	only := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, self)
	only.FleetNodeID = "node-b"
	lr2 := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res = Run(context.Background(), only, lr2, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if probes, runs := lr2.counts(); probes != 1 || runs != 1 {
		t.Fatalf("nothing else admits: probes %d runs %d", probes, runs)
	}
	row, ok := rowFor(clusterOf(t, res), "node-b")
	if !ok || row.State != "skipped" || !strings.Contains(row.Why, "this machine") {
		t.Fatalf("the entry is named, not tried: %+v", row)
	}
	noPosts(t, "this machine's own node", self)

	// With no fleet_node_id the OS hostname is the id, exactly as fleet-serve derives its own.
	byName := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, self)
	prev := hostnameFn // after overflowClient, which pins a hostname of its own
	hostnameFn = func() (string, error) { return "node-b", nil }
	t.Cleanup(func() { hostnameFn = prev })
	lr3 := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res = Run(context.Background(), byName, lr3, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if row, ok := rowFor(clusterOf(t, res), "node-b"); !ok || row.State != "skipped" {
		t.Fatalf("the hostname is the id when fleet_node_id is unset: %+v", row)
	}
}
