package pipeline

// F24: a card-table read that times out under load must not hard-defer a media call.
//
// The incident (2026-10-09, a session benchmarking image generation while two cards ran other
// sessions' media jobs): offload_generate_image deferred with "the card table: nvidia-smi:
// nvidia-smi: context deadline exceeded" after 6 s, while its neighbours got the ordinary
// "gpu queued: card(s) ... held by media" answer. The call's ADMISSION read had answered (about a
// second); the read inside its allocation (gpualloc.BuildInput) ran out its 5 s, and the raw
// error left acquireCards for deferForLease, which classes anything but a queue as
// gpu_lease_unavailable. Everything below drives that shape with a scripted reader and scratch
// leases: no nvidia-smi, no card, no ComfyUI.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// errSmiTimeout is the incident's error, word for word: Read's own prefix, ReadWith's, then the
// deadline.
var errSmiTimeout = fmt.Errorf("nvidia-smi: nvidia-smi: %w", context.DeadlineExceeded)

// flakyTable stands in for nvidia-smi under load: a card-table reader whose answers a test
// scripts. While answers remain it gives the fixture's table; after that it fails the way a
// timed-out nvidia-smi does, either at once or, with hang set, only when the caller's deadline
// ends it (a real hang, the one kind a retry under a longer deadline can tell from a quick
// failure).
type flakyTable struct {
	mu      sync.Mutex
	cards   []gpuprobe.Card
	answers int      // reads still answered from the table; negative = unlimited
	hang    bool     // fail by blocking until the read's context is done
	seq     []string // scripted reads ("ok", "fail", "hang"), taken in order before answers and hang apply
	reads   int
	left    []time.Duration // time left on the read's deadline when it began (0 = no deadline)
}

// flakyTable installs a scripted reader that answers every read until a test says otherwise.
func (f *admitFixture) flakyTable() *flakyTable {
	tbl := &flakyTable{cards: f.cards, answers: -1}
	f.p.alloc.Cards = tbl.read
	return tbl
}

// script sets how many more reads are answered (negative = all), and whether the failing ones hang.
func (t *flakyTable) script(answers int, hang bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.answers, t.hang = answers, hang
}

// steps scripts the next reads one by one: "ok" answers, "fail" errors at once, "hang" blocks until the
// read's deadline. Reads after the script follow script().
func (t *flakyTable) steps(modes ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq = append([]string(nil), modes...)
}

func (t *flakyTable) read(ctx context.Context, _ config.Config) ([]gpuprobe.Card, string, error) {
	t.mu.Lock()
	t.reads++
	left := time.Duration(0)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	t.left = append(t.left, left)
	answer, hang := t.answers != 0, t.hang
	if len(t.seq) > 0 {
		mode := t.seq[0]
		t.seq = t.seq[1:]
		answer, hang = mode == "ok", mode == "hang"
	} else if t.answers > 0 {
		t.answers--
	}
	cards := t.cards
	t.mu.Unlock()
	if answer {
		return cards, "", nil
	}
	if hang {
		select {
		case <-ctx.Done():
			return nil, "", fmt.Errorf("nvidia-smi: nvidia-smi: %w", ctx.Err())
		case <-time.After(30 * time.Second): // a context with no deadline: fail the test, not the suite
			return nil, "", fmt.Errorf("the read was never given a deadline")
		}
	}
	return nil, "", errSmiTimeout
}

func (t *flakyTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reads
}

func (t *flakyTable) deadlines() []time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]time.Duration(nil), t.left...)
}

// syncBuffer is a log sink the media goroutines can write while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureAdmissionLog routes the standard logger to a buffer for the test.
func captureAdmissionLog(t *testing.T) *syncBuffer {
	t.Helper()
	out := &syncBuffer{}
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(out)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return out
}

// holdCard takes a card the way another session's media job does: a lease on that card alone.
func (f *admitFixture) holdCard(uuid string) *gpulease.Lease {
	f.t.Helper()
	l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another session's media job", TTL: time.Hour, Devices: []string{leaseIDOf(uuid)}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = l.Release() })
	return l
}

// wantQueued fails unless res is the ordinary queued answer and returns its payload.
func wantQueued(t *testing.T, res core.Result) queuedPayload {
	t.Helper()
	if res.Meta.ErrClass == "gpu_lease_unavailable" {
		t.Fatalf("a card-table read timeout hard-deferred the call (the F24 defect): %s", res.Reason)
	}
	if res.OK || !res.Deferred || res.Meta.ErrClass != "gpu_queued" || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want the queued answer (gpu_queued / capacity), got ok=%v deferred=%v class=%q defer=%q: %s", res.OK, res.Deferred, res.Meta.ErrClass, res.DeferClass, res.Reason)
	}
	p := queuedData(t, res)
	if !strings.HasPrefix(p.Token, "tk-") {
		t.Fatalf("the queued answer must carry a waiter token, got %q (%s)", p.Token, res.Data)
	}
	return p
}

// ---------------------------------------------------------------------------------------------

// The incident. Two other jobs hold the two free cards; the third call's admission read answers and
// the read inside its allocation times out. It must queue with a token, like the calls around it.
func TestAnAllocationReadTimeoutQueuesTheCallInsteadOfDeferringIt(t *testing.T) {
	logs := captureAdmissionLog(t)
	f := newAdmitFixture(t, nil)
	tbl := f.flakyTable()
	a, b := f.image(nil), f.image(nil)
	f.waitStarted(2)

	before := tbl.count() // the two calls above read the table too; this one's reads are counted from here
	tbl.script(1, false)  // this call's admission read answers; the allocation's re-read times out
	res := f.await(f.image(nil))
	p := wantQueued(t, res)
	if len(p.Devices) != 1 || (p.Devices[0] != leaseIDOf(admitUUIDA) && p.Devices[0] != leaseIDOf(admitUUIDC)) {
		t.Errorf("it waits for one of the two held non-display cards, got %v", p.Devices)
	}
	if p.Position != 1 {
		t.Errorf("queue position = %d, want 1", p.Position)
	}
	// (4) The answer says the table was unreadable and what the call did instead; so does the log.
	for _, want := range []string{"card table", "could not be re-read", "earlier", "screen's card kept closed"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the answer must say the table could not be re-read and what the call did instead (missing %q): %s", want, res.Reason)
		}
	}
	if !strings.Contains(logs.String(), "the card table could not be re-read") {
		t.Errorf("the degrade is logged:\n%s", logs.String())
	}
	// A failure that comes back at once is not a slow read: no retry would change it, and once the call
	// is placed from its stored table it does not ask nvidia-smi again (the admission read, the one failure).
	if n := tbl.count() - before; n != 2 {
		t.Errorf("the call read the table %d times, want 2 (admission, then the allocation's read that failed)", n)
	}

	// The token is good: free the cards and the same request resumes the place and runs.
	tbl.script(-1, false)
	f.letRunnersGo()
	f.await(a)
	f.await(b)
	again := f.await(f.image(map[string]any{"waiter_token": p.Token}))
	if !again.OK {
		t.Fatalf("the resumed call must run once a card is free: %+v", again)
	}
	if toks := f.m.Tokens(); len(toks) != 0 {
		t.Errorf("tokens left over: %+v", toks)
	}
}

// A door that cannot resume (the CLI verbs, the fleet dispatch) gets the plain busy answer it always
// got, with the same words about the table, and leaves no place behind.
func TestADoorThatCannotResumeGetsBusyWithTheTableNoteAndNoToken(t *testing.T) {
	for name, tc := range map[string]struct {
		readsAnswered int // reads answered before the table dies: 0 = the admission read itself fails
		want          string
	}{
		"re-read inside the allocation": {1, "could not be re-read"},
		"admission read":                {0, "could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
			f.holdCard(admitUUIDA)
			f.holdCard(admitUUIDC)
			f.flakyTable().script(tc.readsAnswered, false)
			res := f.await(f.plainImage())
			if res.Meta.ErrClass == "gpu_lease_unavailable" {
				t.Fatalf("a card-table read timeout hard-deferred the call: %s", res.Reason)
			}
			busyNotQueued(t, res)
			if !strings.Contains(res.Reason, tc.want) || !strings.Contains(res.Reason, "card table") {
				t.Errorf("the busy answer must carry the words about the table (%q): %s", tc.want, res.Reason)
			}
			if n := len(f.m.Tokens()); n != 0 {
				t.Errorf("%d token(s) left by a call that can never resume one", n)
			}
		})
	}
}

// A call whose admission read answered keeps placing by card when the re-read dies: with a card
// free it RUNS, on a non-display card, and says nothing to the caller.
func TestAnAllocationReadTimeoutStillPlacesOnAFreeCard(t *testing.T) {
	f := newAdmitFixture(t, nil)
	tbl := f.flakyTable()
	tbl.script(1, false)
	ch := f.image(nil)
	probes := f.waitStarted(1)
	if uuid := probes[0].Env["COMFY_CARD_UUID"]; uuid != admitUUIDA && uuid != admitUUIDC {
		t.Errorf("the call ran on %q, want a non-display card (the monitor is on %s)", uuid, admitUUIDB)
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("the call must run: %+v", r)
	}
}

// Naming the cards is the caller's word: a pin queues on the pinned card without needing the card
// table again, so a dead table cannot touch it.
func TestAPinnedCallQueuesOnItsCardWhateverTheTableDoes(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	f.holdCard(admitUUIDC) // ComfyUI index 2 is nvidia index 2: card C
	tbl := f.flakyTable()
	tbl.script(1, false) // the admission read, then a dead table

	res := f.await(f.image(nil))
	p := wantQueued(t, res)
	if len(p.Devices) != 1 || p.Devices[0] != leaseIDOf(admitUUIDC) {
		t.Errorf("the place is for the pinned card, and only that one: %v", p.Devices)
	}
	if n := tbl.count(); n != 1 {
		t.Errorf("a call that named its card needs the table once, to turn the pin into a card; it read it %d times", n)
	}
}

// Same for a run-graph that declares one device: the card is named, nothing to choose.
func TestADeclaredDeviceQueuesOnItsCardWhateverTheTableDoes(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	f.holdCard(admitUUIDA)
	tbl := f.flakyTable()
	tbl.script(1, false)

	res := f.await(f.graph(map[string]any{"devices": []string{"0"}}))
	p := wantQueued(t, res)
	if len(p.Devices) != 1 || p.Devices[0] != leaseIDOf(admitUUIDA) {
		t.Errorf("the place is for the declared card: %v", p.Devices)
	}
	if n := tbl.count(); n != 1 {
		t.Errorf("read the table %d times, want once (to resolve the declared device)", n)
	}
}

// The admission read itself dead (the case planMedia already degrades): the call asks for the whole
// node, as it did before cards were leased, and now the answer SAYS why it is waiting for the whole
// node and not for a card.
func TestAnUnreadableTableAtAdmissionQueuesOnTheWholeNodeAndSaysSo(t *testing.T) {
	logs := captureAdmissionLog(t)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	f.holdCard(admitUUIDA)
	f.holdCard(admitUUIDC)
	tbl := f.flakyTable()
	tbl.script(0, false) // every read fails

	res := f.await(f.image(nil))
	p := wantQueued(t, res)
	if len(p.Devices) != 0 {
		t.Errorf("a whole-node place names no card, got %v", p.Devices)
	}
	for _, want := range []string{"card table could not be read", "whole node"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the answer must say the table was unreadable and that the call went to the whole node (missing %q): %s", want, res.Reason)
		}
	}
	if !strings.Contains(logs.String(), "the card table could not be read") {
		t.Errorf("the degrade is logged:\n%s", logs.String())
	}
}

// The grant-time check (gpualloc.GrantCheck) reads the table again when a queued call's turn comes.
// A read that dies THEN must not turn a call that waited its turn into a defer: a non-display card
// is granted on the table the call already holds.
func TestAGrantTimeReadTimeoutStillGrantsANonDisplayCard(t *testing.T) {
	f := newAdmitFixture(t, func(c *config.Config) { c.GPUWaitMs = 20000 })
	holdA, holdC := f.holdCard(admitUUIDA), f.holdCard(admitUUIDC)
	tbl := f.flakyTable()
	ch := f.image(nil)

	deadline := time.Now().Add(15 * time.Second)
	for len(f.m.Waiters()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the call never queued for a held card")
		}
		time.Sleep(20 * time.Millisecond)
	}
	tbl.script(0, false) // the table dies while the call waits in line
	_ = holdC
	_ = holdA.Release() // its turn: card A (first of the two it could wait for) comes free

	probes := f.waitStarted(1)
	if uuid := probes[0].Env["COMFY_CARD_UUID"]; uuid != admitUUIDA {
		t.Errorf("the call ran on %q, want card A, the one it queued for", uuid)
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("the grant must not be refused over a table that cannot be read: %+v", r)
	}
}

// What the unreadable table must NOT do is open the operator's screen. With the operator away and
// no desktop floor declared the allocator does hand out the display card; a call placed on a table
// it could not refresh keeps it closed.
func TestADegradedReadKeepsTheDisplayCardClosed(t *testing.T) {
	// Control: table healthy, operator away, the two other cards held: the call takes the display card.
	ctl := newAdmitFixture(t, nil)
	ctl.away = true
	ctl.holdCard(admitUUIDA)
	ctl.holdCard(admitUUIDC)
	ch := ctl.image(nil)
	if uuid := ctl.waitStarted(1)[0].Env["COMFY_CARD_UUID"]; uuid != admitUUIDB {
		t.Fatalf("premise: with the operator away and no floor the display card is free for the call, got %q", uuid)
	}
	ctl.letRunnersGo()
	ctl.await(ch)

	f := newAdmitFixture(t, nil)
	f.away = true
	f.holdCard(admitUUIDA)
	f.holdCard(admitUUIDC)
	tbl := f.flakyTable()
	tbl.script(1, false)
	res := f.await(f.image(nil))
	p := wantQueued(t, res)
	for _, d := range p.Devices {
		if d == leaseIDOf(admitUUIDB) {
			t.Errorf("a table that could not be refreshed opened the display card: %v", p.Devices)
		}
	}
	if got := f.started(); len(got) != 0 {
		t.Errorf("the call ran on %v although every non-display card is held", got)
	}
}

// The unload list a lease may take is scoped by WHICH CARDS the seats sit on. That needs the card
// list, not a fresh reading: a read that died after admission used to widen it to every seat
// (register C-86 again), emptying the seats on the cards the call does not hold.
func TestTheUnloadListIsScopedFromTheTableTheCallAlreadyHolds(t *testing.T) {
	f := newAdmitFixture(t, nil)
	tbl := f.flakyTable()
	tbl.script(2, false) // admission, the allocation's own read; the next read (once the lease is granted) dies
	ch := f.image(nil)
	probe := f.waitStarted(1)[0]
	if probe.Env["COMFY_CARD_UUID"] != admitUUIDA {
		t.Fatalf("premise: the call runs on card A, got %q", probe.Env["COMFY_CARD_UUID"])
	}
	if got := probe.Env["GPU_LEASE_UNLOAD_MODELS"]; got != "seat-a,seat-unpinned" {
		t.Errorf("card A's render may unload %q, want seat-a and the unpinned seat only (seat-c sits on card C)", got)
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("%+v", r)
	}
}

// The queued answer's payload keeps its keys: the note rides in the reason, never as a new shape.
func TestTheDegradedAnswerKeepsThePayloadShape(t *testing.T) {
	f := newAdmitFixture(t, nil)
	f.holdCard(admitUUIDA)
	f.holdCard(admitUUIDC)
	tbl := f.flakyTable()
	tbl.script(1, false)
	res := f.await(f.image(nil))
	wantQueued(t, res)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(res.Data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"queued", "waiter_token", "queue_position", "eta_s", "devices", "held_by", "resume"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("payload lost key %q: %s", k, res.Data)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// The retry: a read that runs out of time is made once more under a longer deadline
// ---------------------------------------------------------------------------------------------

// shortReads gives the allocator's card-table reader deadlines of milliseconds, so a hang costs a blink.
func (f *admitFixture) shortReads(first, retry time.Duration) {
	f.p.alloc.ReadDeadline, f.p.alloc.RetryDeadline = first, retry
}

// A read that hangs once is read again under the longer deadline, and the call is placed from THAT
// reading: nothing degrades, nothing is said, the placement is the one a quiet box would have given.
func TestAReadThatRunsOutOfTimeOnceIsReadAgainAndPlacesOnFreshData(t *testing.T) {
	logs := captureAdmissionLog(t)
	f := newAdmitFixture(t, nil)
	f.shortReads(150*time.Millisecond, 3*time.Second)
	tbl := f.flakyTable()
	tbl.steps("ok", "hang", "ok") // admission; the allocation's first attempt hangs; its retry answers
	ch := f.image(nil)
	probes := f.waitStarted(1)
	if uuid := probes[0].Env["COMFY_CARD_UUID"]; uuid != admitUUIDA && uuid != admitUUIDC {
		t.Errorf("the call ran on %q, want a non-display card", uuid)
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("%+v", r)
	}
	d := tbl.deadlines()
	if len(d) != 3 {
		t.Fatalf("%d reads (%v), want 3: admission, the attempt that hung, its retry", len(d), d)
	}
	if d[1] > 150*time.Millisecond {
		t.Errorf("the first attempt had %v, want the first deadline (150ms)", d[1])
	}
	if d[2] < 2*time.Second || d[2] > 3*time.Second {
		t.Errorf("the retry had %v, want the longer deadline (3s)", d[2])
	}
	if !strings.Contains(logs.String(), "reading it once more") {
		t.Errorf("the retry is logged:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "could not be re-read") {
		t.Errorf("the retry answered, so nothing was degraded:\n%s", logs.String())
	}
}

// A read that hangs both times is read twice and no more: the call goes on from the table it holds, says
// it was read twice, and does not spend a third deadline on any later stage.
func TestAHungReadIsReadTwiceThenTheCallPlacesFromTheTableItHolds(t *testing.T) {
	f := newAdmitFixture(t, nil)
	f.shortReads(60*time.Millisecond, 300*time.Millisecond)
	f.holdCard(admitUUIDA)
	f.holdCard(admitUUIDC)
	tbl := f.flakyTable()
	tbl.steps("ok")     // admission answers
	tbl.script(0, true) // then every read hangs until its deadline

	start := time.Now()
	res := f.await(f.image(nil))
	p := wantQueued(t, res)
	if len(p.Devices) != 1 {
		t.Errorf("devices = %v", p.Devices)
	}
	for _, want := range []string{"could not be re-read", "read twice: no answer within 60ms, then none within 300ms"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the answer must say how the table was tried (missing %q): %s", want, res.Reason)
		}
	}
	d := tbl.deadlines()
	if len(d) != 3 {
		t.Fatalf("%d reads (%v), want 3: admission and the allocation's two attempts", len(d), d)
	}
	if d[2] <= d[1] {
		t.Errorf("the retry must have the longer deadline: %v then %v", d[1], d[2])
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a hung nvidia-smi cost the call %v", took)
	}
}

// After the table failed once the call stays on its stored table: the grant-time check (which reads
// the table again when a queued call's turn comes) makes no further read, so a wedged driver costs one
// retry per call, not one per stage.
func TestADegradedCallDoesNotReadTheTableAgainAtTheGrant(t *testing.T) {
	f := newAdmitFixture(t, func(c *config.Config) { c.GPUWaitMs = 20000 })
	f.shortReads(60*time.Millisecond, 300*time.Millisecond)
	holdA := f.holdCard(admitUUIDA)
	f.holdCard(admitUUIDC)
	tbl := f.flakyTable()
	tbl.steps("ok")
	tbl.script(0, true)
	ch := f.image(nil)

	deadline := time.Now().Add(15 * time.Second)
	for len(f.m.Waiters()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the call never queued for a held card")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = holdA.Release()
	if uuid := f.waitStarted(1)[0].Env["COMFY_CARD_UUID"]; uuid != admitUUIDA {
		t.Errorf("the call ran on %q, want card A", uuid)
	}
	if n := tbl.count(); n != 3 {
		t.Errorf("%d reads, want 3 (admission and two attempts): the grant-time check must reuse the stored table", n)
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("%+v", r)
	}
}

// The admission read itself hanging: read twice, then the whole node (the answer says so). This is the
// one case planMedia already degraded; what is new is the second attempt before it does.
func TestAnAdmissionReadThatNeverAnswersIsReadTwiceThenTheCallAsksForTheWholeNode(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	f.shortReads(60*time.Millisecond, 300*time.Millisecond)
	f.holdCard(admitUUIDA)
	f.holdCard(admitUUIDC)
	tbl := f.flakyTable()
	tbl.script(0, true)

	res := f.await(f.image(nil))
	p := wantQueued(t, res)
	if len(p.Devices) != 0 {
		t.Errorf("a whole-node place names no card, got %v", p.Devices)
	}
	if !strings.Contains(res.Reason, "read twice: no answer within 60ms, then none within 300ms") {
		t.Errorf("the answer must say how the table was tried: %s", res.Reason)
	}
	if n := tbl.count(); n != 2 {
		t.Errorf("%d reads, want 2 (the attempt and its retry): the whole-node path reads nothing", n)
	}
}
