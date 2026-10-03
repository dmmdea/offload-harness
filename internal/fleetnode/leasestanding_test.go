package fleetnode

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// leaseBlock decodes just the lease block of /fleet/health as a raw map, so the test sees
// exactly which keys are on the wire.
func leaseBlock(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health %d", rec.Code)
	}
	var out struct {
		Lease map[string]any `json:"lease"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Lease
}

func standingServer(t *testing.T, st *gpulease.Standing) *Server {
	t.Helper()
	opts := &Options{
		NodeID:     "testnode",
		Snapshot:   goodSnapshot,
		Footprints: func() []FootprintEntry { return nil },
		Lease: func() gpulease.Info {
			return gpulease.Info{Held: true, Class: gpulease.ClassMedia, PID: 42, Epoch: 41, ExpiresAt: time.Now().Add(6 * time.Hour)}
		},
	}
	if st != nil {
		opts.LeaseStanding = func(gpulease.Info) gpulease.Standing { return *st }
	}
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, opts)
	return s
}

// The node publishes what its lease is doing, not only that it is held: a delegator that
// reads "held" cannot tell a render from an abandoned one. `overdue` is NOT in this set: the
// lease block's `overdue` is the expiry-based field the routing change (P1) owns, and a second
// source for the same wire key is a merge hazard (Go keeps the shallower field and the
// embedded one goes dead), so this change publishes only what nothing else does.
func TestHealthPublishesOrphanedAndStalled(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   gpulease.Standing
		want map[string]bool
	}{
		{"orphaned", gpulease.Standing{Orphaned: true}, map[string]bool{"orphaned": true}},
		// A standing that says overdue puts nothing on the wire from here: `overdue` belongs to
		// the expiry-based lease field, which describes the same fact from the same record.
		{"overdue is not published from the standing", gpulease.Standing{Overdue: true}, map[string]bool{}},
		{"stalled", gpulease.Standing{Stalled: true}, map[string]bool{"stalled": true}},
		{"both", gpulease.Standing{Orphaned: true, Overdue: true, Stalled: true}, map[string]bool{"orphaned": true, "stalled": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			l := leaseBlock(t, standingServer(t, &st))
			for _, k := range []string{"orphaned", "overdue", "stalled"} {
				got, _ := l[k].(bool)
				if got != tc.want[k] {
					t.Errorf("lease.%s = %v want %v (block %v)", k, l[k], tc.want[k], l)
				}
			}
			if held, _ := l["held"].(bool); !held {
				t.Fatalf("the lease is still published as held: %v", l)
			}
		})
	}
}

// A healthy lease, and a node with no standing reader, put nothing new on the wire: an
// older delegator sees exactly what it saw before.
func TestHealthLeaseOfAHealthyLeaseCarriesNoStandingKeys(t *testing.T) {
	for name, s := range map[string]*Server{
		"healthy standing":   standingServer(t, &gpulease.Standing{}),
		"no standing reader": standingServer(t, nil),
	} {
		l := leaseBlock(t, s)
		for _, k := range []string{"orphaned", "overdue", "stalled"} {
			if _, present := l[k]; present {
				t.Errorf("%s: lease.%s must be absent: %v", name, k, l)
			}
		}
	}
}

// The standing reader is handed the lease the health block describes.
func TestHealthAsksTheStandingReaderAboutTheHeldLease(t *testing.T) {
	var asked gpulease.Info
	opts := &Options{
		NodeID: "testnode", Snapshot: goodSnapshot, Footprints: func() []FootprintEntry { return nil },
		Lease: func() gpulease.Info {
			return gpulease.Info{Held: true, Class: gpulease.ClassText, PID: 7, Epoch: 99, ExpiresAt: time.Now().Add(time.Hour)}
		},
		LeaseStanding: func(i gpulease.Info) gpulease.Standing { asked = i; return gpulease.Standing{} },
	}
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, opts)
	_ = leaseBlock(t, s)
	if asked.Epoch != 99 || asked.PID != 7 {
		t.Fatalf("the standing reader must see the held lease: %+v", asked)
	}
}
