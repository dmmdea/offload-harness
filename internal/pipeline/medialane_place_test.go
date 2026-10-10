package pipeline

// The place a queued image call keeps in this box's own line (core.PlaceKeeper, P0 plan S2): LeaveMediaPlace,
// KeepMediaPlace and DropMediaPlace wrap the lease manager's token calls and add no admission logic. A ticket
// (internal/mediaremote) waits for a lane anywhere in the cluster, and the place it keeps here is what makes it
// senior to every caller that arrives after it: a later arrival queues behind it, and the ticket itself, resuming its
// place, is served before anyone who came after.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// tokenFile is where the lease manager keeps a place in line.
func tokenFile(f *admitFixture, id string) string {
	return filepath.Join(f.root, "gpu", "tokens", id+".json")
}

// backdatePoll rewrites a place's last-poll time `ago` into the past, the way a place nobody has re-polled looks.
func backdatePoll(t *testing.T, f *admitFixture, id string, ago time.Duration) {
	t.Helper()
	b, err := os.ReadFile(tokenFile(f, id))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	rec["polled_ms"] = time.Now().Add(-ago).UnixMilli()
	if b, err = json.Marshal(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile(f, id), b, 0o666); err != nil {
		t.Fatal(err)
	}
}

func placeOf(t *testing.T, f *admitFixture, params map[string]any, since time.Time) core.MediaPlace {
	t.Helper()
	pl, err := f.p.LeaveMediaPlace(context.Background(), laneReq(params), since)
	if err != nil {
		t.Fatalf("LeaveMediaPlace: %v", err)
	}
	return pl
}

// A call on the pinned plan keeps its place on the card the pin names, and the place is a real token of the lease
// directory: the lease manager resumes it, with the arrival time the call asked to keep.
func TestLeaveMediaPlaceHoldsThePinnedCard(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	hold(t, f, "a bench render", A)
	arrived := time.Now().Add(-2 * time.Minute)
	pl := placeOf(t, f, nil, arrived)
	if !strings.HasPrefix(pl.Token, "tk-") {
		t.Fatalf("a place is a lease-directory token: %+v", pl)
	}
	if len(pl.Devices) != 1 || pl.Devices[0] != A {
		t.Errorf("the place is on the pinned card %s: %+v", A, pl.Devices)
	}
	if pl.Since.UnixMilli() != arrived.UnixMilli() {
		t.Errorf("the place keeps the arrival it was given: %v want %v", pl.Since, arrived)
	}
	tok, ok := f.m.ResumeToken(pl.Token)
	if !ok || tok.SinceMs != arrived.UnixMilli() || len(tok.Devices) != 1 || tok.Devices[0] != A || tok.Class != gpulease.ClassMedia {
		t.Errorf("the lease manager must hold that place: %+v ok=%v", tok, ok)
	}
}

// On the allocated plan the place is on a card the allocator would queue the call on: with every usable card held it is
// one of them, never the whole node (a whole-node place is a barrier for every card) and never nothing.
func TestLeaveMediaPlaceOnTheAutoPlanHoldsACardOfTheQueue(t *testing.T) {
	A, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	hold(t, f, "r1", A)
	hold(t, f, "r2", C)
	pl := placeOf(t, f, nil, time.Time{})
	if len(pl.Devices) != 1 || (pl.Devices[0] != A && pl.Devices[0] != C) {
		t.Fatalf("a card the allocator may queue the call on (A or C; the display card is the operator's): %+v", pl.Devices)
	}
	if time.Since(pl.Since) > time.Minute || time.Since(pl.Since) < 0 {
		t.Errorf("no arrival given: now, got %v", pl.Since)
	}
}

// A caller that already holds a place in line brings it along: the same token, the arrival it left with, whatever
// arrival the ticket itself would have given.
func TestLeaveMediaPlaceKeepsAResumedPlace(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	old := time.Now().Add(-7 * time.Minute)
	tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{A}}, old)
	if err != nil {
		t.Fatal(err)
	}
	pl := placeOf(t, f, map[string]any{"waiter_token": tok.ID}, time.Now())
	if pl.Token != tok.ID || pl.Since.UnixMilli() != old.UnixMilli() {
		t.Errorf("the place that was held is the place that is kept: %+v want token %s since %v", pl, tok.ID, old)
	}
	// A token that names nothing is a new arrival, never an error.
	fresh := placeOf(t, f, map[string]any{"waiter_token": "tk-doesnotexist1"}, time.Time{})
	if fresh.Token == "" || fresh.Token == "tk-doesnotexist1" {
		t.Errorf("an unknown token is a new arrival: %+v", fresh)
	}
}

// A box that cannot keep a place says so with the one typed error, and the call is never an error for it.
func TestLeaveMediaPlaceSaysWhenTheBoxKeepsNoPlace(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	t.Run("a host that does not lease cards", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, noAudit: true})
		if _, err := f.p.LeaveMediaPlace(context.Background(), laneReq(nil), time.Time{}); !errors.Is(err, core.ErrNoMediaPlace) {
			t.Errorf("err = %v, want ErrNoMediaPlace", err)
		}
	})
	t.Run("a call under an inherited lease", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
		l := hold(t, f, "the parent's reservation", A)
		for _, kv := range f.p.mediaGrantEnv(l, []string{A}, "") {
			k, v, _ := strings.Cut(kv, "=")
			t.Setenv(k, v)
		}
		if _, err := f.p.LeaveMediaPlace(context.Background(), laneReq(nil), time.Time{}); !errors.Is(err, core.ErrNoMediaPlace) {
			t.Errorf("err = %v, want ErrNoMediaPlace", err)
		}
	})
	t.Run("a task that is not an image", func(t *testing.T) {
		f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
		req := core.Request{Task: core.TaskGenerateVideo, Input: "a pan", Params: map[string]any{}}
		if _, err := f.p.LeaveMediaPlace(context.Background(), req, time.Time{}); !errors.Is(err, core.ErrNoMediaPlace) {
			t.Errorf("err = %v, want ErrNoMediaPlace", err)
		}
	})
}

// Keeping a place refreshes it and never writes one: a place nobody re-polled for longer than the grace is absent to
// every waiter until it is kept again, and a place that was dropped stays gone.
func TestKeepMediaPlaceRefreshesAndNeverWrites(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	pl := placeOf(t, f, nil, time.Time{})
	backdatePoll(t, f, pl.Token, 2*gpulease.TokenGrace)
	tok, _ := f.m.ResumeToken(pl.Token)
	if f.m.TokenLive(tok) {
		t.Fatal("control: a place nobody polled for twice the grace is absent")
	}
	if !f.p.KeepMediaPlace(pl.Token) {
		t.Fatal("keeping a place that exists must succeed")
	}
	if tok, _ = f.m.ResumeToken(pl.Token); !f.m.TokenLive(tok) || tok.SinceMs != pl.Since.UnixMilli() || tok.Devices[0] != A {
		t.Errorf("kept: live again, same arrival, same card: %+v", tok)
	}
	f.p.DropMediaPlace(pl.Token)
	if _, err := os.Stat(tokenFile(f, pl.Token)); !os.IsNotExist(err) {
		t.Fatalf("a dropped place is gone: %v", err)
	}
	if f.p.KeepMediaPlace(pl.Token) {
		t.Error("a place that was dropped cannot be kept")
	}
	if _, err := os.Stat(tokenFile(f, pl.Token)); !os.IsNotExist(err) {
		t.Error("keeping must never write a place back")
	}
	f.p.DropMediaPlace("tk-neverexisted1") // harmless
	f.p.DropMediaPlace("not a token")      // harmless
	if f.p.KeepMediaPlace("not a token") {
		t.Error("a malformed token keeps nothing")
	}
}

// THE SENIORITY RULE (plan: TestKeptTokenHoldsSeniorityAgainstLaterWaiters). A kept place holds the card against every
// caller that arrives after it, a caller that resumes the place is served before them, a place nobody keeps stops
// holding after the grace, and of places on one card the older one is served first and the younger waits for it: on the
// plan that names its card and on the plan that allocates one. (On the allocated plan the places spread over the cards
// nobody is queued for, as the real admission spreads its callers, so the third place of a box with two usable cards is
// the first to share a card.)
func TestKeptTokenHoldsSeniorityAgainstLaterWaiters(t *testing.T) {
	A, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)
	for _, tc := range []struct {
		name   string
		spec   admitSpec
		places int // how many places are left before the one under test
		setup  func(t *testing.T, f *admitFixture) []*gpulease.Lease
	}{
		{"the pinned plan", admitSpec{order: admitOrder, mutate: pinA}, 1, func(t *testing.T, f *admitFixture) []*gpulease.Lease {
			return []*gpulease.Lease{hold(t, f, "a bench render", A)}
		}},
		// Both usable cards are held when the places are left, so they queue; only card A is let go before the order is
		// asked, because a card nobody is queued for would simply be taken (the allocator spreads callers over such
		// cards, as the real admission does).
		{"the allocated plan, the other usable card busy", admitSpec{order: admitOrder}, 2, func(t *testing.T, f *admitFixture) []*gpulease.Lease {
			hold(t, f, "r2", C)
			return []*gpulease.Lease{hold(t, f, "r1", A)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, tc.spec)
			leases := tc.setup(t, f)
			// The older places, left a minute ago and kept; the youngest, left now and kept. The lease on the card they
			// queue on is released before the order is asked: the question is who is served first, not whether the card
			// is held.
			var older []core.MediaPlace
			for i := 0; i < tc.places; i++ {
				older = append(older, placeOf(t, f, nil, time.Now().Add(-time.Minute+time.Duration(i)*time.Second)))
			}
			youngest := placeOf(t, f, nil, time.Now())
			for _, l := range leases {
				if err := l.Release(); err != nil {
					t.Fatal(err)
				}
			}

			if v := f.p.MediaLaneFree(context.Background(), laneReq(map[string]any{"waiter_token": older[0].Token})); !v.Free {
				t.Errorf("the OLDEST place is served first, the places behind it do not hold the card back: %s", v.Why)
			}
			v := f.p.MediaLaneFree(context.Background(), laneReq(map[string]any{"waiter_token": youngest.Token}))
			if v.Free || v.Ahead < 1 {
				t.Errorf("the YOUNGEST place waits for the places before it on its card: free=%v ahead=%d (%s)", v.Free, v.Ahead, v.Why)
			}
			// A caller that arrives after all of them is behind them all.
			late := f.p.MediaLaneFree(context.Background(), laneReq(nil))
			if late.Free || late.Ahead < 1 {
				t.Errorf("a new arrival queues behind the kept places: free=%v ahead=%d (%s)", late.Free, late.Ahead, late.Why)
			}
			// Nobody keeps the places: after the grace they stop holding anyone back.
			for _, pl := range append(append([]core.MediaPlace(nil), older...), youngest) {
				backdatePoll(t, f, pl.Token, 2*gpulease.TokenGrace)
			}
			if v := f.p.MediaLaneFree(context.Background(), laneReq(nil)); !v.Free {
				t.Errorf("a place nobody keeps holds nothing: %s", v.Why)
			}
			// Kept again, they hold again.
			for _, pl := range append(append([]core.MediaPlace(nil), older...), youngest) {
				f.p.KeepMediaPlace(pl.Token)
			}
			if v := f.p.MediaLaneFree(context.Background(), laneReq(nil)); v.Free {
				t.Error("kept places hold the card again")
			}
		})
	}
}

// A call that resumes a place is ordered by ARRIVAL on every plan: it is served before a caller that joined the line after
// its place, so such a caller is behind it and nothing is in its way; a caller that joined BEFORE its place is ahead. (The
// first cut ordered by arrival on the whole-node plan only, because the only calls that resumed a place on the other plans
// were asking whether a wait-0 try would grant. A ticket asks whether it would be served before anyone who came after
// it, and with two tickets on one idle card each read the other's place as in its way and neither ever started.)
func TestMediaLaneFreeOrdersByArrivalForACallThatResumesItsPlace(t *testing.T) {
	A, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)
	for _, tc := range []struct {
		name    string
		spec    admitSpec
		devices []string   // the place of the call, and of the caller that arrived after it
		older   [][]string // the places that arrived before it: on the allocated plan one per usable card, or the call has a card to go to
	}{
		{"a pin names its cards", admitSpec{order: admitOrder, mutate: pinA}, []string{A}, [][]string{{A}}},
		{"the allocated plan", admitSpec{order: admitOrder}, []string{A}, [][]string{{A}, {C}}},
		{"the whole node", admitSpec{order: admitOrder, mutate: pinSeveral}, nil, [][]string{nil}},
	} {
		t.Run(tc.name+": a caller behind the place", func(t *testing.T) {
			f := newAdmitFixtureWith(t, tc.spec)
			seatWaiterAt(t, f.root, "transcribe a.wav", tc.devices...)
			tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: tc.devices}, time.Now().Add(-5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			v := f.p.MediaLaneFree(context.Background(), laneReq(map[string]any{"waiter_token": tok.ID}))
			if !v.Free || v.Ahead != 0 {
				t.Errorf("a caller behind the place is not in its way: free=%v ahead=%d (%s)", v.Free, v.Ahead, v.Why)
			}
		})
		t.Run(tc.name+": a caller ahead of the place", func(t *testing.T) {
			f := newAdmitFixtureWith(t, tc.spec)
			for _, devs := range tc.older {
				if _, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: devs}, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: tc.devices}, time.Now().Add(-5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			v := f.p.MediaLaneFree(context.Background(), laneReq(map[string]any{"waiter_token": tok.ID}))
			if v.Free || v.Ahead < 1 {
				t.Errorf("an older place is ahead: free=%v ahead=%d (%s)", v.Free, v.Ahead, v.Why)
			}
		})
	}
}
