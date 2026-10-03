package fleetnode

// An expired lease on the wire (plan P9). A lease whose term ended unrenewed is still HELD
// (its holder heartbeats, its claim stays), so the node keeps publishing it busy and
// overdue exactly as P1 does, and additionally says it expired. Nothing about it frees the
// node or refuses work; the key is a fact for the delegator and the operator.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// expiredLeaseDir writes the lease directory a wrapper leaves behind once its term ended
// unrenewed: a whole-node record labelled expired, a live holder (this process) and a
// heartbeat from just now.
func expiredLeaseDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	rec := `{"epoch": 9, "class": "media", "reason": "film", "holder": {"pid": ` + strconv.Itoa(os.Getpid()) + `, "start_time_ms": 0},` +
		` "acquired_at_ms": ` + strconv.FormatInt(now.Add(-9*time.Hour).UnixMilli(), 10) + `,` +
		` "expires_at_ms": ` + strconv.FormatInt(now.Add(-3*time.Hour).UnixMilli(), 10) + `,` +
		` "renewed_at_ms": ` + strconv.FormatInt(now.UnixMilli(), 10) + `,` +
		` "term_ms": 21600000, "max_total_ms": 172800000, "expired": true, "expired_why": "its owner is gone"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// THE REAL READER, not a hand-built Info: an expired, heartbeating lease is held, busy, overdue
// and expired. The Busy rule (remaining > threshold OR overdue) must not let the label turn a
// card that is demonstrably in use into a free one.
func TestFleetBusyStaysTrueWhenOverdue(t *testing.T) {
	dir := expiredLeaseDir(t)
	info := gpulease.InspectDir(dir)
	if !info.Held || !info.Expired {
		t.Fatalf("the reader must keep an expired, heartbeating lease held and labelled: %+v", info)
	}
	h := leaseHealthOf(info, time.Now(), 0)
	if !h.Busy || !h.Overdue || !h.Expired {
		t.Fatalf("lease health = %+v, want busy, overdue and expired", h)
	}
	// With the busy rule switched off (fleet_busy_lease_sec < 0) the label is still a fact.
	if off := leaseHealthOf(info, time.Now(), -1); off.Busy || !off.Expired || !off.Overdue {
		t.Fatalf("rule off: %+v", off)
	}
	// Not expired, not published: the key is absent on a lease inside its term.
	inside := leaseHealthOf(gpulease.Info{Held: true, Class: gpulease.ClassMedia, ExpiresAt: time.Now().Add(time.Hour)}, time.Now(), 0)
	if inside.Expired {
		t.Fatalf("a lease inside its term is not expired: %+v", inside)
	}
}

// With several leases live the node's lease block is the lowest epoch's, but an expired
// sibling must not hide behind a healthy lower epoch.
func TestExpiredSiblingLeaseIsPublished(t *testing.T) {
	now := time.Now()
	healthy := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 3, ExpiresAt: now.Add(2 * time.Hour)}
	expired := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 4, ExpiresAt: now.Add(-time.Hour), Expired: true}
	primary := healthy
	primary.Leases = []gpulease.Info{healthy, expired}
	h := leaseHealthOf(primary, now, 0)
	if !h.Expired {
		t.Fatalf("an expired lease behind a healthy lower epoch must still be published: %+v", h)
	}
}

// Through the real handler, as JSON: the key is there when true and absent when not, so a
// node one release behind and every older delegator see exactly the bytes they always did.
func TestLeaseBlockPublishesExpiredOnlyWhenExpired(t *testing.T) {
	dir := expiredLeaseDir(t)
	read := func(info func() gpulease.Info) map[string]any {
		s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, &Options{
			NodeID: "testnode", Snapshot: goodSnapshot, Footprints: func() []FootprintEntry { return nil },
			Lease: info,
		})
		rec := do(t, s, "GET", "/fleet/health", "", nil)
		var raw struct {
			Lease map[string]any `json:"lease"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		return raw.Lease
	}
	lease := read(func() gpulease.Info { return gpulease.InspectDir(dir) })
	if lease["held"] != true || lease["busy"] != true || lease["overdue"] != true || lease["expired"] != true {
		t.Fatalf("lease block = %v, want held, busy, overdue and expired", lease)
	}
	healthy := read(func() gpulease.Info {
		return gpulease.Info{Held: true, Class: gpulease.ClassMedia, PID: 42, ExpiresAt: time.Now().Add(6 * time.Hour)}
	})
	if _, present := healthy["expired"]; present {
		t.Fatalf("expired key present on a lease inside its term: %v", healthy)
	}
}
