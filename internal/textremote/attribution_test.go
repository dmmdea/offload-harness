package textremote

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

func textReq() core.Request {
	r := classifyReq()
	r.Door = "offload_classify"
	return r
}

// D5/D6: a remote text call is ONE card on the node that served it (named by its dispatch host)
// and ONE asker ledger row the observer does not card a second time.
func TestRemoteTextIsOneCardOnTheServingNodeAndOneRow(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "rk-node-fleet", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	host := hostOf(t, node)
	rig := newPairRig(t, true, host)
	local := attrLocal{&localRunner{}, rig}

	res := Run(context.Background(), clientConfig(node), local, textReq(), "remote")
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
	if cards["completed"]["model"] != "npu-2b" || cards["completed"]["startedAt"] == nil {
		t.Errorf("completed frame: %v", cards["completed"])
	}
	rows := rig.rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want ONE asker row: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Task != "classify" || row.Door != "offload_classify" || row.Route != "remote" || row.Placement != "remote: forced" ||
		row.Node != host || row.NodeID != "rk-node-fleet" || row.FleetJobID == "" || !row.CardByCaller || row.Deferred || row.ModelTier != "npu-2b" {
		t.Fatalf("asker row = %+v", row)
	}
	if cards["queued"]["id"] != row.FleetJobID {
		t.Errorf("card id %v is not the fleet job id %q", cards["queued"]["id"], row.FleetJobID)
	}
	if local.count() != 0 {
		t.Fatal("route remote ran the local cascade")
	}
}

func TestRemoteTextCardResolvesThroughTheFleetNodeID(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-b", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	rig := newPairRig(t, true, "node-b")
	if res := Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, textReq(), "remote"); !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	if got := rig.cards()["queued"]["scheduledOn"]; got != "node-b-uuid" {
		t.Fatalf("scheduledOn = %v, want the member the fleet node id names", got)
	}
}

// (c) No node chosen: a row, no card.
func TestRemoteTextThatNeverReachedANodeWritesARowAndNoCard(t *testing.T) {
	setBusy(t, false)
	noLane := newFakeNode(t, "node-a", []string{"agent"}, nil, remoteOK)
	rig := newPairRig(t, true)
	res := Run(context.Background(), clientConfig(noLane), attrLocal{&localRunner{}, rig}, textReq(), "remote")
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
func TestRemoteTextRefusedDispatchClosesTheCardFailed(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-b", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	node.refuse = 503
	rig := newPairRig(t, true, hostOf(t, node))
	res := Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, textReq(), "remote")
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

// auto spilled to a node is attributed like remote, with the auto route and its placement note.
func TestAutoSpilledTextIsOneCardAndOneRow(t *testing.T) {
	setBusy(t, true)
	node := newFakeNode(t, "node-b", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	rig := newPairRig(t, true, hostOf(t, node))
	local := attrLocal{&localRunner{}, rig}
	res := Run(context.Background(), clientConfig(node), local, textReq(), "auto")
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

// Auto falling back to the local cascade stays byte-identical: when no node was reached the attempt
// leaves NOTHING (the local run writes its own row through the pipeline), and when a node refused
// after a card opened, that card closes failed and the attempt keeps one deferred row.
func TestAutoFallingBackToLocalLeavesNothingWhenNoNodeWasReached(t *testing.T) {
	setBusy(t, true)
	noLane := newFakeNode(t, "node-a", []string{"agent"}, nil, remoteOK)
	rig := newPairRig(t, true)
	local := attrLocal{&localRunner{res: core.Result{OK: true}}, rig}
	res := Run(context.Background(), clientConfig(noLane), local, textReq(), "auto")
	if !res.OK || local.count() != 1 {
		t.Fatalf("Run: %+v (local ran %d), want the local result", res, local.count())
	}
	if rig.frameCount() != 0 || len(rig.rows()) != 0 {
		t.Fatalf("a fallback that reached no node must write nothing: frames %d rows %d", rig.frameCount(), len(rig.rows()))
	}
}

func TestAutoFallingBackAfterARefusalKeepsTheFailedAttempt(t *testing.T) {
	setBusy(t, true)
	node := newFakeNode(t, "node-b", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	node.refuse = 503
	rig := newPairRig(t, true, hostOf(t, node))
	local := attrLocal{&localRunner{res: core.Result{OK: true}}, rig}
	res := Run(context.Background(), clientConfig(node), local, textReq(), "auto")
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

func TestLocalTextRoutesWriteNoRemoteAttribution(t *testing.T) {
	node := newFakeNode(t, "node-b", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	for name, c := range map[string]struct {
		route string
		busy  bool
	}{"local": {"local", true}, "default": {"", true}, "auto idle": {"auto", false}} {
		setBusy(t, c.busy)
		rig := newPairRig(t, true, hostOf(t, node))
		local := attrLocal{&localRunner{res: core.Result{OK: true}}, rig}
		if res := Run(context.Background(), clientConfig(node), local, textReq(), c.route); !res.OK || local.count() != 1 {
			t.Fatalf("%s: %+v", name, res)
		}
		if rig.frameCount() != 0 || len(rig.rows()) != 0 {
			t.Errorf("%s: attributed a call that ran here: frames %d rows %d", name, rig.frameCount(), len(rig.rows()))
		}
	}
}

// D7/D11: the dispatch names its asker, and says "the node cards this job" only when this box will not.
func TestRemoteTextSendsTheAttributionHeaders(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-b", []string{"text"}, []string{"classify", "extract"}, remoteOK)

	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
	if res := Run(context.Background(), clientConfig(node), &localRunner{}, textReq(), "remote"); !res.OK {
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
	if res := Run(context.Background(), cfg, attrLocal{&localRunner{}, rig}, textReq(), "remote"); !res.OK {
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
func TestRemoteTextCardTurnsRunningWhenTheNodeSaysTheJobStarted(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-b", []string{"text"}, []string{"classify", "extract"}, remoteOK)
	node.runningFirst = true
	rig := newPairRig(t, true, hostOf(t, node))
	if res := Run(context.Background(), clientConfig(node), attrLocal{&localRunner{}, rig}, textReq(), "remote"); !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["queued"] == nil || cards["running"] == nil || cards["completed"] == nil || rig.frameCount() != 3 {
		t.Fatalf("cards = %v (%d frames), want queued, running, completed", cards, rig.frameCount())
	}
}
