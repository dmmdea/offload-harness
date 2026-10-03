package pairworkloads

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

type remoteRig struct {
	t    *testing.T
	cap  *capture
	srv  *httptest.Server
	e    *Emitter
	led  *ledger.Ledger
	path string
}

func newRemoteRig(t *testing.T, enabled bool) *remoteRig {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	t.Cleanup(srv.Close)
	e := New(Config{Enabled: enabled, Endpoint: srv.URL, OpenDir: t.TempDir(), AppDir: writeAttributionAppDir(t, viewOnlyFixture)})
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	e.AttachLedger(l) // the observer must leave the remote rows alone
	return &remoteRig{t: t, cap: c, srv: srv, e: e, led: l, path: path}
}

func (r *remoteRig) rows() []ledger.Entry {
	r.t.Helper()
	rows, err := ledger.ReadAll(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

// frames returns the frames by state, failing when one job id is not the only id (one card per call).
func (r *remoteRig) frames() map[string]map[string]any {
	r.t.Helper()
	r.e.Wait()
	out := map[string]map[string]any{}
	ids := map[any]bool{}
	for i := 0; i < r.cap.count(); i++ {
		wi := r.cap.info(i)
		ids[wi["id"]] = true
		out[wi["state"].(string)] = wi
	}
	if len(ids) > 1 {
		r.t.Fatalf("one call opened %d cards: %v", len(ids), ids)
	}
	return out
}

func vqaReq() core.Request {
	return core.Request{Task: core.TaskVQA, Door: "offload_vqa", Input: "what is this"}
}

// D5/D6: one dispatched call is ONE card on the node that served it and ONE asker row that the ledger
// observer does not card a second time.
func TestRemoteCallIsOneCardOnTheServingNodeAndOneRow(t *testing.T) {
	r := newRemoteRig(t, true)
	h := NewRemoteCall(r.e, r.led, vqaReq(), "remote")
	h.Dispatched("http://NODE-B:18811", "node-b-fleet16", "vision-abc123")
	h.Running()
	h.Running() // once
	h.Finish(core.Result{OK: true, Meta: core.Meta{Node: "node-b-fleet16", Model: "qwen3-vl-8b", Placement: "remote: forced", TokensIn: 7, TokensOut: 3}})
	h.Finish(core.Result{OK: true}) // once

	f := r.frames()
	if len(f) != 3 {
		t.Fatalf("states = %v, want queued, running, completed", f)
	}
	for state, wi := range f {
		if wi["id"] != "vision-abc123" || wi["engine"] != "llamacpp" || wi["scheduledOn"] != "node-b-uuid" || wi["originatedFrom"] != "self-uuid" {
			t.Errorf("%s frame: %v", state, wi)
		}
		if wi["requesterId"] == nil {
			t.Errorf("%s frame carries no requester", state)
		}
	}
	if f["queued"]["startedAt"] != nil || f["running"]["startedAt"] == nil || f["completed"]["completedAt"] == nil || f["completed"]["model"] != "qwen3-vl-8b" {
		t.Fatalf("lifecycle timestamps wrong: %v", f)
	}
	if r.cap.count() != 3 {
		t.Fatalf("frames = %d, want exactly 3 (queued, running, completed)", r.cap.count())
	}
	rows := r.rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want ONE asker row", len(rows))
	}
	row := rows[0]
	if row.Task != "vqa" || row.Door != "offload_vqa" || row.Route != "remote" || row.Placement != "remote: forced" ||
		row.Node != "node-b" || row.NodeID != "node-b-fleet16" || row.FleetJobID != "vision-abc123" ||
		!row.CardByCaller || row.Deferred || row.ModelTier != "qwen3-vl-8b" || row.TokensIn != 7 {
		t.Fatalf("asker row = %+v", row)
	}
}

// The dispatch host may not be a member name at all (an address literal the config uses); the fleet
// node id from health is the alias that still lands the card on the right node.
func TestRemoteCallResolvesThroughTheFleetNodeAlias(t *testing.T) {
	r := newRemoteRig(t, true)
	h := NewRemoteCall(r.e, r.led, vqaReq(), "auto")
	h.Dispatched("http://203.0.113.5:18811", "node-b", "vision-1")
	h.Finish(core.Result{OK: true, Meta: core.Meta{Node: "node-b"}})
	f := r.frames()
	if f["queued"]["scheduledOn"] != "node-b-uuid" || f["completed"]["scheduledOn"] != "node-b-uuid" {
		t.Fatalf("alias did not land the card on node-b: %v", f)
	}
	// A node that answered ran the job: the completed card has a start.
	if f["completed"]["startedAt"] == nil {
		t.Fatalf("a completed card without startedAt reads 'never started': %v", f["completed"])
	}
}

// A view-only node serves a call (D10 + D5 together): the card names it.
func TestRemoteCallOnAViewOnlyNode(t *testing.T) {
	r := newRemoteRig(t, true)
	h := NewRemoteCall(r.e, r.led, vqaReq(), "remote")
	h.Dispatched("http://node-v:18811", "node-v-fleet16", "vision-2")
	h.Finish(core.Result{OK: true, Meta: core.Meta{Node: "node-v-fleet16"}})
	if got := r.frames()["queued"]["scheduledOn"]; got != "view-uuid" {
		t.Fatalf("scheduledOn = %v, want the view-only node", got)
	}
}

func TestRemoteCallFailureCarriesTheReasonAndNoStart(t *testing.T) {
	r := newRemoteRig(t, true)
	h := NewRemoteCall(r.e, r.led, vqaReq(), "remote")
	h.Dispatched("http://node-b:18811", "node-b-fleet16", "vision-3")
	res := core.Deferf("dispatch http://node-b:18811: status 503: queue full", "", core.Meta{Placement: "remote: forced"})
	res.DeferClass = core.DeferClassCapacity
	h.Finish(res)
	f := r.frames()
	if len(f) != 2 || f["failed"]["error"] != "dispatch http://node-b:18811: status 503: queue full" || f["failed"]["startedAt"] != nil {
		t.Fatalf("frames = %v", f)
	}
	rows := r.rows()
	if len(rows) != 1 || !rows[0].Deferred || rows[0].Reason == "" || !rows[0].CardByCaller {
		t.Fatalf("rows = %+v", rows)
	}
}

// (c) A placement defer before any dispatch wrote no card, but the call still has its ledger row, and
// the observer does not turn that row into a card on this box.
func TestRemoteCallThatNeverReachedANodeHasARowAndNoCard(t *testing.T) {
	r := newRemoteRig(t, true)
	h := NewRemoteCall(r.e, r.led, vqaReq(), "remote")
	res := core.Deferf("no fleet node is eligible for the vision lane", "", core.Meta{Placement: "remote: forced"})
	h.Finish(res)
	r.e.Wait()
	if r.cap.count() != 0 {
		t.Fatalf("frames = %d, want none: no node was chosen", r.cap.count())
	}
	rows := r.rows()
	if len(rows) != 1 || rows[0].Node != "" || !rows[0].CardByCaller || !rows[0].Deferred || rows[0].Placement != "remote: forced" {
		t.Fatalf("rows = %+v", rows)
	}
}

// The auto route falling back to a local run writes the local run's own row, so an attempt that never
// reached a node leaves nothing behind, and one that did leaves a failed card and its row.
func TestDiscardWritesNothingBeforeDispatchAndClosesTheCardAfter(t *testing.T) {
	r := newRemoteRig(t, true)
	NewRemoteCall(r.e, r.led, vqaReq(), "auto").Discard("no fleet node is eligible")
	r.e.Wait()
	if r.cap.count() != 0 || len(r.rows()) != 0 {
		t.Fatalf("a discarded attempt that reached no node must write nothing: frames %d rows %d", r.cap.count(), len(r.rows()))
	}

	h := NewRemoteCall(r.e, r.led, vqaReq(), "auto")
	h.Dispatched("http://node-b:18811", "node-b-fleet16", "vision-4")
	h.Discard("node node-b-fleet16: job vision-4: 5 consecutive poll failures")
	h.Finish(core.Result{OK: true}) // already ended
	f := r.frames()
	if f["failed"] == nil || f["queued"] == nil || r.cap.count() != 2 {
		t.Fatalf("a discarded dispatched attempt must close its card failed: %v (%d frames)", f, r.cap.count())
	}
	if rows := r.rows(); len(rows) != 1 || !rows[0].Deferred || rows[0].Node != "node-b" {
		t.Fatalf("rows = %+v", rows)
	}
}

// With the emitter disabled (key off, PAIR absent) the call still has its row and nothing is posted.
func TestRemoteCallWithADisabledEmitterStillWritesTheRow(t *testing.T) {
	r := newRemoteRig(t, false)
	h := NewRemoteCall(r.e, r.led, vqaReq(), "remote")
	h.Dispatched("http://node-b:18811", "node-b-fleet16", "vision-5")
	h.Finish(core.Result{OK: true, Meta: core.Meta{Node: "node-b-fleet16"}})
	r.e.Wait()
	if r.cap.count() != 0 {
		t.Fatalf("a disabled emitter posted %d frames", r.cap.count())
	}
	if rows := r.rows(); len(rows) != 1 || rows[0].Node != "node-b" {
		t.Fatalf("rows = %+v", rows)
	}
	// Nil emitter and nil ledger are tolerated.
	n := NewRemoteCall(nil, nil, vqaReq(), "remote")
	n.Dispatched("http://node-b:1", "x", "id")
	n.Running()
	n.Finish(core.Result{OK: true})
}

func TestRemoteComposeCardIsHyperframes(t *testing.T) {
	r := newRemoteRig(t, true)
	h := NewRemoteCall(r.e, r.led, core.Request{Task: core.TaskComposeVideo, Door: "offload_compose_video"}, "remote")
	h.Dispatched("http://node-b:18811", "node-b-fleet16", "compose-1")
	h.Finish(core.Result{OK: true, Meta: core.Meta{Node: "node-b-fleet16", Model: "hyperframes"}})
	f := r.frames()
	if f["queued"]["engine"] != "hyperframes" || f["queued"]["model"] != "hyperframes" || f["completed"]["engine"] != "hyperframes" {
		t.Fatalf("compose card: %v", f)
	}
}
