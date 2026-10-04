package pairworkloads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// The H6 correctness review's findings (F1-F6), each pinned by the failure it described.

func setRelayStatus(f *fakeRelay, status int) {
	f.mu.Lock()
	f.status = status
	f.mu.Unlock()
}

// sweeperFor is a sweeper of the relays' register: every producer pid dead, no pid recycling.
func sweeperFor(t *testing.T, open string, relays ...*fakeRelay) *Emitter {
	t.Helper()
	var bases []string
	for _, r := range relays {
		bases = append(bases, r.srv.URL)
	}
	sw := New(Config{Enabled: true, Endpoint: "http://127.0.0.1:1/unused", AppDir: noPairAppDir(t), OpenDir: open,
		Relay: RelayConfig{Bases: bases, Token: "t"}})
	sw.alive = dead
	sw.procStart = func(int) (int64, bool) { return 0, false }
	return sw
}

// F1: a harness built before the relay sweeps the same register directory and reads every ".json"
// marker with pid <= 0 as orphaned, posting "failed" for a job still running. A relayed marker
// (in-flight and pending) is therefore not a ".json" file, which those binaries skip whole.
func TestRelayedMarkerFileIsInvisibleToAnOlderSweeper(t *testing.T) {
	m := newRelayMember(t)
	m.relayOne(t, relayFrame(t, "workload:submitted", queuedInfo(), map[string]any{"node": "node-v"}), "node-q")

	// oldSweepSees is the pre-relay sweep's file filter and orphan rule, verbatim.
	oldSweepSees := func(dir string) (orphaned int) {
		ents, _ := os.ReadDir(dir)
		for _, de := range ents {
			name := de.Name()
			if de.IsDir() || strings.Contains(name, tmpInfix) || strings.HasSuffix(name, lockSuffix) || !strings.HasSuffix(name, ".json") {
				continue
			}
			raw, _ := os.ReadFile(filepath.Join(dir, name))
			var mk openMarker
			if json.Unmarshal(raw, &mk) == nil && len(mk.Info) > 0 && !mk.Pending && mk.PID <= 0 {
				orphaned++
			}
		}
		return orphaned
	}
	if n := oldSweepSees(m.open); n != 0 {
		ents, _ := os.ReadDir(m.open)
		t.Fatalf("an older sweeper would close %d running relayed card(s) as failed: %v", n, ents)
	}
	ents, _ := os.ReadDir(m.open)
	if len(ents) != 1 || strings.HasSuffix(ents[0].Name(), ".json") || !strings.HasSuffix(ents[0].Name(), remoteSuffix) {
		t.Fatalf("relayed marker = %v, want one file ending %q and not .json", ents, remoteSuffix)
	}

	// The pending form (a terminal frame the member's PAIR could not take) keeps the same name.
	var up atomic.Bool
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up.Load() {
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	broken := New(Config{Enabled: true, Endpoint: down.URL, AppDir: m.appDr, OpenDir: m.open})
	done := queuedInfo()
	done["state"], done["completedAt"] = "completed", 2000
	ev, _ := ParseRelay(relayFrame(t, "workload:completed", done, nil), "node-q")
	broken.Emit(ev)
	broken.Wait()
	ents, _ = os.ReadDir(m.open)
	if len(ents) != 1 || !strings.HasSuffix(ents[0].Name(), remoteSuffix) {
		t.Fatalf("pending relayed marker = %v, want one %q file", ents, remoteSuffix)
	}

	// This build's own sweep still reads it: once PAIR answers it delivers the kept frame, once.
	up.Store(true)
	sw := New(Config{Enabled: true, Endpoint: down.URL, AppDir: m.appDr, OpenDir: m.open})
	sw.alive = dead
	sw.procStart = func(int) (int64, bool) { return 0, false }
	if n := sw.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the sweep delivered %d pending relayed frame(s), want 1", n)
	}
	if ents, _ := os.ReadDir(m.open); len(ents) != 0 {
		t.Fatalf("register after the sweep = %v", ents)
	}
}

// F2: with several relays, one dead relay's marker ends neither the pass nor the other relays'
// cards: the sweep skips that relay's remaining markers and closes the healthy relay's.
func TestSweepSkipsADeadRelayAndClosesTheHealthyOnes(t *testing.T) {
	a, b := newFakeRelay(t), newFakeRelay(t)
	e, open := relayEmitter(t, a, b)
	for _, id := range []string{"led-a-1", "led-a-2"} {
		e.Emit(Event{JobID: id, Model: "m", Engine: "llamacpp", State: "running", CreatedAt: 1, StartedAt: 2})
	}
	e.Wait()
	e.demoteRelay(relayURL(a.srv.URL)) // the next job's first frame goes to b
	e.Emit(Event{JobID: "led-b-1", Model: "m", Engine: "llamacpp", State: "running", CreatedAt: 1, StartedAt: 2})
	e.Wait()
	if a.count() != 2 || b.count() != 1 {
		t.Fatalf("setup: a=%d b=%d frames, want 2 and 1", a.count(), b.count())
	}

	setRelayStatus(a, http.StatusServiceUnavailable)
	sw := sweeperFor(t, open, a, b)
	if n := sw.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the sweep closed %d card(s), want the one on the healthy relay", n)
	}
	if b.count() != 2 || b.info(1)["id"] != "led-b-1" || b.info(1)["state"] != "failed" {
		t.Fatalf("the healthy relay's card was not closed (b hits %d)", b.count())
	}
	if a.count() != 3 {
		t.Fatalf("the dead relay was asked %d times, want 1 attempt for the pass (2 setup frames + 1)", a.count()-2)
	}
	ents, _ := os.ReadDir(open)
	var names []string
	for _, de := range ents {
		names = append(names, de.Name())
	}
	if len(ents) != 2 || strings.Contains(strings.Join(names, ","), lockSuffix) {
		t.Fatalf("register after the pass = %v, want the two dead-relay markers and no lock", names)
	}
}

// F3: a job's relay pin is only as good as the first post that relay took. A first frame that FAILED
// on the relay must not keep the job pinned there while another relay is healthy.
func TestRelayPinIsDroppedWhenTheFirstPostFails(t *testing.T) {
	a, b := newFakeRelay(t), newFakeRelay(t)
	setRelayStatus(a, http.StatusServiceUnavailable)
	e, _ := relayEmitter(t, a, b)
	e.Emit(Event{JobID: "led-p-1", Model: "m", Engine: "llamacpp", State: "queued", CreatedAt: 1})
	e.Wait()
	if a.count() != 1 {
		t.Fatalf("the first frame went to a=%d, want the first relay", a.count())
	}
	if _, pinned := e.pinnedRelay("led-p-1"); pinned {
		t.Fatal("the job is still pinned to a relay that never took one of its frames")
	}
	e.Emit(Event{JobID: "led-p-1", Model: "m", Engine: "llamacpp", State: "running", CreatedAt: 1, StartedAt: 2})
	e.Wait()
	if b.count() != 1 || b.info(0)["state"] != "running" {
		t.Fatalf("the next frame did not move to the healthy relay (a=%d b=%d)", a.count(), b.count())
	}
	if u, pinned := e.pinnedRelay("led-p-1"); !pinned || u != relayURL(b.srv.URL) {
		t.Fatalf("pin = %q (%v), want the relay that took the frame", u, pinned)
	}
	e.Emit(Event{JobID: "led-p-1", Model: "m", Engine: "llamacpp", State: "completed", CreatedAt: 1, CompletedAt: 3})
	e.Wait()
	if b.count() != 2 || a.count() != 1 {
		t.Fatalf("terminal went to a=%d b=%d, want the relay the card is open on", a.count(), b.count())
	}
}

// F3's other side: a pin a relay CONFIRMED stays when a later in-flight post fails, because the card
// is open there and its terminal frame has to reach it.
func TestRelayPinStaysWhenALaterPostFails(t *testing.T) {
	a, b := newFakeRelay(t), newFakeRelay(t)
	e, _ := relayEmitter(t, a, b)
	e.Emit(Event{JobID: "led-p-2", Model: "m", Engine: "llamacpp", State: "queued", CreatedAt: 1})
	e.Wait()
	setRelayStatus(a, http.StatusServiceUnavailable)
	e.Emit(Event{JobID: "led-p-2", Model: "m", Engine: "llamacpp", State: "running", CreatedAt: 1, StartedAt: 2})
	e.Wait()
	if u, pinned := e.pinnedRelay("led-p-2"); !pinned || u != relayURL(a.srv.URL) {
		t.Fatalf("pin = %q (%v): the card is open on the first relay, the pin must stay", u, pinned)
	}
	if b.count() != 0 {
		t.Fatalf("a frame of an open card went to another relay (b=%d)", b.count())
	}
}

// F4: frames of one job are not ordered on the wire (by design, PAIR merges by lifecycle rank), so
// the member ignores an in-flight frame that lands after its card's terminal frame: it must not
// write a marker nothing closes or count a card open.
func TestRelayLimiterIgnoresALateInFlightFrameOfAClosedCard(t *testing.T) {
	now := time.Now()
	l := NewRelayLimiter(1000, 1000, 1000, 1000)
	l.now = func() time.Time { return now }
	l.SetOpenCaps(2, 4)
	if got := l.Admit("node-q", "j1", "queued"); got != CardAdmitted {
		t.Fatalf("queued = %v", got)
	}
	if got := l.Admit("node-q", "j1", "completed"); got != CardAdmitted {
		t.Fatalf("terminal = %v", got)
	}
	if got := l.Admit("node-q", "j1", "running"); got != CardStale {
		t.Fatalf("a running frame after the terminal frame = %v, want CardStale", got)
	}
	if l.OpenCards("node-q") != 0 || l.AdmitCard("node-q", "j1", "running") {
		t.Fatalf("a stale frame reopened the card (open = %d)", l.OpenCards("node-q"))
	}
	if got := l.Admit("node-r", "j1", "running"); got != CardAdmitted {
		t.Fatalf("another asker's card with the same id was refused: %v", got)
	}
	// The cap verdict is still its own.
	l.Admit("node-q", "j2", "queued")
	l.Admit("node-q", "j3", "queued")
	if got := l.Admit("node-q", "j4", "queued"); got != CardCapped {
		t.Fatalf("a third open card past the cap of 2 = %v, want CardCapped", got)
	}
	// An id a producer reuses after the memory window opens a card again.
	l.Admit("node-q", "j2", "completed")
	l.Admit("node-q", "j3", "completed")
	now = now.Add(relayClosedTTL + time.Second)
	if got := l.Admit("node-q", "j1", "queued"); got != CardAdmitted {
		t.Fatalf("a reused id after %s = %v, want admitted", relayClosedTTL, got)
	}
}

func TestRelayLimiterBoundsItsClosedMemory(t *testing.T) {
	now := time.Now()
	l := NewRelayLimiter(1000, 1000, 1000, 1000)
	l.now = func() time.Time { return now }
	for i := 0; i < relayClosedMax+50; i++ {
		now = now.Add(time.Millisecond)
		l.Admit("node-q", "j"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String(), "completed")
	}
	l.mu.Lock()
	n := len(l.closed)
	l.mu.Unlock()
	if n > relayClosedMax {
		t.Fatalf("closed memory = %d entries, bound is %d", n, relayClosedMax)
	}
}

// F5: a terminal frame through a relay with no marker of its own (its in-flight frames never went
// out) is kept as a pending frame when the relay does not take it, and the sweep delivers it.
func TestRelayTerminalWithoutAMarkerIsKeptWhenTheRelayIsDown(t *testing.T) {
	r := newFakeRelay(t)
	setRelayStatus(r, http.StatusServiceUnavailable)
	e, open := relayEmitter(t, r)
	e.Emit(Event{JobID: "led-t-1", Model: "m", Engine: "llamacpp", State: "completed", CreatedAt: 1, CompletedAt: 3})
	e.Wait()
	ents, _ := os.ReadDir(open)
	if len(ents) != 1 {
		t.Fatalf("register = %v, want the pending terminal marker", ents)
	}
	raw, _ := os.ReadFile(filepath.Join(open, ents[0].Name()))
	var mk openMarker
	_ = json.Unmarshal(raw, &mk)
	if !mk.Pending || mk.Relay == nil || mk.Endpoint != r.srv.URL+RelayPath {
		t.Fatalf("marker = %+v, want a pending terminal frame for the relay", mk)
	}

	setRelayStatus(r, http.StatusOK)
	before := r.count()
	if n := sweeperFor(t, open, r).SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the sweep delivered %d, want the kept terminal frame", n)
	}
	if r.count() != before+1 || r.info(before)["state"] != "completed" {
		t.Fatalf("the relay got %d frame(s) after the retry, last state %v", r.count()-before, r.info(r.count() - 1)["state"])
	}
	if ents, _ := os.ReadDir(open); len(ents) != 0 {
		t.Fatalf("register after delivery = %v", ents)
	}

	// A delivered terminal frame leaves nothing behind.
	e.Emit(Event{JobID: "led-t-2", Model: "m", Engine: "llamacpp", State: "completed", CreatedAt: 1, CompletedAt: 3})
	e.Wait()
	if ents, _ := os.ReadDir(open); len(ents) != 0 {
		t.Fatalf("a delivered terminal-only frame left %v", ents)
	}
}

// F6: a card field longer than the member's bound is shortened on the relaying side; the frame is
// never refused (a 400 is permanent: the card would silently never exist).
func TestRelayShortensOverlongFieldsInsteadOfLosingTheCard(t *testing.T) {
	r := newFakeRelay(t)
	e, _ := relayEmitter(t, r)
	longModel := strings.Repeat("é", 200) + ".ps1" // 400+ bytes, multibyte
	longID := "led-" + strings.Repeat("9", 200)
	e.Emit(Event{JobID: longID, Model: longModel, Engine: "gpu-lease", State: "queued", CreatedAt: 1,
		Requester: "offload-harness/" + strings.Repeat("s", 300)})
	e.Wait()
	e.Emit(Event{JobID: longID, Model: longModel, Engine: "gpu-lease", State: "completed", CreatedAt: 1, CompletedAt: 2,
		Requester: "offload-harness/" + strings.Repeat("s", 300)})
	e.Wait()
	if r.count() != 2 {
		t.Fatalf("relay hits = %d", r.count())
	}
	var ids []string
	for i := 0; i < 2; i++ {
		raw, _ := json.Marshal(r.hit(i).body)
		if _, err := ParseRelay(raw, "node-q"); err != nil {
			t.Fatalf("frame %d is refused by the member's decode: %v", i, err)
		}
		info := r.info(i)
		model, _ := info["model"].(string)
		if len(model) > relayFieldMax || !utf8.ValidString(model) || model == "" {
			t.Fatalf("model = %q (%d bytes)", model, len(model))
		}
		ids = append(ids, info["id"].(string))
	}
	if ids[0] != ids[1] || len(ids[0]) > relayFieldMax {
		t.Fatalf("the cut id differs between a job's frames or is over the bound: %q / %q", ids[0], ids[1])
	}
	if relayID(longID) == relayID(longID[:len(longID)-1]+"8") {
		t.Fatal("two long ids that differ past the cut share a card")
	}
}

func TestRelayTextCutsOnARuneBoundaryAndScrubsControls(t *testing.T) {
	if got := relayText("short", 128); got != "short" {
		t.Fatalf("a field within the bound changed: %q", got)
	}
	got := relayText(strings.Repeat("é", 100), 129)
	if len(got) > 129 || !utf8.ValidString(got) || len(got) != 128 {
		t.Fatalf("cut = %d bytes valid=%v, want 128 bytes on a rune boundary", len(got), utf8.ValidString(got))
	}
	if got := relayText("a\x00b\nc", 128); got != "a b c" {
		t.Fatalf("control characters = %q", got)
	}
}
