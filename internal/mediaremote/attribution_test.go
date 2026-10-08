package mediaremote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The remote-attribution contract of 0.165.0 (D5-D11), for the media lane: a call sent to a node is
// ONE card on the node that served it and ONE asker ledger row, and every dispatch names its asker.

func hostOf(t *testing.T, n *node) string {
	t.Helper()
	u, err := url.Parse(n.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

type attrDoor struct {
	req  func() core.Request
	path string
}

// attrDoors are the two ways a media call reaches a node: the tokenless dispatch (no input file) and the
// token-gated media-job door (a still travels in a bundle).
func attrDoors(t *testing.T) map[string]attrDoor {
	return map[string]attrDoor{
		"dispatch": {func() core.Request {
			r := video(nil)
			r.Door = "offload_generate_video"
			return r
		}, "/fleet/dispatch"},
		"media-job": {func() core.Request {
			r := video(map[string]any{"still": writeFile(t, t.TempDir(), "still.png", png)})
			r.Door = "offload_generate_video"
			return r
		}, "/fleet/media-job"},
	}
}

// postedJobID is the job id the call sent to the node on path.
func postedJobID(t *testing.T, n *node, path string) string {
	t.Helper()
	for _, p := range n.posts() {
		if p.path != path {
			continue
		}
		var w struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(p.body, &w); err != nil || w.JobID == "" {
			t.Fatalf("POST %s body has no job id (%v)", path, err)
		}
		return w.JobID
	}
	t.Fatalf("no POST to %s: %+v", path, n.posts())
	return ""
}

func TestRemoteMediaCallIsOneCardOnTheServingNodeAndOneRow(t *testing.T) {
	for name, door := range attrDoors(t) {
		t.Run(name, func(t *testing.T) {
			n := startNode(t, nodeOpts{mediaInputs: true})
			host := hostOf(t, n)
			rig := newPairRig(t, true, host)
			res := Run(context.Background(), clientCfg(t, n), rig.p, door.req(), "remote", nil)
			if !res.OK || res.Meta.Placement != "remote: forced" {
				t.Fatalf("Run: %+v", res)
			}
			jobID := postedJobID(t, n, door.path)

			cards := rig.cards()
			if cards["queued"] == nil || cards["completed"] == nil {
				t.Fatalf("states = %v, want queued and completed", cards)
			}
			for state, wi := range cards {
				if wi["scheduledOn"] != host+"-uuid" || wi["originatedFrom"] != "self-uuid" {
					t.Errorf("%s frame: %v", state, wi)
				}
			}
			rows := rig.rows()
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want ONE asker row: %+v", len(rows), rows)
			}
			row := rows[0]
			if row.Task != string(core.TaskGenerateVideo) || row.Door != "offload_generate_video" || row.Route != "remote" ||
				row.Placement != "remote: forced" || row.Node != host || row.NodeID != "render-node" || !row.CardByCaller || row.Deferred {
				t.Fatalf("asker row = %+v", row)
			}
			if row.FleetJobID != jobID || cards["queued"]["id"] != jobID {
				t.Errorf("the job the node was sent is %q, the row carries %q and the card %v", jobID, row.FleetJobID, cards["queued"]["id"])
			}
		})
	}
}

// The fleet node id is an alias: a dispatch host that is no PAIR member name still lands the card.
func TestRemoteMediaCardResolvesThroughTheFleetNodeID(t *testing.T) {
	n := startNode(t, nodeOpts{})
	rig := newPairRig(t, true, "render-node")
	if res := Run(context.Background(), clientCfg(t, n), rig.p, video(nil), "remote", nil); !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	if got := rig.cards()["completed"]["scheduledOn"]; got != "render-node-uuid" {
		t.Fatalf("scheduledOn = %v, want the member the fleet node id names", got)
	}
}

// auto sent to a node because this machine has no lane is attributed like remote, with the auto route. The route
// the ledger records is the NORMALISED one (local|auto|remote, core/remoteattr.go), whatever the caller typed: the
// MCP doors omit `route` and send "", and a caller may spell it with case or spaces. Handing the raw string to
// core.BeginRemote wrote the row with Route "" for the commonest remote spill and failed nothing (review of 0.170.0,
// C5C6).
func TestAutoSpilledMediaCallIsOneCardAndOneRow(t *testing.T) {
	for _, route := range []string{"auto", "", "  AUTO "} {
		t.Run(fmt.Sprintf("route %q", route), func(t *testing.T) {
			n := startNode(t, nodeOpts{})
			rig := newPairRig(t, true, hostOf(t, n))
			res := Run(context.Background(), clientCfg(t, n), rig.p, video(nil), route, nil)
			if !res.OK || res.Meta.Placement != "remote: no video lane on this machine" {
				t.Fatalf("Run: %+v", res)
			}
			if cards := rig.cards(); cards["queued"] == nil || cards["completed"] == nil {
				t.Fatalf("cards = %v", cards)
			}
			rows := rig.rows()
			if len(rows) != 1 || rows[0].Route != "auto" || rows[0].Placement != "remote: no video lane on this machine" || !rows[0].CardByCaller {
				t.Fatalf("rows = %+v, want one row with the normalised route \"auto\"", rows)
			}
		})
	}
}

// A forced remote call is recorded under "remote" however it was spelled.
func TestAForcedRemoteMediaCallIsRecordedUnderTheNormalisedRoute(t *testing.T) {
	dead := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{"http://127.0.0.1:1"}, FleetAuthToken: "tok"}
	for _, route := range []string{"remote", "REMOTE", " Remote "} {
		t.Run(fmt.Sprintf("route %q", route), func(t *testing.T) {
			rig := newPairRig(t, true)
			if res := Run(context.Background(), dead, rig.p, video(nil), route, nil); !res.Deferred {
				t.Fatalf("Run: %+v, want a defer: no node answers", res)
			}
			if rows := rig.rows(); len(rows) != 1 || rows[0].Route != "remote" {
				t.Fatalf("rows = %+v, want one row with the normalised route \"remote\"", rows)
			}
		})
	}
}

// A call that never reached a node still has its row, and no card: a roster nobody answers, a request
// refused before the network, an unreadable input file.
func TestRemoteMediaCallThatNeverReachedANodeWritesARowAndNoCard(t *testing.T) {
	dead := config.Config{MediaDir: t.TempDir(), DelegateRemotes: []string{"http://127.0.0.1:1"}, FleetAuthToken: "tok"}
	missing := video(map[string]any{"still": t.TempDir() + "/nope.png"})
	for name, c := range map[string]struct {
		cfg   config.Config
		req   core.Request
		class string
	}{
		"no node answers":  {dead, video(nil), core.DeferClassCapacity},
		"no fleet at all":  {config.Config{MediaDir: t.TempDir()}, video(nil), core.DeferClassConfig},
		"unreadable input": {dead, missing, ""},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newPairRig(t, true)
			res := Run(context.Background(), c.cfg, rig.p, c.req, "remote", nil)
			if !res.Deferred || (c.class != "" && res.DeferClass != c.class) {
				t.Fatalf("Run: %+v", res)
			}
			if n := rig.frameCount(); n != 0 {
				t.Fatalf("frames = %d, want none: no node was chosen", n)
			}
			rows := rig.rows()
			if len(rows) != 1 || rows[0].Node != "" || !rows[0].CardByCaller || !rows[0].Deferred || rows[0].Route != "remote" {
				t.Fatalf("rows = %+v", rows)
			}
		})
	}
}

// The node refusing the POST of a call that opened its card closes that card failed and leaves its row,
// on both doors.
func TestRemoteMediaRefusedDispatchClosesTheCardFailed(t *testing.T) {
	for name, door := range attrDoors(t) {
		t.Run(name, func(t *testing.T) {
			n := startNode(t, nodeOpts{mediaInputs: true})
			n.refusePost = http.StatusServiceUnavailable
			rig := newPairRig(t, true, hostOf(t, n))
			res := Run(context.Background(), clientCfg(t, n), rig.p, door.req(), "remote", nil)
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
		})
	}
}

// The node's own verdict on a job it ran (a render that deferred) closes the card failed with its reason.
func TestRemoteMediaNodeDeferClosesTheCardFailed(t *testing.T) {
	n := startNode(t, nodeOpts{})
	n.runner.deferAs = "ltx: out of memory"
	rig := newPairRig(t, true, hostOf(t, n))
	res := Run(context.Background(), clientCfg(t, n), rig.p, video(nil), "remote", nil)
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

// A call whose deadline passes while the node is rendering still ends its handle: the row is written and
// the card is closed failed, instead of staying queued for good.
func TestRemoteMediaCallThatRunsOutOfTimeStillClosesItsCard(t *testing.T) {
	hold := make(chan struct{})
	n := startNode(t, nodeOpts{hold: hold})
	defer close(hold)
	rig := newPairRig(t, true, hostOf(t, n))
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	res := Run(ctx, clientCfg(t, n), rig.p, video(nil), "remote", nil)
	if !res.Deferred || res.DeferClass != core.DeferClassBudget {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["queued"] == nil || cards["failed"] == nil {
		t.Fatalf("cards = %v, want the card closed failed", cards)
	}
	if rows := rig.rows(); len(rows) != 1 || !rows[0].Deferred || rows[0].Node == "" {
		t.Fatalf("rows = %+v", rows)
	}
}

// The local routes never reach the handle: Pipeline.Run owns the local row.
func TestLocalMediaRoutesWriteNoRemoteAttribution(t *testing.T) {
	n := startNode(t, nodeOpts{})
	withLane := localGraphCfg(t, n)
	noFleet := config.Config{MediaDir: t.TempDir()}
	for name, c := range map[string]struct {
		cfg   config.Config
		route string
		req   func() core.Request
	}{
		"local":              {withLane, "local", func() core.Request { return video(nil) }},
		"auto with a lane":   {withLane, "auto", func() core.Request { return graphReq(t) }},
		"default with lane":  {withLane, "", func() core.Request { return graphReq(t) }},
		"auto with no fleet": {noFleet, "auto", func() core.Request { return video(nil) }},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newPairRig(t, true)
			runner := &attrRunner{rig: rig}
			if res := Run(context.Background(), c.cfg, runner, c.req(), c.route, nil); !res.OK || runner.runs != 1 {
				t.Fatalf("Run: %+v (%d runs)", res, runner.runs)
			}
			if rig.frameCount() != 0 || len(rig.rows()) != 0 {
				t.Fatalf("a call that ran here was attributed as remote: frames %d rows %d", rig.frameCount(), len(rig.rows()))
			}
		})
	}
}

// attrRunner is a Runner that is also the rig's attributor, so a test can see the local path.
type attrRunner struct {
	rig  *pairRig
	runs int
}

func (a *attrRunner) Run(context.Context, core.Request) core.Result {
	a.runs++
	return core.Result{OK: true}
}

func (a *attrRunner) BeginRemote(req core.Request, route string) core.RemoteAttribution {
	return a.rig.p.BeginRemote(req, route)
}

// Every dispatch names its asker, and says "the node cards this job" ONLY when this box will not (its
// emitter is not enabled): both doors, the tokenless dispatch and the token-gated media-job.
func TestRemoteMediaSendsTheAttributionHeaders(t *testing.T) {
	for name, door := range attrDoors(t) {
		t.Run(name+"/emitter disabled", func(t *testing.T) {
			n := startNode(t, nodeOpts{mediaInputs: true})
			cfg := clientCfg(t, n) // pair_workloads_enabled is off
			t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
			if res := Run(context.Background(), cfg, &recordingRunner{}, door.req(), "remote", nil); !res.OK {
				t.Fatalf("Run: %+v", res)
			}
			h := postHeader(t, n, door.path)
			if h.Get(core.PairCardHeader) != core.PairCardNode {
				t.Errorf("%s = %q, want %q: nothing on this box will card the job", core.PairCardHeader, h.Get(core.PairCardHeader), core.PairCardNode)
			}
			if h.Get(core.AskerHeader) == "" {
				t.Errorf("%s missing", core.AskerHeader)
			}
		})
		t.Run(name+"/emitter enabled", func(t *testing.T) {
			n := startNode(t, nodeOpts{mediaInputs: true})
			cfg := clientCfg(t, n)
			cfg.PairWorkloadsEnabled = true
			rig := newPairRig(t, true)
			t.Setenv("OFFLOAD_PAIR_APPDIR", rig.appDir)
			if res := Run(context.Background(), cfg, rig.p, door.req(), "remote", nil); !res.OK {
				t.Fatalf("Run: %+v", res)
			}
			h := postHeader(t, n, door.path)
			if _, sent := h[http.CanonicalHeaderKey(core.PairCardHeader)]; sent {
				t.Errorf("%s sent (%q) by an asker that cards the job itself: it would be carded twice", core.PairCardHeader, h.Get(core.PairCardHeader))
			}
			if h.Get(core.AskerHeader) != "node-a" {
				t.Errorf("%s = %q, want the asker's PAIR member name node-a", core.AskerHeader, h.Get(core.AskerHeader))
			}
		})
	}
}

// postHeader is the headers of the call's POST to path.
func postHeader(t *testing.T, n *node, path string) http.Header {
	t.Helper()
	for _, p := range n.posts() {
		if p.path == path {
			return p.header
		}
	}
	t.Fatalf("no POST to %s: %+v", path, n.posts())
	return nil
}

// The node's own running state turns the card running, once, between queued and the terminal frame.
func TestRemoteMediaCardTurnsRunningWhenTheNodeSaysTheJobStarted(t *testing.T) {
	hold := make(chan struct{})
	n := startNode(t, nodeOpts{hold: hold})
	rig := newPairRig(t, true, hostOf(t, n))
	done := make(chan core.Result, 1)
	go func() { done <- Run(context.Background(), clientCfg(t, n), rig.p, video(nil), "remote", nil) }()
	// The job is claimed as soon as it is admitted and the runner then blocks on hold: wait until the
	// client has polled a few times, so it has seen "running", then let the render finish.
	deadline := time.Now().Add(5 * time.Second)
	for polls(n) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(hold)
	if res := <-done; !res.OK {
		t.Fatalf("Run: %+v", res)
	}
	cards := rig.cards()
	if cards["queued"] == nil || cards["running"] == nil || cards["completed"] == nil || rig.frameCount() != 3 {
		t.Fatalf("cards = %v (%d frames), want queued, running, completed", cards, rig.frameCount())
	}
}

func polls(n *node) int {
	c := 0
	for _, r := range n.requests() {
		if r.method == http.MethodGet && strings.HasPrefix(r.path, "/fleet/jobs/") {
			c++
		}
	}
	return c
}
