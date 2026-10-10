package visionremote

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// attrLocal is the Runner the MCP server hands the lane in production: a local runner that is also
// the core.RemoteAttributor (the pipeline), here backed by the rig.
type attrLocal struct {
	*localRunner
	rig *pairRig
}

func (a attrLocal) BeginRemote(req core.Request, route string) core.RemoteAttribution {
	return a.rig.p.BeginRemote(req, route)
}

func hostOf(t *testing.T, f *fakeNode) string {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

func clientConfig(f *fakeNode) config.Config {
	cfg := config.Default()
	cfg.DelegateRemotes = []string{f.srv.URL}
	cfg.FleetAuthToken = "tok"
	return cfg
}

func visionReq(t *testing.T) core.Request {
	r := assessReq(pngFile(t))
	r.Door = "offload_assess_image"
	return r
}

// D5/D6: a remote vision call is ONE card on the node that served it (named by its dispatch host)
// and ONE asker ledger row the observer does not card a second time.
func TestRemoteVisionIsOneCardOnTheServingNodeAndOneRow(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c-ampere16", []string{"vision"}, remoteOK)
	host := hostOf(t, node)
	rig := newPairRig(t, true, host)
	local := attrLocal{&localRunner{}, rig}

	res := Run(context.Background(), clientConfig(node), local, visionReq(t), "remote")
	if !res.OK || res.Meta.Placement != "remote: forced" {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["queued"] == nil || cards["completed"] == nil {
		t.Fatalf("states = %v, want queued and completed", cards)
	}
	for state, wi := range cards {
		if wi["scheduledOn"] != host+"-uuid" || wi["originatedFrom"] != "self-uuid" || wi["engine"] != "llamacpp" {
			t.Errorf("%s frame: %v", state, wi)
		}
	}
	if cards["completed"]["model"] != "fake-vlm" || cards["completed"]["startedAt"] == nil {
		t.Errorf("completed frame: %v", cards["completed"])
	}
	rows := rig.rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want ONE asker row: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Task != "assess_image" || row.Door != "offload_assess_image" || row.Route != "remote" || row.Placement != "remote: forced" ||
		row.Node != host || row.NodeID != "node-c-ampere16" || row.FleetJobID == "" || !row.CardByCaller || row.Deferred || row.ModelTier != "fake-vlm" {
		t.Fatalf("asker row = %+v", row)
	}
	if cards["queued"]["id"] != row.FleetJobID {
		t.Errorf("card id %v is not the fleet job id %q", cards["queued"]["id"], row.FleetJobID)
	}
	if local.count() != 0 {
		t.Fatal("route remote ran the local seat")
	}
}

func TestRemoteVisionCardResolvesThroughTheFleetNodeID(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)
	rig := newPairRig(t, true, "node-b")
	if res := Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, visionReq(t), "remote"); !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	if got := rig.cards()["queued"]["scheduledOn"]; got != "node-b-uuid" {
		t.Fatalf("scheduledOn = %v, want the member the fleet node id names", got)
	}
}

// (c) No node chosen: a row, no card.
func TestRemoteVisionThatNeverReachedANodeWritesARowAndNoCard(t *testing.T) {
	setBusy(t, false)
	noLane := newFakeNode(t, "node-a", []string{"agent"}, remoteOK)
	rig := newPairRig(t, true)
	res := Run(context.Background(), clientConfig(noLane), attrLocal{&localRunner{}, rig}, visionReq(t), "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("Run: %+v", res)
	}
	if rig.frameCount() != 0 {
		t.Fatalf("frames = %d, want none", rig.frameCount())
	}
	rows := rig.rows()
	if len(rows) != 1 || rows[0].Node != "" || !rows[0].CardByCaller || !rows[0].Deferred || rows[0].Route != "remote" {
		t.Fatalf("rows = %+v", rows)
	}
}

// The node refusing a dispatch the call opened a card for closes that card failed and leaves its row.
func TestRemoteVisionRefusedDispatchClosesTheCardFailed(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)
	node.refuse = 503
	rig := newPairRig(t, true, hostOf(t, node))
	res := Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, visionReq(t), "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["queued"] == nil || cards["failed"] == nil || cards["failed"]["startedAt"] != nil || cards["failed"]["error"] == nil {
		t.Fatalf("cards = %v, want queued then failed with a reason and no start", cards)
	}
	if rows := rig.rows(); len(rows) != 1 || !rows[0].Deferred || rows[0].Node == "" {
		t.Fatalf("rows = %+v", rows)
	}
}

// A node that answered with a defer that is NOT a held card (the call ran and broke) closes the card
// failed with the node's reason; the node ran it, so the card shows it started.
func TestRemoteVisionNodeDeferClosesTheCardFailed(t *testing.T) {
	setBusy(t, false)
	d := core.Deferf("vision call failed: the seat did not answer", "", core.Meta{ErrClass: "timeout", Model: "fake-vlm"})
	node := newFakeNode(t, "node-b", []string{"vision"}, d)
	rig := newPairRig(t, true, hostOf(t, node))
	res := Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, visionReq(t), "remote")
	if !res.Deferred {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["failed"] == nil || cards["failed"]["error"] != "vision call failed: the seat did not answer" || cards["failed"]["startedAt"] == nil || cards["completed"] != nil {
		t.Fatalf("cards = %v, want one failed card that started, carrying the node's reason", cards)
	}
	if rows := rig.rows(); len(rows) != 1 || !rows[0].Deferred || rows[0].ErrClass != "timeout" {
		t.Fatalf("rows = %+v", rows)
	}
}

// A node that answered that ANOTHER job holds its card (gpu_busy: the call never ran) is held back, not
// failed: the card closes quiet, completed with no start and the reason in `error` (PAIR has no
// cancelled state, pairworkloads.CardOutcome), and the asker's row keeps the class. The vision wire
// carries the whole Result back, so the class always arrives.
func TestRemoteVisionNodeHeldDeferClosesTheCardQuiet(t *testing.T) {
	setBusy(t, false)
	d := core.Deferf("gpu busy: a generation job holds the node's card", "", core.Meta{ErrClass: "gpu_busy", Model: "fake-vlm"})
	node := newFakeNode(t, "node-b", []string{"vision"}, d)
	rig := newPairRig(t, true, hostOf(t, node))
	res := Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, visionReq(t), "remote")
	if !res.Deferred || res.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["failed"] != nil || cards["completed"] == nil || cards["completed"]["error"] != "gpu busy: a generation job holds the node's card" || cards["completed"]["startedAt"] != nil {
		t.Fatalf("cards = %v, want one completed card with no start carrying the reason, and no failed frame", cards)
	}
	if rows := rig.rows(); len(rows) != 1 || !rows[0].Deferred || rows[0].ErrClass != "gpu_busy" || rows[0].Node == "" {
		t.Fatalf("rows = %+v, the asker's row keeps the class", rows)
	}
}

// auto spilled to a node is attributed like remote, with the auto route and its placement note.
func TestAutoSpilledVisionIsOneCardAndOneRow(t *testing.T) {
	setBusy(t, true)
	node := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)
	rig := newPairRig(t, true, hostOf(t, node))
	local := attrLocal{&localRunner{}, rig}
	res := Run(context.Background(), clientConfig(node), local, visionReq(t), "auto")
	if !res.OK || res.Meta.Placement != "remote: local gpu busy" || local.count() != 0 {
		t.Fatalf("Run: %+v (local ran %d)", res, local.count())
	}
	if cards := rig.cards(); cards["queued"] == nil || cards["completed"] == nil {
		t.Fatalf("cards = %v", cards)
	}
	rows := rig.rows()
	if len(rows) != 1 || rows[0].Route != "auto" || rows[0].Placement != "remote: local gpu busy" || !rows[0].CardByCaller {
		t.Fatalf("rows = %+v", rows)
	}
}

// Auto falling back to the local seat stays byte-identical: when no node was reached the attempt
// leaves NOTHING (the local run writes its own row through the pipeline), and when a node refused
// after a card opened, that card closes failed and the attempt keeps one deferred row.
func TestAutoFallingBackToLocalLeavesNothingWhenNoNodeWasReached(t *testing.T) {
	setBusy(t, true)
	noLane := newFakeNode(t, "node-a", []string{"agent"}, remoteOK)
	rig := newPairRig(t, true)
	local := attrLocal{&localRunner{res: core.Result{OK: true}}, rig}
	res := Run(context.Background(), clientConfig(noLane), local, visionReq(t), "auto")
	if !res.OK || local.count() != 1 {
		t.Fatalf("Run: %+v (local ran %d), want the local result", res, local.count())
	}
	if rig.frameCount() != 0 || len(rig.rows()) != 0 {
		t.Fatalf("a fallback that reached no node must write nothing: frames %d rows %d", rig.frameCount(), len(rig.rows()))
	}
}

func TestAutoFallingBackAfterARefusalKeepsTheFailedAttempt(t *testing.T) {
	setBusy(t, true)
	node := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)
	node.refuse = 503
	rig := newPairRig(t, true, hostOf(t, node))
	local := attrLocal{&localRunner{res: core.Result{OK: true}}, rig}
	res := Run(context.Background(), clientConfig(node), local, visionReq(t), "auto")
	if !res.OK || local.count() != 1 {
		t.Fatalf("Run: %+v (local ran %d), want the local result", res, local.count())
	}
	cards := rig.cards()
	if cards["queued"] == nil || cards["failed"] == nil {
		t.Fatalf("cards = %v, want the refused attempt's card closed failed", cards)
	}
	if rows := rig.rows(); len(rows) != 1 || !rows[0].Deferred || rows[0].Route != "auto" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestLocalVisionRoutesWriteNoRemoteAttribution(t *testing.T) {
	node := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)
	for name, c := range map[string]struct {
		route string
		busy  bool
	}{"local": {"local", true}, "default": {"", true}, "auto idle": {"auto", false}} {
		setBusy(t, c.busy)
		rig := newPairRig(t, true, hostOf(t, node))
		local := attrLocal{&localRunner{res: core.Result{OK: true}}, rig}
		if res := Run(context.Background(), clientConfig(node), local, visionReq(t), c.route); !res.OK || local.count() != 1 {
			t.Fatalf("%s: %+v", name, res)
		}
		if rig.frameCount() != 0 || len(rig.rows()) != 0 {
			t.Errorf("%s: attributed a call that ran here: frames %d rows %d", name, rig.frameCount(), len(rig.rows()))
		}
	}
}

// D7/D11: the dispatch names its asker, and says "the node cards this job" only when this box will not.
func TestRemoteVisionSendsTheAttributionHeaders(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)

	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
	if res := Run(context.Background(), clientConfig(node), &localRunner{}, visionReq(t), "remote"); !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	h := node.dispatchHeader()
	if h.Get(core.PairCardHeader) != core.PairCardNode || h.Get(core.AskerHeader) == "" {
		t.Errorf("emitter disabled: pair-card %q asker %q, want %q and a name", h.Get(core.PairCardHeader), h.Get(core.AskerHeader), core.PairCardNode)
	}

	rig := newPairRig(t, true)
	t.Setenv("OFFLOAD_PAIR_APPDIR", rig.appDir)
	cfg := clientConfig(node)
	cfg.PairWorkloadsEnabled = true
	if res := Run(context.Background(), cfg, attrLocal{&localRunner{}, rig}, visionReq(t), "remote"); !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	h = node.dispatchHeader()
	if _, sent := h[http.CanonicalHeaderKey(core.PairCardHeader)]; sent {
		t.Errorf("emitter enabled: %s sent (%q): the job would be carded twice", core.PairCardHeader, h.Get(core.PairCardHeader))
	}
	if h.Get(core.AskerHeader) != "node-a" {
		t.Errorf("emitter enabled: asker %q, want the member name node-a", h.Get(core.AskerHeader))
	}
}

// The node's own running state turns the card running, once, between queued and the terminal frame.
func TestRemoteVisionCardTurnsRunningWhenTheNodeSaysTheJobStarted(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)
	node.runningFirst = true
	rig := newPairRig(t, true, hostOf(t, node))
	if res := Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, visionReq(t), "remote"); !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["queued"] == nil || cards["running"] == nil || cards["completed"] == nil || rig.frameCount() != 3 {
		t.Fatalf("cards = %v (%d frames), want queued, running, completed", cards, rig.frameCount())
	}
}
