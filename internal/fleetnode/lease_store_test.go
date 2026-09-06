package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/storesteward"
)

func leasedOpts(info gpulease.Info) *Options {
	return &Options{
		NodeID: "leased-node", Snapshot: goodSnapshot,
		Footprints: func() []FootprintEntry { return nil },
		GpuVendor:  "nvidia", GpuArch: "ampere",
		Lease: func() gpulease.Info { return info },
	}
}

func textLease() gpulease.Info {
	return gpulease.Info{Held: true, Class: gpulease.ClassText, PID: 4242, Reason: "arm B window",
		ExpiresAt: time.Date(2026, 9, 6, 17, 0, 0, 0, time.UTC)}
}

// TestHealthAdvertisesAHeldLease: a held lease is published under "lease" with
// class, pid, reason and expiry; an unreserved card publishes no key at all
// (the pre-0.113.16 shape, byte for byte).
func TestHealthAdvertisesAHeldLease(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, leasedOpts(textLease()))
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Lease *LeaseHealth `json:"lease"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Lease == nil || !got.Lease.Held || got.Lease.Class != "text" || got.Lease.PID != 4242 ||
		got.Lease.Reason != "arm B window" || got.Lease.Until != "2026-09-06T17:00:00Z" {
		t.Fatalf("lease not advertised as held: %s", rec.Body.String())
	}

	s2, _ := newTestServer(t, imageCfg(), &fakeRunner{}, leasedOpts(gpulease.Info{}))
	rec = do(t, s2, http.MethodGet, "/fleet/health", "", nil)
	if strings.Contains(rec.Body.String(), `"lease"`) {
		t.Fatalf("an unreserved card must publish no lease key: %s", rec.Body.String())
	}
}

// TestDispatchRefusedUnderATextLease: new work on a text-leased node is refused
// 503 naming the holder — the re-placeable status a delegator moves on from —
// while a media lease (a render arbitrated on the node itself) admits as before.
// The control arm proves the refusal is the lease's: the same dispatch on the
// same server with the lease gone is admitted 202.
func TestDispatchRefusedUnderATextLease(t *testing.T) {
	info := textLease()
	held := true
	opts := leasedOpts(info)
	opts.Lease = func() gpulease.Info {
		if held {
			return info
		}
		return gpulease.Info{}
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := dispatchImage(t, s, "j-leased")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d under a text lease, want 503: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"node leased", "class=text", "pid=4242", "reason=", "arm B window", "2026-09-06T17:00:00Z"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("refusal must name the holder (%q missing): %s", want, rec.Body.String())
		}
	}
	// Control arm: release the lease, same dispatch → admitted.
	held = false
	rec = dispatchImage(t, s, "j-after-release")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d after release, want 202: %s", rec.Code, rec.Body.String())
	}
	// Media lease: never a placement refusal.
	media := info
	media.Class = gpulease.ClassMedia
	s2, _ := newTestServer(t, imageCfg(), &fakeRunner{}, leasedOpts(media))
	if rec := dispatchImage(t, s2, "j-media"); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d under a media lease, want 202: %s", rec.Code, rec.Body.String())
	}
}

// TestHealthPublishesTheStoreStewardStatus: with a steward wired, health carries
// its last status under "store"; without one there is no key.
func TestHealthPublishesTheStoreStewardStatus(t *testing.T) {
	opts := leasedOpts(gpulease.Info{})
	opts.Store = func() storesteward.Status {
		return storesteward.Status{Root: "/srv/kv", UsedGB: 61.2, CapGB: 100, HighGB: 95, LowGB: 85, Files: 1203, Prunes: 2}
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	var got struct {
		Store *storesteward.Status `json:"store"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Store == nil || got.Store.Root != "/srv/kv" || got.Store.Files != 1203 || got.Store.Prunes != 2 || got.Store.CapGB != 100 {
		t.Fatalf("store status not published: %s", rec.Body.String())
	}
	s2, _ := newTestServer(t, imageCfg(), &fakeRunner{}, leasedOpts(gpulease.Info{}))
	if rec := do(t, s2, http.MethodGet, "/fleet/health", "", nil); strings.Contains(rec.Body.String(), `"store"`) {
		t.Fatalf("no steward must mean no store key: %s", rec.Body.String())
	}
}

// TestJobsOnFinishFiresOutsideTheLock: the steward's turn counter is called on
// every terminal job and may read the store back without deadlocking.
func TestJobsOnFinishFiresOutsideTheLock(t *testing.T) {
	j := NewJobs(time.Hour, 4)
	t.Cleanup(func() { j.DrainAndStop(time.Second) })
	fired := make(chan int, 4)
	j.OnFinish(func() { q, r := j.Counts(); fired <- q + r })
	j.Accept("a", func(ctx context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("OnFinish never fired for a finished job (or deadlocked reading the store)")
	}
}
