package pipeline

// The place a queued whole-node graph keeps in this box's own line (core.PlaceKeeper for run_graph; ADR 0083 Amendment 1). A graph
// ticket (internal/mediaremote) waits for a lane anywhere in the cluster, and the place it keeps here is what makes it senior to every
// caller that arrives after it, exactly as an image ticket's is. A graph that names no devices holds the WHOLE node, so its place is
// the whole-node one, the token the real admission leaves for it (wholeNodeBusy) and spends when the ticket's final local run
// resumes it with its waiter_token.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func graphPlaceOf(t *testing.T, f *admitFixture, params map[string]any, since time.Time) core.MediaPlace {
	t.Helper()
	pl, err := f.p.LeaveMediaPlace(context.Background(), graphLaneReq(params), since)
	if err != nil {
		t.Fatalf("LeaveMediaPlace for a graph: %v", err)
	}
	return pl
}

// A graph's place is a real token of the lease directory, on the whole node (no card named), for the run-graph reason, with the
// arrival time the ticket asked to keep.
func TestLeaveMediaPlaceForAGraphHoldsTheWholeNode(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	arrived := time.Now().Add(-2 * time.Minute)
	pl := graphPlaceOf(t, f, nil, arrived)
	if len(pl.Token) < 3 || pl.Token[:3] != "tk-" {
		t.Fatalf("a place is a lease-directory token: %+v", pl)
	}
	if len(pl.Devices) != 0 {
		t.Errorf("a whole-node graph holds no named card: %+v", pl.Devices)
	}
	if pl.Since.UnixMilli() != arrived.UnixMilli() {
		t.Errorf("the place keeps the arrival it was given: %v want %v", pl.Since, arrived)
	}
	tok, ok := f.m.ResumeToken(pl.Token)
	if !ok || tok.SinceMs != arrived.UnixMilli() || len(tok.Devices) != 0 || tok.Class != gpulease.ClassMedia || tok.Reason != "run-graph" {
		t.Errorf("the lease manager must hold that place, for run-graph, on the whole node: %+v ok=%v", tok, ok)
	}
}

// THE SENIORITY RULE for a graph. A kept place holds the node against every caller that arrives after it; the graph that resumes
// the place is served before them; a place nobody keeps stops holding after the grace. And the ticket's final local run, which
// resumes the place through waiter_token, is granted by the REAL admission and spends the place.
func TestAGraphPlaceHoldsSeniorityAndIsSpentByTheLocalRun(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	pl := graphPlaceOf(t, f, nil, time.Now().Add(-time.Minute))

	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(map[string]any{"waiter_token": pl.Token})); !v.Free {
		t.Errorf("the graph that holds the place is served first: %s", v.Why)
	}
	late := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil))
	if late.Free || late.Ahead < 1 {
		t.Errorf("a graph that arrives after the place queues behind it: free=%v ahead=%d (%s)", late.Free, late.Ahead, late.Why)
	}
	if img := f.p.MediaLaneFree(context.Background(), laneReq(nil)); img.Free {
		t.Errorf("an image that arrives after the place queues behind a whole-node place: %s", img.Why)
	}
	backdatePoll(t, f, pl.Token, 2*gpulease.TokenGrace)
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); !v.Free {
		t.Errorf("a place nobody keeps holds nothing: %s", v.Why)
	}
	if !f.p.KeepMediaPlace(pl.Token) {
		t.Fatal("keeping a place that exists must succeed")
	}
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); v.Free {
		t.Error("a kept place holds the node again")
	}

	// The ticket's local run: the real wait-0 admission grants it through the place, and the place is spent.
	if ok, err := graphGrant(f, graphLaneReq(map[string]any{"waiter_token": pl.Token})); !ok {
		t.Fatalf("the final local run resumes the place and is granted: %v", err)
	}
	if _, err := os.Stat(tokenFile(f, pl.Token)); !os.IsNotExist(err) {
		t.Errorf("the place is spent by the run that served it: %v", err)
	}
}

// A graph that already holds a place of the local line brings it along, and a token that names nothing is a new arrival.
func TestLeaveMediaPlaceKeepsAGraphsResumedPlace(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	old := time.Now().Add(-7 * time.Minute)
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "run-graph"}, old)
	if err != nil {
		t.Fatal(err)
	}
	pl := graphPlaceOf(t, f, map[string]any{"waiter_token": tok.ID}, time.Now())
	if pl.Token != tok.ID || pl.Since.UnixMilli() != old.UnixMilli() {
		t.Errorf("the place that was held is the place that is kept: %+v want token %s since %v", pl, tok.ID, old)
	}
	fresh := graphPlaceOf(t, f, map[string]any{"waiter_token": "tk-doesnotexist1"}, time.Time{})
	if fresh.Token == "" || fresh.Token == "tk-doesnotexist1" {
		t.Errorf("an unknown token is a new arrival: %+v", fresh)
	}
}

// A box that cannot keep a graph's place says so with the one typed error, and the graph is never an error for it. A graph that
// names devices is card-scoped and stays here: it is no ticket, so it has no place to keep.
func TestLeaveMediaPlaceSaysWhenTheBoxKeepsNoPlaceForAGraph(t *testing.T) {
	t.Run("a host that does not lease cards", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, noAudit: true})
		if _, err := f.p.LeaveMediaPlace(context.Background(), graphLaneReq(nil), time.Time{}); !errors.Is(err, core.ErrNoMediaPlace) {
			t.Errorf("err = %v, want ErrNoMediaPlace", err)
		}
	})
	t.Run("a graph under an inherited lease", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
		l := hold(t, f, "the parent's reservation", leaseIDOf(admitUUIDA))
		for _, kv := range f.p.mediaGrantEnv(l, []string{leaseIDOf(admitUUIDA)}, "") {
			k, v, _ := strings.Cut(kv, "=")
			t.Setenv(k, v)
		}
		if _, err := f.p.LeaveMediaPlace(context.Background(), graphLaneReq(nil), time.Time{}); !errors.Is(err, core.ErrNoMediaPlace) {
			t.Errorf("err = %v, want ErrNoMediaPlace", err)
		}
	})
	t.Run("a graph that names devices", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
		if _, err := f.p.LeaveMediaPlace(context.Background(), graphLaneReq(map[string]any{"devices": []string{"0"}}), time.Time{}); !errors.Is(err, core.ErrNoMediaPlace) {
			t.Errorf("err = %v, want ErrNoMediaPlace", err)
		}
	})
}
