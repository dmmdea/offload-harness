package mediaremote

// A busy local image lane overflows to a node of the fleet that renders the same recipe (ADR 0082). Everything below
// runs against real fleet nodes (their doors, job store and recipe advertisement) and a delegator whose pipeline is
// a runner that answers the lane question and counts what it ran. The contract being tested, in the order the tests
// take it:
//
//	byte-identical on a box with no fleet, and a free lane runs here untouched;
//	a busy lane goes to the node whose recipe is strictly equal, under the node's own family name, with the recipe
//	  digest and the caller's refine and seed exactly as sent;
//	what cannot be placed is named, per node, and the call then runs here as it always did;
//	a refusal before acceptance moves on, a node's own answer after it never does, and a call the node holds is
//	  never run twice.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// A box with no delegate_remotes takes the path it took before the lane probe existed: the runner is called once
// with the request untouched, the lane is never asked about, no node is read, and nothing is attributed.
func TestNoFleetBoxIsByteIdentical(t *testing.T) {
	useVersion(t)
	stubLocalRoutes(t, configured("generate_image"))
	var rosterReads atomic.Int32
	prev := probeRoster
	probeRoster = func(ctx context.Context, remotes []string, token string, timeout time.Duration) []rosterprobe.Reading {
		rosterReads.Add(1)
		return prev(ctx, remotes, token, timeout)
	}
	t.Cleanup(func() { probeRoster = prev })

	cfg := config.Config{MediaDir: t.TempDir()}
	binding(comfyTree(t), "krea2", nil)(&cfg)
	rig := newPairRig(t, true)
	lr := &laneRunner{verdict: heldLane(), rig: rig, local: queuedLocal()}
	req := overflowReq(t, "", nil)
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
	n := startImageNode(t, "node-b", "krea2", nil, nodeOpts{})
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

// A free lane runs here and the fleet is not even read: the call is what it was before the lane probe.
func TestFreeLaneRunsLocalUnchanged(t *testing.T) {
	n := startImageNode(t, "node-b", "krea2", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)
	lr := &laneRunner{verdict: core.LaneVerdict{Free: true}}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if !res.OK || string(res.Data) != `{"local":true}` {
		t.Fatalf("the call runs here: %+v", res)
	}
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Errorf("one question, one run: probes %d runs %d", probes, runs)
	}
	untouched(t, "the node", n)
}

// THE F52 SHAPE. The local lane is held; a node binds the same int8 build under another NAME; the call goes there and
// comes back verified. The answer says where it ran and why it left, the job ran under the node's own family name, the
// recipe digest travelled, the card is that node's, and nothing ran here.
func TestHeldLaneOverflowsToMatchingNode(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, `"comfy_cuda_device":"2","imagegen_timeout_sec":900`)}, n)
	lr := &laneRunner{verdict: heldLane()}
	req := overflowReq(t, "qwen-image-2.1-fast", nil)
	out := req.Params["out"].(string)

	res := Run(context.Background(), cfg, lr, req, "auto", nil)
	if !res.OK {
		t.Fatalf("the idle matching node must take the call: %+v", res)
	}
	if res.Meta.Node != "node-b" {
		t.Errorf("meta.node = %q", res.Meta.Node)
	}
	if !strings.HasPrefix(res.Meta.Placement, "remote: ") || !strings.Contains(res.Meta.Placement, "bench render of the spring set") || !strings.Contains(res.Meta.Placement, "node-b") {
		t.Errorf("the placement must say where the call ran and what held the local lane: %q", res.Meta.Placement)
	}
	m := decode(t, res)
	if m["image_path"] != out || m["node"] != "node-b" || m["remote_job_id"] == "" {
		t.Errorf("result = %v", m)
	}
	if _, unverified := m["unverified"]; unverified {
		t.Errorf("the node published the file's hash, so the result is verified: %v", m)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "IMG" {
		t.Errorf("the render must land at the caller's out, hash-checked: %q %v", b, err)
	}
	if probes, runs := lr.counts(); probes != 1 || runs != 0 {
		t.Errorf("the lane was asked once and nothing ran here: probes %d runs %d", probes, runs)
	}
	if len(n.posts()) != 1 {
		t.Fatalf("one dispatch: %d", len(n.posts()))
	}
	jobID, payload := posted(t, n)
	if n.posts()[0].path != "/fleet/dispatch" {
		t.Errorf("an image job with no input file uses the tokenless dispatch: %s", n.posts()[0].path)
	}
	local := mustRecipe(t, cfg, "qwen-image-2.1-fast")
	if payload["recipe_digest"] != local.Digest() || payload["family"] != "qwen-image-2.1" || payload["seed"] != float64(424242) {
		t.Errorf("payload = %v (want the node's own family name, this machine's recipe digest %s and the seed)", payload, local.Digest())
	}
	if _, has := payload["out"]; has {
		t.Errorf("out never travels: %v", payload)
	}
	if got := n.runner.last().Params; got["family"] != "qwen-image-2.1" || got["seed"] != 424242 {
		t.Errorf("the node's pipeline ran %v", got)
	}
	if !strings.HasPrefix(jobID, "media-") {
		t.Errorf("job id %q", jobID)
	}
}

func mustRecipe(t *testing.T, cfg config.Config, family string) mediacap.Recipe {
	t.Helper()
	l, ok, why := localIdentity(cfg, family)
	if !ok {
		t.Fatalf("no recipe for %q: %s", family, why)
	}
	return l.recipe
}

// The call names no family: it is the delegator's DEFAULT binding that was asked for, and the node must render that
// recipe, never its own default. A node whose default is another model is not a candidate; the one that binds the
// delegator's default under a name of its own is, and the family travels by that name and never empty.
func TestEmptyFamilyNeverLandsOnADifferentDefault(t *testing.T) {
	other := startImageNode(t, "node-c", "hidream-o1", nil, nodeOpts{})
	match := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "qwen-image-2.1-int8", nil, other, match)
	lr := &laneRunner{verdict: heldLane()}

	res := Run(context.Background(), cfg, lr, overflowReq(t, "", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the node that binds this machine's default recipe takes it: %+v", res)
	}
	noPosts(t, "the node whose default is another model", other)
	_, payload := posted(t, match)
	if payload["family"] != "qwen-image-2.1-fast" || payload["recipe_digest"] == "" {
		t.Errorf("the family must travel by the node's own name, never empty: %v", payload)
	}
	if got := match.runner.last().Params["family"]; got != "qwen-image-2.1-fast" {
		t.Errorf("the node ran family %v, not its default", got)
	}
}

// A node that binds the same family NAME to another build is not a candidate, and the answer says which key differs and
// which family of this machine WOULD match it, so the caller can choose that one knowingly.
func TestStrictMismatchNamesTheDifferingKey(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{
		"qwen-image-2.1": block(bf16Ckpt, ""), "qwen-image-2.1-fast": block(int8Ckpt, ""),
	}, n)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}

	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1", nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("nothing matches: the call runs here once: probes %d runs %d", probes, runs)
	}
	noPosts(t, "the node with the int8 build", n)
	rows := clusterOf(t, res)
	row, ok := rowFor(rows, "node-b")
	if !ok || row.State != "not-capable" {
		t.Fatalf("cluster = %+v", rows)
	}
	for _, want := range []string{"imagegen_ckpt: " + bf16Ckpt + " vs " + int8Ckpt, "send family=qwen-image-2.1-fast to use its qwen-image-2.1"} {
		if !strings.Contains(row.Why, want) {
			t.Errorf("why must contain %q: %s", want, row.Why)
		}
	}
	if len(row.Differs) != 1 || !strings.HasPrefix(row.Differs[0], "imagegen_ckpt: ") {
		t.Errorf("differs = %v", row.Differs)
	}
	if !strings.Contains(res.Reason, row.Why) || !strings.Contains(string(res.Data), "tk-abc12345") {
		t.Errorf("the local answer keeps its place in line and quotes the cluster: %s | %s", res.Reason, res.Data)
	}
}

// refine=false travels, and a node that does not carry it is a named miss (never one that refines anyway). A call that
// does not ask for it is not held to it.
func TestRefineFalseCarriedOrNodeNamedMiss(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	newNode := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	oldNode := startImageNode(t, "node-c", "hidream-o1", fams, nodeOpts{healthEdit: without("refine_honoured")})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, oldNode, newNode)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}

	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", map[string]any{"refine": false}), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the node that carries refine takes it: %+v", res)
	}
	noPosts(t, "the node that predates refine_honoured", oldNode)
	_, payload := posted(t, newNode)
	if payload["refine"] != false {
		t.Errorf("refine=false must travel: %v", payload)
	}
	if got := newNode.runner.last().Params["refine"]; got != false {
		t.Errorf("the node's pipeline got refine %v", got)
	}

	// With only the old node, the call is not placed and the answer names it.
	alone := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, oldNode)
	lr2 := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res = Run(context.Background(), alone, lr2, overflowReq(t, "qwen-image-2.1-fast", map[string]any{"refine": false}), "auto", nil)
	row, ok := rowFor(clusterOf(t, res), "node-c")
	if !ok || row.State != "not-capable" || !strings.Contains(row.Why, "refine=false") {
		t.Fatalf("the old node must be a named miss: %+v | %s", row, res.Data)
	}
	// Without refine=false the same old node is a fine target.
	lr3 := &laneRunner{verdict: heldLane()}
	if res := Run(context.Background(), alone, lr3, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil); !res.OK || res.Meta.Node != "node-c" {
		t.Errorf("a call that does not ask for refine=false is not held to it: %+v", res)
	}
	// The same rule on the path that always went to a node: refine=false to a node that cannot carry it is a miss, not a
	// contract refusal (and not a silent refinement).
	forced := clientCfg(t, oldNode)
	r := Run(context.Background(), forced, &recordingRunner{}, core.Request{Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"refine": false}}, "remote", nil)
	if r.OK || r.DeferClass != core.DeferClassCapacity || !strings.Contains(r.Reason, "refine=false") {
		t.Errorf("route remote must name the node that cannot carry refine=false: %+v", r)
	}
	forcedNew := clientCfg(t, newNode)
	if r := Run(context.Background(), forcedNew, &recordingRunner{}, core.Request{Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"refine": false}}, "remote", nil); !r.OK {
		t.Errorf("route remote to a node that carries it: %+v", r)
	}
}

// A per-request steps on a graph that takes steps and cfg together needs the node's binding to have SET cfg: a node that
// left it to the builder would fail the job (the runner's usage error). It is a named miss, not a failed render.
func TestStepsOverrideNeedsCFGSetOnTheNode(t *testing.T) {
	noCFG := strings.Replace(block(int8Ckpt, ""), `"imagegen_steps":40,"imagegen_cfg":1,`, "", 1)
	left := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": noCFG}, nodeOpts{})
	set := startImageNode(t, "node-c", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	// This machine sets steps and cfg too; the node that leaves both to the builder resolves to the same recipe.
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, left, set)
	lr := &laneRunner{verdict: heldLane()}

	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", map[string]any{"steps": 30}), "auto", nil)
	if !res.OK || res.Meta.Node != "node-c" {
		t.Fatalf("the node that set cfg takes a call with steps: %+v", res)
	}
	noPosts(t, "the node that left cfg to the builder", left)

	// Without a per-request steps the two spellings of the same recipe are equal: the first node in config order wins.
	lr2 := &laneRunner{verdict: heldLane()}
	if res := Run(context.Background(), cfg, lr2, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil); !res.OK || res.Meta.Node != "node-b" {
		t.Errorf("an unset steps/cfg is the builder's 40 / 1, the same recipe: %+v", res)
	}
}

// A recipe is compared between one release and itself: the graph builders ship with the version.
func TestHarnessVersionMustMatch(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	old := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{healthEdit: func(m map[string]any) { m["harness_version"] = "0.178.0" }})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, old)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	row, ok := rowFor(clusterOf(t, res), "node-b")
	if !ok || row.State != "not-capable" || !strings.Contains(row.Why, "0.178.0") || !strings.Contains(row.Why, fixtureVersion) {
		t.Fatalf("a node on another release is a named miss: %+v", row)
	}
	noPosts(t, "the node on another release", old)
}

// A node that holds a lease of any class is busy; the answer says by what and for how long.
func TestAnIdleNodeIsTheOnlyCandidate(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	busy := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{lease: leased()})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, busy)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("no node can take it: it runs here: probes %d runs %d", probes, runs)
	}
	a, ok := rowFor(clusterOf(t, res), "node-a")
	if !ok || a.State != "busy" || !strings.Contains(a.Why, "a bench render") || !strings.Contains(a.Why, "media-class lease") || !strings.Contains(a.Why, "left") {
		t.Errorf("node-a: %+v", a)
	}
	noPosts(t, "the busy node", busy)
}

// A node that reports the family's route as not configured is not capable, whatever its recipe says.
func TestTheFamilysRouteMustBeConfiguredWhenTheNodeReportsIt(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	missing := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{routes: func(config.Config) []mediacap.Route {
		return []mediacap.Route{{Name: "generate_image:qwen-image-2.1", Engine: "comfyui", State: mediacap.BoundButMissing}}
	}})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, missing)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	b, ok := rowFor(clusterOf(t, res), "node-b")
	if !ok || b.State != "not-capable" || !strings.Contains(b.Why, "generate_image:qwen-image-2.1") || !strings.Contains(b.Why, "BOUND-BUT-MISSING") {
		t.Errorf("node-b: %+v", b)
	}
	noPosts(t, "the node whose route is missing", missing)
}

// Among idle matching nodes the one with the shorter queue goes first, then config order.
func TestCandidatesRankByQueueThenConfigOrder(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	queued := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{healthEdit: func(m map[string]any) { m["queue_depth"] = 3; m["jobs_running"] = 1; m["jobs_queued"] = 2 }})
	first := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	second := startImageNode(t, "node-c", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, queued, first, second)
	lr := &laneRunner{verdict: heldLane()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the shorter queue first, then config order: %+v", res)
	}
	noPosts(t, "the node with a queue", queued)
	noPosts(t, "the later node", second)
}

// A bounce: the first node answers, after it accepted the job, that another job holds its card (gpu_busy). Nothing ran
// there, so the call goes on to the next node under a FRESH job id. Each node that held the job had a card, and the
// first closes quiet (never started); the call has ONE ledger row, naming the node that served it.
func TestBounceOnGPUBusyUsesFreshJobID(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	a := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{deferAs: "gpu busy: card(s) held by a media-class lease", deferClass: core.ErrClassGPUBusy})
	b := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
	rig := newPairRig(t, true, "node-a", "node-b")
	cfg.PairWorkloadsEnabled = true
	lr := &laneRunner{verdict: heldLane(), rig: rig}

	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the second node serves the bounced call: %+v", res)
	}
	if _, runs := lr.counts(); runs != 0 {
		t.Errorf("a call that a node took is never also run here: %d", runs)
	}
	ida, idb := jobIDs(t, a), jobIDs(t, b)
	if len(ida) != 1 || len(idb) != 1 || ida[0] == idb[0] {
		t.Fatalf("each node is sent the call once, under a fresh job id: %v / %v", ida, idb)
	}
	byID := map[any]map[string]map[string]any{}
	rig.e.Wait()
	rig.mu.Lock()
	for _, f := range rig.frames {
		wi := f["params"].(map[string]any)["workloadInfo"].(map[string]any)
		if byID[wi["id"]] == nil {
			byID[wi["id"]] = map[string]map[string]any{}
		}
		byID[wi["id"]][wi["state"].(string)] = wi
	}
	rig.mu.Unlock()
	cardA, cardB := byID[ida[0]], byID[idb[0]]
	if cardA["queued"] == nil || cardA["completed"] == nil || cardA["failed"] != nil || cardA["completed"]["startedAt"] != nil {
		t.Errorf("the bounced node's card closes quiet, never started: %v", cardA)
	}
	if cardB["queued"] == nil || cardB["completed"] == nil || cardB["completed"]["startedAt"] == nil {
		t.Errorf("the serving node's card runs to completion: %v", cardB)
	}
	if cardA["queued"]["scheduledOn"] == cardB["queued"]["scheduledOn"] {
		t.Errorf("the two cards are on two nodes: %v / %v", cardA["queued"]["scheduledOn"], cardB["queued"]["scheduledOn"])
	}
	rows := rig.rows()
	if len(rows) != 1 || rows[0].NodeID != "node-b" || rows[0].FleetJobID != idb[0] || rows[0].Deferred || rows[0].Route != "auto" {
		t.Errorf("one ledger row, for the node that served the call: %+v", rows)
	}
}

// A node that answers 503 to the POST never took the job: no card was opened for it, and the call goes on. The card is
// the accepting node's alone.
func TestOneCardOnTheAcceptingNode(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	full := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{})
	full.refusePost = 503
	ok := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, full, ok)
	rig := newPairRig(t, true, "node-a", "node-b")
	cfg.PairWorkloadsEnabled = true
	lr := &laneRunner{verdict: heldLane(), rig: rig}

	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("%+v", res)
	}
	if len(full.posts()) != 1 {
		t.Errorf("the full node was tried once: %d", len(full.posts()))
	}
	cards := rig.cards() // fails unless every frame belongs to ONE card
	if cards["queued"]["scheduledOn"] != "node-b-uuid" || cards["completed"] == nil || rig.frameCount() < 2 {
		t.Errorf("the one card is the accepting node's: %v", cards)
	}
	if rows := rig.rows(); len(rows) != 1 || rows[0].NodeID != "node-b" {
		t.Errorf("rows = %+v", rows)
	}
}

// What a node itself answered after it accepted the job is an observed answer: a render that failed is the call's result
// and is never placed anywhere else, here or on another node.
func TestObservedNodeErrorIsNeverReplaced(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	a := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{deferAs: "image generation failed: ComfyUI exited with status 1"})
	b := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
	lr := &laneRunner{verdict: heldLane()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if res.OK || !res.Deferred || !strings.Contains(res.Reason, "ComfyUI exited") || res.Meta.Node != "node-a" {
		t.Fatalf("the node's own deferral is the call's result: %+v", res)
	}
	noPosts(t, "the second node", b)
	if _, runs := lr.counts(); runs != 0 {
		t.Errorf("a failed render on a node is not re-run here: %d", runs)
	}
}

// A node that has the job and then goes away cannot be recalled (a media job cannot be withdrawn), so the call is NOT
// run anywhere else: it fails, naming the node and the remote job.
func TestFailureAfterAcceptanceNeverFallsBackToLocal(t *testing.T) {
	hold := make(chan struct{})
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	a := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{hold: hold})
	t.Cleanup(func() { close(hold) }) // after the node starts, so it runs BEFORE the node's drain
	b := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
	lr := &laneRunner{verdict: heldLane()}
	done := make(chan core.Result, 1)
	go func() {
		done <- Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	}()
	// Until the client has polled the job at least once, the node has certainly accepted it.
	deadline := time.Now().Add(10 * time.Second)
	for polls(a) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	a.srv.CloseClientConnections()
	a.srv.Close()
	var res core.Result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the call never ended")
	}
	if res.OK || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "node-a") || !strings.Contains(res.Reason, "media-") {
		t.Fatalf("the call fails naming the node and the remote job: %+v", res)
	}
	if _, runs := lr.counts(); runs != 0 {
		t.Errorf("a job a node holds is never also run here: %d", runs)
	}
	noPosts(t, "the other node", b)
}

// A call that runs under a lease its process inherited has a card chosen for it: it is never asked about, never placed.
func TestInheritedLeaseNeverOverflows(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)
	t.Setenv("GPU_LEASE_DIR", t.TempDir())
	t.Setenv("GPU_LEASE_EPOCH", "7")
	t.Setenv("GPU_LEASE_CLASS", "media")
	lr := &laneRunner{verdict: heldLane()}
	if res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil); !res.OK {
		t.Fatal(res)
	}
	if probes, runs := lr.counts(); probes != 0 || runs != 1 {
		t.Errorf("under an inherited lease the call runs here without a question: probes %d runs %d", probes, runs)
	}
	untouched(t, "the node", n)
}

// Only an image job overflows in this release (graphs arrive with their own file check), and a graph that names cards
// never leaves: a card id names a card on this machine.
func TestDeclaredDevicesNeverOverflow(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", nil, nodeOpts{})
	cfg := overflowClient(t, "krea2", nil, n)
	stubLocalRoutes(t, configured("generate_image", "run_graph"))
	graph := writeFile(t, t.TempDir(), "g.json", []byte(`{"1":{}}`))
	lr := &laneRunner{verdict: heldLane()}
	req := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": graph, "devices": []string{"0"}}}
	if res := Run(context.Background(), cfg, lr, req, "auto", nil); !res.OK {
		t.Fatal(res)
	}
	if probes, runs := lr.counts(); probes != 0 || runs != 1 {
		t.Errorf("a graph with declared devices runs here: probes %d runs %d", probes, runs)
	}
	untouched(t, "the node", n)
}

// A call that resumes a place in the local line (it carries its waiter_token) keeps that place: it is not placed
// elsewhere, and the place is not spent. (Late binding across the fleet is the ticket queue's, not this release's.)
func TestAResumedCallStaysInTheLocalLine(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)
	lr := &laneRunner{verdict: heldLane()}
	req := overflowReq(t, "qwen-image-2.1-fast", map[string]any{"waiter_token": "tk-abc12345"})
	if res := Run(context.Background(), cfg, lr, req, "auto", nil); !res.OK {
		t.Fatal(res)
	}
	if probes, runs := lr.counts(); probes != 0 || runs != 1 {
		t.Errorf("a resumed call runs where its place is: probes %d runs %d", probes, runs)
	}
	untouched(t, "the node", n)
}

// A `remotes` argument that is not among delegate_remotes is the contract defer of the paths that use it; a call that
// stays on a free or busy local lane is not made to fail by it.
func TestABadRemotesArgumentDoesNotBreakALocalCall(t *testing.T) {
	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	stranger := startImageNode(t, "node-z", "hidream-o1", nil, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)
	lr := &laneRunner{verdict: heldLane()}
	if res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", []string{stranger.srv.URL}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if probes, runs := lr.counts(); probes != 0 || runs != 1 {
		t.Errorf("probes %d runs %d", probes, runs)
	}
	untouched(t, "the stranger", stranger)
	untouched(t, "the configured node", n)
}

// Nothing admits: the call falls through to today's local run and its deferral carries a cluster[] block with one row
// per roster entry, in config order, each with the state and the reason in the node's own terms.
func TestNothingAdmitsFallsBackWithClusterBlock(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	busy := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{lease: leased()})
	gone := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	gone.srv.Close()
	other := startImageNode(t, "node-c", "hidream-o1", map[string]string{"qwen-image-2.1": block(fp4Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, busy, gone, other)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}

	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("the local run is today's: probes %d runs %d", probes, runs)
	}
	if res.OK || res.Meta.ErrClass != core.ErrClassGPUQueued || !strings.Contains(string(res.Data), `"waiter_token":"tk-abc12345"`) {
		t.Fatalf("the local deferral and its place in line are kept: %+v", res)
	}
	rows := clusterOf(t, res)
	if len(rows) != 3 {
		t.Fatalf("one row per roster entry: %+v", rows)
	}
	want := []struct{ state, node, why string }{
		{"busy", "node-a", "a bench render"},
		{"unreachable", "127.0.0.1", ""},
		{"not-capable", "node-c", "imagegen_ckpt: " + int8Ckpt + " vs " + fp4Ckpt},
	}
	for i, w := range want {
		if rows[i].State != w.state || !strings.Contains(rows[i].Node, w.node) || !strings.Contains(rows[i].Why, w.why) || rows[i].Why == "" {
			t.Errorf("row %d = %+v, want state %s naming %s / %q", i, rows[i], w.state, w.node, w.why)
		}
		if !strings.Contains(res.Reason, rows[i].Why) {
			t.Errorf("the reason must quote row %d: %s", i, res.Reason)
		}
	}
	noPosts(t, "the busy node", busy)
	noPosts(t, "the other build", other)

	// A free local result is never annotated: the block belongs to a deferral.
	lr2 := &laneRunner{verdict: heldLane()}
	ok := Run(context.Background(), cfg, lr2, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if !ok.OK || strings.Contains(string(ok.Data), "cluster") {
		t.Errorf("a local call that ran is not annotated: %+v", ok)
	}
}

// A node that stopped answering is not probed again at once: it backs off 5, 15, 60 and then 300 seconds per
// consecutive failed probe (chosen constants), so a powered-off box costs one probe per step and not one per call.
func TestDeadNodeBacksOff(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	good := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, good)
	const dead = "http://127.0.0.1:9" // never listened on
	cfg.DelegateRemotes = []string{dead, good.srv.URL}

	var deadProbes int
	prev := probeRoster
	probeRoster = func(ctx context.Context, remotes []string, token string, timeout time.Duration) []rosterprobe.Reading {
		var out []rosterprobe.Reading
		for i, b := range remotes {
			if b == dead {
				deadProbes++
				out = append(out, rosterprobe.Reading{Member: rosterprobe.Member{Index: i, Base: b}, Err: errors.New("dial tcp 127.0.0.1:9: connection refused"), Source: rosterprobe.SourceProbe})
				continue
			}
			r := prev(ctx, []string{b}, token, timeout)[0]
			r.Member.Index = i
			out = append(out, r)
		}
		return out
	}
	t.Cleanup(func() { probeRoster = prev })
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	prevNow := placerNow
	placerNow = func() time.Time { return now }
	t.Cleanup(func() { placerNow = prevNow })

	call := func() core.Result {
		lr := &laneRunner{verdict: heldLane()}
		res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
		if !res.OK || res.Meta.Node != "node-b" {
			t.Fatalf("the live node serves the call whatever the dead one does: %+v", res)
		}
		return res
	}
	// step: wait since the previous call, probes of the dead node so far
	for _, s := range []struct {
		advance time.Duration
		probes  int
		note    string
	}{
		{0, 1, "first call: probed, fails (backs off 5 s)"},
		{4 * time.Second, 1, "inside the 5 s: not probed"},
		{2 * time.Second, 2, "past 5 s: probed again, fails (backs off 15 s)"},
		{14 * time.Second, 2, "inside the 15 s: not probed"},
		{2 * time.Second, 3, "past 15 s: probed, fails (backs off 60 s)"},
		{59 * time.Second, 3, "inside the 60 s: not probed"},
		{2 * time.Second, 4, "past 60 s: probed, fails (backs off 300 s)"},
		{299 * time.Second, 4, "inside the 300 s: not probed"},
		{2 * time.Second, 5, "past 300 s: probed, fails (stays at 300 s)"},
		{299 * time.Second, 5, "the cap holds"},
	} {
		now = now.Add(s.advance)
		call()
		if deadProbes != s.probes {
			t.Fatalf("%s: the dead node was probed %d times, want %d", s.note, deadProbes, s.probes)
		}
	}
	// A node that answers again is forgotten at once.
	placer.ok(dead)
	if _, backed := placer.backedOff(dead); backed {
		t.Error("a node that answered carries no backoff")
	}
}

// The seed the caller gave is the seed the node renders, above 2^53 too.
func TestSeedTravelsExactly(t *testing.T) {
	const big = 9007199254740993 // 2^53 + 1: not a float64
	p, err := plan(core.Request{Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"seed": big, "width": int64(2048), "steps": "30"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := p.payload["seed"].(int); !ok || got != big {
		t.Errorf("seed = %#v, want the exact %d", p.payload["seed"], big)
	}
	if p.payload["width"] != 2048 || p.payload["steps"] != 30 {
		t.Errorf("width/steps = %#v / %#v", p.payload["width"], p.payload["steps"])
	}

	n := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)
	lr := &laneRunner{verdict: heldLane()}
	if res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", map[string]any{"seed": big}), "auto", nil); !res.OK {
		t.Fatal(res)
	}
	if got := n.runner.last().Params["seed"]; got != big {
		t.Errorf("the node's pipeline got seed %v, want %d", got, big)
	}
}
