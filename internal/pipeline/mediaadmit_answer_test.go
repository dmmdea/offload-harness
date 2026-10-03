package pipeline

// What a queued media answer says about the line it left the caller in. The token is the part a
// client acts on, but the holder list and the ETA are what an operator reads to decide whether to
// wait, so they must be about the right leases: those in the way of THIS request.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

type queuedPayload struct {
	Token    string   `json:"waiter_token"`
	Position int      `json:"queue_position"`
	ETASec   int      `json:"eta_s"`
	Devices  []string `json:"devices"`
	HeldBy   []uint64 `json:"held_by"`
}

func queuedData(t *testing.T, res core.Result) queuedPayload {
	t.Helper()
	var p queuedPayload
	if err := json.Unmarshal(res.Data, &p); err != nil {
		t.Fatalf("queued data %s: %v", res.Data, err)
	}
	return p
}

// A call that needs the whole node is in the way of, and blocked by, EVERY lease. The answer used
// to skip every card lease for it (an empty card set intersects nothing), so it reported no
// holder, an ETA of zero seconds and "card(s)  are promised to callers ahead of this one" while a
// lease with a declared hour was the only reason it was queued.
func TestAWholeNodeQueuedAnswerNamesTheLeaseInTheWay(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDA)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	res := f.await(f.graph(nil))
	if res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("want a queued answer, got %q: %s", res.Meta.ErrClass, res.Reason)
	}
	p := queuedData(t, res)
	if len(p.HeldBy) != 1 || p.HeldBy[0] != holder.Epoch() {
		t.Errorf("held_by = %v, want the one lease in the way (epoch %d)", p.HeldBy, holder.Epoch())
	}
	if p.ETASec < 3500 || p.ETASec > 3600 {
		t.Errorf("eta_s = %d, want about the hour the holder declared", p.ETASec)
	}
	if strings.Contains(res.Reason, "card(s)  ") {
		t.Errorf("the reason names a card set that is empty: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "whole node") {
		t.Errorf("the reason should say what was asked for, the whole node: %q", res.Reason)
	}
	if strings.Contains(res.Reason, "at most 0s") {
		t.Errorf("the reason promises a zero-second wait behind an hour-long lease: %q", res.Reason)
	}
}

// A single-card call is blocked only by the lease on ITS card: a lease on another card is not in
// its way and is not named as if it were.
func TestASingleCardQueuedAnswerNamesOnlyTheLeaseOnItsCard(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	onC, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "its card", TTL: 2 * time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = onC.Release() }()
	onA, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another card", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDA)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = onA.Release() }()

	res := f.await(f.image(nil))
	if res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("want a queued answer, got %q: %s", res.Meta.ErrClass, res.Reason)
	}
	p := queuedData(t, res)
	if len(p.HeldBy) != 1 || p.HeldBy[0] != onC.Epoch() {
		t.Errorf("held_by = %v, want only the lease on the call's own card (epoch %d)", p.HeldBy, onC.Epoch())
	}
	if p.ETASec < 7100 || p.ETASec > 7200 {
		t.Errorf("eta_s = %d, want the two hours of the lease on its card, not the hour of the other", p.ETASec)
	}
}

// "At most 0s" was printed when nothing ahead declares an end, which reads as "no wait". Zero is
// "no estimate"; a positive figure is the longest declared end among the leases in the way.
func TestAQueuedAnswerNeverPromisesAZeroSecondWait(t *testing.T) {
	none := (&errGPUQueued{Token: "tk-aaaaaaaa", Position: 2, Why: "the whole node promised to callers ahead of this one"}).Error()
	if strings.Contains(none, "at most") || !strings.Contains(none, "no estimate") {
		t.Errorf("an answer with no declared end must say it has no estimate: %q", none)
	}
	some := (&errGPUQueued{Token: "tk-aaaaaaaa", Position: 1, ETASec: 120, Why: "card(s) x in use"}).Error()
	if !strings.Contains(some, "at most 120s until the lease(s) in the way end") {
		t.Errorf("a declared end is the ceiling the answer states: %q", some)
	}
}
