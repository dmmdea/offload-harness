package gpuactivity

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Plan P5 (register C-86): the run registry and the activity verdict know which cards a run and
// a lease sit on, so a drain waits only for work on the leased cards and a job that strays onto
// a card it did not claim is flagged.

func TestRunRecordsItsDevices(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	h, err := reg.Begin(Run{Seat: "seat-a", Kind: "contract", Devices: []string{"0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	got := reg.List(time.Now())
	if len(got) != 1 || len(got[0].Devices) != 1 || got[0].Devices[0] != "0" {
		t.Fatalf("a run carries the pins of its seat, got %+v", got)
	}
	// A record from a build that predates the field reads as "no pin known".
	if old := (Run{Seat: "x"}); len(old.Devices) != 0 {
		t.Fatalf("zero value has no devices, got %v", old.Devices)
	}
}

func TestWhereAndOnSeatPinnedCountRunsPerCard(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	start := func(r Run) {
		t.Helper()
		h, err := reg.Begin(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.End)
	}
	start(Run{Seat: "seat-a", Kind: "contract", Devices: []string{"0"}})
	start(Run{Seat: "seat-b", Kind: "contract", Devices: []string{"0"}}) // another seat on the same card
	start(Run{Seat: "seat-c", Kind: "contract", Devices: []string{"2"}})
	start(Run{Seat: "seat-d", Kind: "agent_run"}) // a record with no pin

	now := time.Now()
	onCard0 := reg.Where(now, func(r Run) bool { return r.OnPin("0") })
	if len(onCard0) != 2 {
		t.Fatalf("two runs sit on card 0, got %+v", onCard0)
	}
	// A single-card seat's line is the card's line: its own runs and every other run on that card.
	got := reg.OnSeatPinned(now, []string{"0"}, "seat-a")
	if len(got) != 2 {
		t.Fatalf("a single-card seat counts the runs on its card (seat-a and seat-b), got %+v", got)
	}
	// A seat that spans cards keeps the per-seat line.
	got = reg.OnSeatPinned(now, []string{"0", "2"}, "seat-a")
	if len(got) != 1 || got[0].Seat != "seat-a" {
		t.Fatalf("a multi-card seat counts its own runs, got %+v", got)
	}
	// An unknown pin is the per-seat line too.
	got = reg.OnSeatPinned(now, nil, "seat-d")
	if len(got) != 1 || got[0].Seat != "seat-d" {
		t.Fatalf("an unknown pin counts by seat name, got %+v", got)
	}
}

const (
	tCard0 = "GPU-aaaa0000-0000-0000-0000-000000000000"
	tCard1 = "GPU-bbbb0000-0000-0000-0000-000000000000"
	tCard2 = "GPU-cccc0000-0000-0000-0000-000000000000"
)

func trio(u0, u1, u2 int) []GPU {
	return []GPU{
		{Index: 0, UUID: tCard0, Name: "Card A", UtilPct: u0, UtilKnown: true, MemTotalMiB: 16000},
		{Index: 1, UUID: tCard1, Name: "Card B", UtilPct: u1, UtilKnown: true, MemTotalMiB: 16000, DisplayActive: true},
		{Index: 2, UUID: tCard2, Name: "Card A", UtilPct: u2, UtilKnown: true, MemTotalMiB: 16000},
	}
}

func scopedHolder(devs ...string) *Holder {
	return &Holder{PID: 100, Alive: true, Class: "media", AgeSec: 600, Reason: "render", Devices: devs}
}

func TestDeviceTrespassFlagged(t *testing.T) {
	now := time.Now()
	idle := SeatState{Name: "agent-pool"}

	// The lease holds card 2; card 0 is busy and nothing the harness knows explains it: the job
	// is using a card it did not claim.
	v := View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: idle, GPUs: trio(85, 0, 90)}
	verdict, note := Assess(v)
	if verdict != VerdictHeldWorking {
		t.Fatalf("the lease's own card is busy: held-working, got %q (%s)", verdict, note)
	}
	if !strings.Contains(note, "device-trespass") || !strings.Contains(note, "card 0") {
		t.Fatalf("card 0 is busy outside the lease's cards and must be flagged: %s", note)
	}
	v.Verdict, v.Note = verdict, note
	_ = v
	flagged := trespassOf(v)
	if len(flagged) != 1 || flagged[0].Index != 0 {
		t.Fatalf("trespass = %+v, want card 0 only", flagged)
	}
	if m := v.Map(); m["device_trespass"] == nil {
		t.Fatalf("the JSON view carries the flag: %v", m)
	}
	if m := (View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: idle, GPUs: trio(0, 0, 90)}).Map(); m["device_trespass"] != nil {
		t.Fatalf("no trespass, no key: %v", m)
	}

	for _, c := range []struct {
		name string
		view View
	}{
		{"the display card is the desktop's", View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: idle, GPUs: trio(0, 60, 90)}},
		{"another live lease holds that card", View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), OtherDevices: []string{strings.ToLower(tCard0)}, Seat: idle, GPUs: trio(85, 0, 90)}},
		{"a registered run on that card explains it", View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: idle, GPUs: trio(85, 0, 90),
			Runs: []Run{{Kind: "contract", Seat: "seat-a", Devices: []string{"0"}}}}},
		{"a quiet card outside the set", View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: idle, GPUs: trio(3, 0, 90)}},
		{"a whole-node lease has no outside", View{At: now, Held: true, Holder: &Holder{PID: 1, Alive: true, Class: "media"}, Seat: idle, GPUs: trio(85, 0, 90)}},
	} {
		if got := trespassOf(c.view); len(got) != 0 {
			t.Errorf("%s: must not be flagged, got %+v", c.name, got)
		}
		if _, note := Assess(c.view); strings.Contains(note, "device-trespass") {
			t.Errorf("%s: the note must not say device-trespass: %s", c.name, note)
		}
	}
}

// With a bounded lease, "the cards are busy under it" means the lease's cards: a card the
// lease does not hold, busy with a seat's run, is not the holder's work.
func TestHeldWorkingReadsTheLeasesOwnCardsWhenItNamesThem(t *testing.T) {
	now := time.Now()
	idle := SeatState{Name: "agent-pool"}
	v := View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: idle, GPUs: trio(85, 0, 1),
		Runs: nil, OtherDevices: []string{strings.ToLower(tCard0)}}
	verdict, note := Assess(v)
	if verdict != VerdictHeldIdle {
		t.Fatalf("the lease's card 2 is idle and card 0 belongs to another lease: held-idle, got %q (%s)", verdict, note)
	}
	// Unbounded (whole-node) lease: unchanged, any card the harness can use counts.
	w := View{At: now, Held: true, Holder: &Holder{PID: 1, Alive: true, Class: "media"}, Seat: idle, GPUs: trio(85, 0, 1)}
	if verdict, _ := Assess(w); verdict != VerdictHeldWorking {
		t.Fatalf("a whole-node lease reads as before, got %q", verdict)
	}
}

// The seat is busy: its card is not named in the view, so a busy card outside the lease cannot be
// attributed and is not flagged.
func TestNoTrespassIsFlaggedWhileTheSeatItselfIsBusy(t *testing.T) {
	v := View{At: time.Now(), Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), GPUs: trio(85, 0, 90),
		Seat: SeatState{Name: "agent-pool", Loaded: true, Inflight: 2, Source: "metrics"}}
	if got := trespassOf(v); len(got) != 0 {
		t.Fatalf("a busy seat explains a busy card the view cannot place, got %+v", got)
	}
	v.Seat = SeatState{Name: "agent-pool", Loaded: true, Starting: true}
	if got := trespassOf(v); len(got) != 0 {
		t.Fatalf("a seat that is loading explains it too, got %+v", got)
	}
}

// The snapshot hands Assess the cards of the lease it read, the cards of every OTHER live lease,
// and runs the caller's scope function over a legacy lease first.
func TestSnapshotCarriesTheLeasesCardsAndAppliesTheScope(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	a, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render a", Devices: []string{"gpu-cccc0000-x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render b", Devices: []string{"gpu-aaaa0000-x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()

	var scoped atomic.Int64
	v := Snapshot(context.Background(), Options{StateDir: root, Scope: func(i gpulease.Info) gpulease.Info { scoped.Add(1); return i }})
	if !v.Held || v.Holder == nil || len(v.Holder.Devices) != 1 || v.Holder.Devices[0] != "gpu-cccc0000-x" {
		t.Fatalf("the holder carries the cards of the lowest lease, got %+v", v.Holder)
	}
	if len(v.OtherDevices) != 1 || v.OtherDevices[0] != "gpu-aaaa0000-x" {
		t.Fatalf("the other lease's cards ride beside it, got %v", v.OtherDevices)
	}
	if scoped.Load() == 0 {
		t.Fatal("the scope function must be applied to the lease it read")
	}
	m2 := v.Map()
	if _, ok := m2["holder"]; !ok {
		t.Fatalf("holder missing from the map: %v", m2)
	}
}

// P4 makes text seats on the free cards legal, so a busy card outside the lease is usually a
// seat serving there, a cascade call on the router, the desktop: the registry knows only the
// runs of the two doors that register, and the view only the planner seat's gauge. The flag
// therefore says "possible" and names what it could not rule out; it never states that the
// holder strayed.
func TestTrespassIsOnlyPossibleAndNeverAccusesTheHolder(t *testing.T) {
	now := time.Now()
	// A router seat (not the planner seat the view tracks) is serving a cascade call on card 0.
	v := View{At: now, Held: true, Holder: scopedHolder(strings.ToLower(tCard2)), Seat: SeatState{Name: "agent-pool"}, GPUs: trio(85, 0, 90)}
	_, note := Assess(v)
	for _, want := range []string{"possible device-trespass", "card 0", "not attributed", "text seat"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note must say %q: %s", want, note)
		}
	}
	for _, forbidden := range []string{"the holder is using a card it did not claim", "using a card it did not claim"} {
		if strings.Contains(note, forbidden) {
			t.Errorf("the note must not accuse the holder (%q): %s", forbidden, note)
		}
	}
}
