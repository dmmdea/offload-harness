package composeremote

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func dispatchHost(t *testing.T, n *node) string {
	t.Helper()
	u, err := url.Parse(n.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

// D5/D6: a remote composition is ONE card on the node that rendered it (named by its dispatch host)
// and ONE asker ledger row that the observer does not card a second time.
func TestRemoteComposeIsOneCardOnTheServingNodeAndOneRow(t *testing.T) {
	n := startNode(t, false)
	cfg := clientCfg(t, n)
	host := dispatchHost(t, n)
	rig := newPairRig(t, true, host)

	req := core.Request{Task: core.TaskComposeVideo, Door: "offload_compose_video", Params: map[string]any{"template": "title-card", "variables": map[string]any{"title": "Hi"}, "format": "mp4"}}
	res := Run(context.Background(), cfg, rig.p, req, "remote")
	if !res.OK || res.Meta.Placement != "remote: forced" {
		t.Fatalf("Run: %+v", res)
	}

	cards := rig.cards()
	if cards["queued"] == nil || cards["completed"] == nil {
		t.Fatalf("states = %v, want queued and completed", cards)
	}
	for state, wi := range cards {
		if wi["scheduledOn"] != host+"-uuid" || wi["originatedFrom"] != "self-uuid" || wi["engine"] != "hyperframes" {
			t.Errorf("%s frame: %v", state, wi)
		}
	}
	if cards["completed"]["startedAt"] == nil || cards["completed"]["error"] != nil {
		t.Errorf("completed frame: %v", cards["completed"])
	}
	rows := rig.rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want ONE asker row: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Task != "compose_video" || row.Door != "offload_compose_video" || row.Route != "remote" || row.Placement != "remote: forced" ||
		row.Node != host || row.NodeID != "render-node" || row.FleetJobID == "" || !row.CardByCaller || row.Deferred {
		t.Fatalf("asker row = %+v", row)
	}
	if id := cards["queued"]["id"]; id != row.FleetJobID {
		t.Errorf("card id %v is not the fleet job id %q the row carries", id, row.FleetJobID)
	}
}

// The fleet node id is an alias: a dispatch host that is no member name still lands the card.
func TestRemoteComposeCardResolvesThroughTheFleetNodeID(t *testing.T) {
	n := startNode(t, false)
	rig := newPairRig(t, true, "render-node")
	res := Run(context.Background(), clientCfg(t, n), rig.p, core.Request{Task: core.TaskComposeVideo, Params: map[string]any{"template": "title-card"}}, "remote")
	if !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	if got := rig.cards()["completed"]["scheduledOn"]; got != "render-node-uuid" {
		t.Fatalf("scheduledOn = %v, want the member the fleet node id names", got)
	}
}

// The node's own refusal after dispatch is the call's terminal card (failed, with the reason) and
// its one row.
func TestRemoteComposeNodeDeferClosesTheCardFailed(t *testing.T) {
	n := startNode(t, false)
	n.runner.defer_ = "compose_video: LINT_ERRORS: 2 errors"
	rig := newPairRig(t, true, dispatchHost(t, n))
	res := Run(context.Background(), clientCfg(t, n), rig.p, core.Request{Task: core.TaskComposeVideo, Params: map[string]any{"template": "title-card"}}, "remote")
	if res.OK {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["failed"] == nil || cards["failed"]["error"] == nil || cards["completed"] != nil {
		t.Fatalf("cards = %v, want one failed card", cards)
	}
	if rows := rig.rows(); len(rows) != 1 || !rows[0].Deferred || rows[0].Reason == "" {
		t.Fatalf("rows = %+v", rows)
	}
}

// (c) A placement defer before any dispatch writes NO card, but the call still has its row.
func TestRemoteComposeThatNeverReachedANodeWritesARowAndNoCard(t *testing.T) {
	dead := &config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{"http://127.0.0.1:1"}, FleetAuthToken: "tok"}
	rig := newPairRig(t, true)
	res := Run(context.Background(), *dead, rig.p, core.Request{Task: core.TaskComposeVideo, Door: "cli:compose-video", Params: map[string]any{"template": "title-card"}}, "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("Run: %+v, want a capacity defer", res)
	}
	if n := rig.frameCount(); n != 0 {
		t.Fatalf("frames = %d, want none: no node was chosen", n)
	}
	rows := rig.rows()
	if len(rows) != 1 || rows[0].Node != "" || !rows[0].CardByCaller || !rows[0].Deferred || rows[0].Route != "remote" {
		t.Fatalf("rows = %+v", rows)
	}

	// An auto call with no lane here and no remotes at all is the same: a row, no card.
	rig2 := newPairRig(t, true)
	res = Run(context.Background(), config.Config{}, rig2.p, core.Request{Task: core.TaskComposeVideo}, "auto")
	if res.DeferClass != core.DeferClassConfig {
		t.Fatalf("Run: %+v", res)
	}
	if rig2.frameCount() != 0 || len(rig2.rows()) != 1 {
		t.Fatalf("auto with no remotes: frames %d rows %d", rig2.frameCount(), len(rig2.rows()))
	}
}

// The route the ledger records is the NORMALISED one (local|auto|remote, core/remoteattr.go), whatever the caller
// typed: the MCP door omits `route` and sends "", and a caller may spell it with case or spaces. Handing the raw
// string to core.BeginRemote wrote the asker row with Route "" or "REMOTE" and failed nothing (review of 0.170.0,
// C5C6). No node and no composition lane here, so the call is deferred after it opened its attribution: one row.
func TestARemoteComposeCallIsRecordedUnderTheNormalisedRoute(t *testing.T) {
	for route, want := range map[string]string{
		"": "auto", "auto": "auto", "  AUTO ": "auto",
		"remote": "remote", "REMOTE": "remote", " Remote ": "remote",
	} {
		t.Run(fmt.Sprintf("route %q", route), func(t *testing.T) {
			rig := newPairRig(t, true)
			res := Run(context.Background(), config.Config{}, rig.p, core.Request{Task: core.TaskComposeVideo}, route)
			if !res.Deferred {
				t.Fatalf("Run: %+v, want a defer: this box has no lane and no remotes", res)
			}
			if rows := rig.rows(); len(rows) != 1 || rows[0].Route != want {
				t.Fatalf("rows = %+v, want one row with the normalised route %q", rows, want)
			}
		})
	}
}

// The local route is byte-identical: it never reaches the attribution handle, so it writes nothing
// of its own here (Pipeline.Run owns the local row).
func TestLocalComposeRouteWritesNoRemoteAttribution(t *testing.T) {
	rig := newPairRig(t, true)
	runner := &fixedRunner{rig: rig}
	res := Run(context.Background(), config.Config{}, runner, core.Request{Task: core.TaskComposeVideo}, "local")
	if !res.OK || runner.runs != 1 {
		t.Fatalf("Run: %+v (%d runs)", res, runner.runs)
	}
	if rig.frameCount() != 0 || len(rig.rows()) != 0 {
		t.Fatalf("local route attributed a remote call: frames %d rows %d", rig.frameCount(), len(rig.rows()))
	}
}

// fixedRunner is a Runner that is also the rig's attributor, so a test can see the local path.
type fixedRunner struct {
	rig  *pairRig
	runs int
}

func (f *fixedRunner) Run(context.Context, core.Request) core.Result {
	f.runs++
	return core.Result{OK: true}
}

func (f *fixedRunner) BeginRemote(req core.Request, route string) core.RemoteAttribution {
	return f.rig.p.BeginRemote(req, route)
}

// D7/D11: every dispatch names its asker, and says "the node cards this job" ONLY when this box will
// not (its emitter is not enabled). Both composeremote doors (template and project) carry them.
func TestRemoteComposeSendsTheAttributionHeaders(t *testing.T) {
	proj := t.TempDir()
	_ = os.WriteFile(filepath.Join(proj, "index.html"), []byte(`<p>x</p>`), 0o644)
	for name, c := range map[string]struct {
		params map[string]any
		open   bool
	}{
		"template": {map[string]any{"template": "title-card"}, false},
		"project":  {map[string]any{"project_dir": proj}, true},
	} {
		t.Run(name+"/emitter disabled", func(t *testing.T) {
			n := startNode(t, c.open)
			cfg := clientCfg(t, n) // pair_workloads_enabled is off
			t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
			if res := Run(context.Background(), cfg, &localOnly{}, core.Request{Task: core.TaskComposeVideo, Params: c.params}, "remote"); !res.OK {
				t.Fatalf("Run: %+v", res)
			}
			h := n.postHeader()
			if h.Get(core.PairCardHeader) != core.PairCardNode {
				t.Errorf("%s = %q, want %q: nothing on this box will card the job", core.PairCardHeader, h.Get(core.PairCardHeader), core.PairCardNode)
			}
			if h.Get(core.AskerHeader) == "" {
				t.Errorf("%s missing", core.AskerHeader)
			}
		})
		t.Run(name+"/emitter enabled", func(t *testing.T) {
			n := startNode(t, c.open)
			cfg := clientCfg(t, n)
			cfg.PairWorkloadsEnabled = true
			rig := newPairRig(t, true)
			t.Setenv("OFFLOAD_PAIR_APPDIR", rig.appDir)
			if res := Run(context.Background(), cfg, rig.p, core.Request{Task: core.TaskComposeVideo, Params: c.params}, "remote"); !res.OK {
				t.Fatalf("Run: %+v", res)
			}
			h := n.postHeader()
			if _, sent := h[http.CanonicalHeaderKey(core.PairCardHeader)]; sent {
				t.Errorf("%s sent (%q) by an asker that cards the job itself: it would be carded twice", core.PairCardHeader, h.Get(core.PairCardHeader))
			}
			if h.Get(core.AskerHeader) != "node-a" {
				t.Errorf("%s = %q, want the asker's PAIR member name node-a", core.AskerHeader, h.Get(core.AskerHeader))
			}
		})
	}
}

// localOnly is a Runner with no attribution (a test double, a node's own runner).
type localOnly struct{}

func (localOnly) Run(context.Context, core.Request) core.Result { return core.Result{OK: true} }

// A render the node is still working on reads "running" on the first poll: the card turns running,
// once, between queued and the terminal frame.
func TestRemoteComposeCardTurnsRunningWhenTheNodeSaysTheJobStarted(t *testing.T) {
	n := startNode(t, false)
	n.runner.delay = 600 * time.Millisecond
	rig := newPairRig(t, true, dispatchHost(t, n))
	res := Run(context.Background(), clientCfg(t, n), rig.p, core.Request{Task: core.TaskComposeVideo, Params: map[string]any{"template": "title-card"}}, "remote")
	if !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["queued"] == nil || cards["running"] == nil || cards["completed"] == nil || rig.frameCount() != 3 {
		t.Fatalf("cards = %v (%d frames), want queued, running, completed", cards, rig.frameCount())
	}
}
