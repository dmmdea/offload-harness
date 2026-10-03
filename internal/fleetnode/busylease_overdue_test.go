package fleetnode

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A short lease (the render arbitrated on the node itself) must never make the
// node a non-target. The 120 s threshold is the rule an overdue verdict has to
// leave intact: P1 adds a second way to be busy, it does not move this one.
func TestShortLeaseBelowThresholdNotBusy(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		remaining time.Duration
	}{
		{"twenty seconds left", 20 * time.Second},
		{"one second under the threshold", 119 * time.Second},
		{"exactly the threshold", 120 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := leaseHealthOf(gpulease.Info{Held: true, Class: gpulease.ClassMedia, ExpiresAt: now.Add(tc.remaining)}, now, 0)
			if h.Busy {
				t.Fatalf("a lease with %s left read as busy; the short-lease rule is broken", tc.remaining)
			}
			if h.Overdue {
				t.Fatalf("a lease with %s left read as overdue", tc.remaining)
			}
		})
	}
}

// The clamp to zero used to make a lease that outlived its declared window read
// as FREE: remaining 0 is below every threshold, so Busy came out false for the
// one lease that is demonstrably still holding the cards (its holder keeps the
// heartbeat). Held and past ExpiresAt is busy, and says why.
func TestOverdueHeldLeaseIsBusyAndSaysSo(t *testing.T) {
	now := time.Now()
	h := leaseHealthOf(gpulease.Info{Held: true, Class: gpulease.ClassMedia, ExpiresAt: now.Add(-3 * time.Hour)}, now, 0)
	if !h.Busy {
		t.Fatal("a held lease past its declared window must read busy: the clamp to zero made it look free")
	}
	if !h.Overdue {
		t.Fatal("the overdue fact must travel on its own: busy alone cannot tell a delegator the window lapsed")
	}
	if h.RemainingSec != 0 {
		t.Fatalf("remaining_sec = %d, want 0 for an overdue lease", h.RemainingSec)
	}
	// The window ending THIS instant is not yet overdue.
	edge := leaseHealthOf(gpulease.Info{Held: true, Class: gpulease.ClassMedia, ExpiresAt: now}, now, 0)
	if edge.Overdue || edge.Busy {
		t.Fatalf("a lease ending exactly now = %+v, want neither overdue nor busy", edge)
	}
	// A lease with no declared end has nothing to be overdue against.
	open := leaseHealthOf(gpulease.Info{Held: true, Class: gpulease.ClassMedia}, now, 0)
	if open.Overdue || open.Busy {
		t.Fatalf("a lease with no declared end = %+v, want neither overdue nor busy", open)
	}
	// An Info built from a raw record carries the Unix epoch, not the zero Time
	// (IsZero is false for it): still no declared end.
	epoch := leaseHealthOf(gpulease.Info{Held: true, Class: gpulease.ClassMedia, ExpiresAt: time.UnixMilli(0)}, now, 0)
	if epoch.Overdue || epoch.Busy {
		t.Fatalf("a lease ending at the Unix epoch = %+v, want neither overdue nor busy: that is an unset end, not a window that lapsed in 1970", epoch)
	}
}

// fleet_busy_lease_sec < 0 turns the duration rule off ("text-only refusal, the
// pre-0.113.27 behaviour"). Overdue-busy is part of that rule, so an operator
// who switched it off does not get it back; the overdue FACT still travels,
// because it is information, not a verdict.
func TestOverdueDoesNotOverrideADisabledBusyRule(t *testing.T) {
	now := time.Now()
	h := leaseHealthOf(gpulease.Info{Held: true, Class: gpulease.ClassMedia, ExpiresAt: now.Add(-time.Hour)}, now, -1)
	if h.Busy {
		t.Fatal("a negative fleet_busy_lease_sec must disable the busy verdict for an overdue lease too")
	}
	if !h.Overdue {
		t.Fatal("overdue is a fact about the lease and must still be published with the rule off")
	}
}

// The same facts through the REAL handler, as JSON: an overdue exclusive text
// lease keeps lease.busy true, says overdue, drops remaining_sec (omitempty, so
// it is absent rather than 0), and the node stops advertising an idle slot.
func TestOverdueHeldExclusiveLeaseKeepsFleetBusyTrue(t *testing.T) {
	cfg := config.Config{}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, &Options{
		NodeID:     "testnode",
		Snapshot:   goodSnapshot,
		Footprints: func() []FootprintEntry { return nil },
		Lease: func() gpulease.Info {
			return gpulease.Info{Held: true, Class: gpulease.ClassText, PID: 42, Exclusive: true, ExpiresAt: time.Now().Add(-90 * time.Minute)}
		},
	})
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health %d", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var lease map[string]any
	if err := json.Unmarshal(raw["lease"], &lease); err != nil {
		t.Fatalf("lease block: %v (%s)", err, raw["lease"])
	}
	if lease["held"] != true || lease["busy"] != true || lease["overdue"] != true {
		t.Fatalf("lease block = %v, want held, busy and overdue all true", lease)
	}
	if _, present := lease["remaining_sec"]; present {
		t.Fatalf("remaining_sec present on an overdue lease: %v (omitempty drops the zero)", lease)
	}
	var exclusive bool
	if err := json.Unmarshal(raw["lease_exclusive"], &exclusive); err != nil || !exclusive {
		t.Fatalf("lease_exclusive = %s, want true", raw["lease_exclusive"])
	}
	var sat struct {
		High     bool `json:"high"`
		IdleSlot bool `json:"idle_slot"`
	}
	if err := json.Unmarshal(raw["saturation"], &sat); err != nil {
		t.Fatal(err)
	}
	if !sat.High || sat.IdleSlot {
		t.Fatalf("saturation = %+v: an overdue held lease still has the cards, so the node must not advertise an idle slot", sat)
	}
}

// A lease that is NOT overdue publishes the same bytes it always did: the new
// key is omitempty, so an unchanged lease and every older delegator see no new
// field at all.
func TestLeaseBlockOmitsOverdueWhenNotOverdue(t *testing.T) {
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, &Options{
		NodeID:     "testnode",
		Snapshot:   goodSnapshot,
		Footprints: func() []FootprintEntry { return nil },
		Lease: func() gpulease.Info {
			return gpulease.Info{Held: true, Class: gpulease.ClassMedia, PID: 42, ExpiresAt: time.Now().Add(6 * time.Hour)}
		},
	})
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	var raw struct {
		Lease map[string]any `json:"lease"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Lease["busy"] != true {
		t.Fatalf("lease = %v, want the long lease busy as before", raw.Lease)
	}
	if _, present := raw.Lease["overdue"]; present {
		t.Fatalf("overdue key present on a lease inside its window: %v", raw.Lease)
	}
}

// An overdue lease is a fact about the lease, not a refusal. A media lease does
// not make the node turn dispatches away (the node queues what arrives), so the
// node must keep advertising that it would accept: saturation.high false and an
// idle slot, exactly as before the overdue verdict existed. Folding the overdue
// busy into the node's refusing flag made the delegator's capacity wait skip the
// node (hasRoom is false for a high saturation) while a first deal still placed
// on it, so a run that was refused once for another reason deferred on capacity
// instead of asking the one remote that would have taken it.
func TestOverdueMediaLeaseDoesNotMakeTheNodeAdvertiseRefusal(t *testing.T) {
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, &Options{
		NodeID:     "testnode",
		Snapshot:   goodSnapshot,
		Footprints: func() []FootprintEntry { return nil },
		Lease: func() gpulease.Info {
			return gpulease.Info{Held: true, Class: gpulease.ClassMedia, PID: 42, ExpiresAt: time.Now().Add(-90 * time.Minute)}
		},
	})
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	var raw struct {
		Lease      map[string]any `json:"lease"`
		Saturation struct {
			High     bool `json:"high"`
			IdleSlot bool `json:"idle_slot"`
		} `json:"saturation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Lease["busy"] != true || raw.Lease["overdue"] != true {
		t.Fatalf("lease = %v, want the verdict itself unchanged: busy and overdue", raw.Lease)
	}
	if raw.Saturation.High || !raw.Saturation.IdleSlot {
		t.Fatalf("saturation = %+v: an overdue media lease does not refuse dispatch, so the node must still advertise room", raw.Saturation)
	}
}

// A lease record whose declared end is missing, zero or negative has no window
// to be overdue against. Through the REAL reader (InspectDir over a scratch
// lease directory, not a hand-built Info) the end must come out as the zero
// time: time.UnixMilli(0) is 1970, not the zero Time, so a guard written
// against IsZero alone never fires for a real record and the lease reads busy
// and overdue for a record somebody hand-edited, truncated or wrote elsewhere.
func TestRealLeaseRecordWithNoDeclaredEndIsNotOverdue(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ name, expires string }{
		{"zero", `"expires_at_ms": 0,`},
		{"missing", ``},
		{"negative", `"expires_at_ms": -5,`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			rec := `{"epoch": 7, "class": "media", "holder": {"pid": ` + strconv.Itoa(os.Getpid()) + `, "start_time_ms": 0},` +
				` "acquired_at_ms": ` + strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10) + `,` + tc.expires +
				` "renewed_at_ms": ` + strconv.FormatInt(now.UnixMilli(), 10) + `}`
			if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(rec), 0o600); err != nil {
				t.Fatal(err)
			}
			info := gpulease.InspectDir(dir)
			if !info.Held {
				t.Fatalf("fixture is not a held lease: %+v", info)
			}
			h := leaseHealthOf(info, now, 0)
			if h.Overdue || h.Busy {
				t.Fatalf("a held record with no declared end read as overdue=%v busy=%v (until %q): there is no window to be past", h.Overdue, h.Busy, h.Until)
			}
		})
	}
}
