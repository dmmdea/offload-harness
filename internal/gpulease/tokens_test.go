package gpulease

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A place-keeping token (plan P13). A media call that waited its window and still has no card
// does not fail with "busy": it leaves a TOKEN, a record of its place in line, and tells the
// caller. The caller re-calls with the token and the call resumes the place it left.
//
// The token has no process behind it (the caller's MCP server is always alive, which says
// nothing about whether the caller is coming back), so its life is its last poll:
//
//   - for TokenGrace after the poller left, the place is held: later waiters on the same cards
//     queue behind it;
//   - after that the token is ABSENT: skipped by every waiter, so a client that wandered off
//     never blocks the line, and ignored by a whole-node barrier;
//   - for TokenTTL after the poller left it can still be resumed, with the original arrival
//     time, so a client that comes back is ahead of everyone who arrived after it.
//
// Tokens live in their own directory, not among the waiters: a binary that predates them prunes
// every waiter record whose process is not polling, and would drop a token it cannot refresh.

func tokenManager(t *testing.T) (*Manager, *time.Time) {
	t.Helper()
	m, now := newTestManager(t)
	m.tokenGrace = 30 * time.Second
	return m, now
}

func TestLeaveTokenWritesAResumablePlace(t *testing.T) {
	m, now := tokenManager(t)
	since := now.Add(-3 * time.Second)
	tok, err := m.LeaveToken(ClassMedia, Options{Reason: "image-gen", Devices: []string{"gpu-aaaa", "gpu-bbbb"}}, since)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.ID, "tk-") || len(tok.ID) < 10 {
		t.Fatalf("token id = %q", tok.ID)
	}
	got, ok := m.ResumeToken(tok.ID)
	if !ok {
		t.Fatal("a token just left must be resumable")
	}
	if got.Class != ClassMedia || got.Reason != "image-gen" || len(got.Devices) != 2 || !got.Since().Equal(since.Truncate(time.Millisecond)) {
		t.Fatalf("resumed = %+v, want the class, reason, cards and arrival time left", got)
	}
}

func TestATokenIsResumableForTenMinutesAfterTheLastPoll(t *testing.T) {
	m, now := tokenManager(t)
	tok, _ := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	*now = now.Add(TokenTTL - time.Second)
	if _, ok := m.ResumeToken(tok.ID); !ok {
		t.Fatal("a token must still be resumable just inside its ten minutes")
	}
	*now = now.Add(2 * time.Second)
	if _, ok := m.ResumeToken(tok.ID); ok {
		t.Fatal("a token past ten minutes since its last poll must not resume")
	}
	if entries, _ := os.ReadDir(m.tokensDir()); len(entries) != 0 {
		t.Fatalf("an expired token must be pruned on read, %d file(s) left", len(entries))
	}
}

// Leaving a token again with the same place (the caller came back, waited, gave up again)
// refreshes its poll time and keeps one record.
func TestLeavingTheSameTokenAgainRefreshesItsPoll(t *testing.T) {
	m, now := tokenManager(t)
	first, _ := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	*now = now.Add(9 * time.Minute)
	again, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}, ResumeToken: first.ID}, first.Since())
	if err != nil || again.ID != first.ID {
		t.Fatalf("again = %+v err=%v, want the same token id", again, err)
	}
	*now = now.Add(9 * time.Minute) // 18 minutes after the first poll, 9 after the second
	if _, ok := m.ResumeToken(first.ID); !ok {
		t.Fatal("a token whose poller came back must live ten minutes from the LAST poll")
	}
	if entries, _ := os.ReadDir(m.tokensDir()); len(entries) != 1 {
		t.Fatalf("one place in line is one record, got %d", len(entries))
	}
}

func TestATokenIdThatIsNotOursIsNeverOpened(t *testing.T) {
	m, _ := tokenManager(t)
	for _, id := range []string{"", "..", "../meta", `..\meta`, "tk-../x", "TK-ABCDEFGH", "tk-", "tk-short", strings.Repeat("a", 200)} {
		if _, ok := m.ResumeToken(id); ok {
			t.Errorf("token %q must not resume", id)
		}
		m.DropToken(id) // must be harmless
	}
	// A file placed next to the tokens is not reachable through an id.
	if err := os.MkdirAll(m.tokensDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	// A well-formed token one level up: reachable only if the id is allowed to carry a path.
	secret := `{"id":"tk-abcdefgh","class":"media","since_ms":1,"polled_ms":` + strconv.FormatInt(m.now().UnixMilli(), 10) + `}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(m.tokensDir()), "secret.json"), []byte(secret), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.ResumeToken("../secret"); ok {
		t.Fatal("a path component in a token id must not be followed")
	}
}

// ---------------------------------------------------------------------------
// Ordering: a token is a waiter that is not there
// ---------------------------------------------------------------------------

func registerAt(t *testing.T, m *Manager, opts Options) Waiter {
	t.Helper()
	w, unreg := m.registerWaiter(ClassMedia, opts)
	t.Cleanup(unreg)
	if w.path == "" {
		t.Fatal("waiter registration failed")
	}
	return w
}

// A live token holds its place: a later waiter on the same card queues behind it.
func TestALiveTokenBlocksALaterWaiterOnItsCard(t *testing.T) {
	m, now := tokenManager(t)
	if _, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5 * time.Second)
	w := registerAt(t, m, Options{Devices: []string{"gpu-aaaa"}})
	if m.isFrontOfQueue(w) {
		t.Fatal("a token left five seconds ago still holds its place: the later waiter is not first")
	}
}

// TestAbsentTokenDoesNotBlockLaterWaiters: the poller is gone for longer than the grace, so the
// line moves on without it.
func TestAbsentTokenDoesNotBlockLaterWaiters(t *testing.T) {
	m, now := tokenManager(t)
	if _, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(m.tokenGrace + time.Second)
	w := registerAt(t, m, Options{Devices: []string{"gpu-aaaa"}})
	if !m.isFrontOfQueue(w) {
		t.Fatal("a token absent past its grace must not hold back a later waiter")
	}
	if _, ok := m.ResumeToken(firstTokenID(t, m)); !ok {
		t.Fatal("an absent token is still resumable within its ten minutes: it only stopped blocking")
	}
}

// A token on another card never blocks (disjoint backfill, as for any waiter).
func TestALiveTokenDoesNotBlockADisjointWaiter(t *testing.T) {
	m, now := tokenManager(t)
	_, _ = m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	*now = now.Add(time.Second)
	if w := registerAt(t, m, Options{Devices: []string{"gpu-bbbb"}}); !m.isFrontOfQueue(w) {
		t.Fatal("a waiter for another card is not behind a token for this one")
	}
}

// TestWholeNodeBarrierIgnoresExpiredToken: a token that wants the whole node holds back every
// later waiter while its poller is within the grace, and nobody once it is absent.
func TestWholeNodeBarrierIgnoresExpiredToken(t *testing.T) {
	m, now := tokenManager(t)
	_, _ = m.LeaveToken(ClassMedia, Options{}, *now) // the whole node
	*now = now.Add(time.Second)
	w := registerAt(t, m, Options{Devices: []string{"gpu-bbbb"}})
	if m.isFrontOfQueue(w) {
		t.Fatal("a live whole-node token is a barrier: every later waiter queues behind it")
	}
	*now = now.Add(m.tokenGrace + time.Second)
	if !m.isFrontOfQueue(w) {
		t.Fatal("a whole-node token whose poller has been gone past the grace must be ignored")
	}
	// An expired token (past its ten minutes) is gone altogether.
	*now = now.Add(TokenTTL)
	if len(m.Tokens()) != 0 {
		t.Fatalf("expired tokens must not be listed: %+v", m.Tokens())
	}
}

// TestTokenResumesFifoPlace: the caller comes back inside the window; its waiter carries the
// ORIGINAL arrival time, so it is ahead of the waiter that arrived after it left.
func TestTokenResumesFifoPlace(t *testing.T) {
	m, now := tokenManager(t)
	tok, _ := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	*now = now.Add(10 * time.Second)
	later := registerAt(t, m, Options{Devices: []string{"gpu-aaaa"}}) // arrived after the token's owner left
	if m.isFrontOfQueue(later) {
		t.Fatal("setup: the later waiter is behind the live token")
	}
	*now = now.Add(5 * time.Second)

	resumed := registerAt(t, m, Options{Devices: []string{"gpu-aaaa"}, ResumeToken: tok.ID})
	if resumed.SinceMs != tok.SinceMs {
		t.Fatalf("the resumed waiter carries since %d, want the token's %d", resumed.SinceMs, tok.SinceMs)
	}
	if !m.isFrontOfQueue(resumed) {
		t.Fatal("the resumed waiter is ahead of everyone who arrived after the token was left")
	}
	if m.isFrontOfQueue(later) {
		t.Fatal("the later waiter must now queue behind the resumed one")
	}
	if _, ok := m.ResumeToken(tok.ID); ok {
		t.Fatal("a resumed token is consumed: the live waiter stands for it")
	}
}

// A token that no longer exists (expired, resumed by someone else) resumes nothing: the call is a
// new arrival, never an error.
func TestResumingAnUnknownTokenIsANewArrival(t *testing.T) {
	m, now := tokenManager(t)
	w := registerAt(t, m, Options{Devices: []string{"gpu-aaaa"}, ResumeToken: "tk-" + strings.Repeat("z", 12)})
	if w.SinceMs != now.UnixMilli() {
		t.Fatalf("since = %d, want now (%d): an unknown token is a new arrival", w.SinceMs, now.UnixMilli())
	}
}

// A waiter that resumed from a token never waits behind that very token, even if the file is
// still there.
func TestAResumedWaiterIgnoresItsOwnTokenRecord(t *testing.T) {
	m, now := tokenManager(t)
	tok, _ := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	*now = now.Add(time.Second)
	// A later arrival time than the token's (the place was left again after waiting): only the id
	// keeps the waiter from queueing behind the record of its own place.
	self := Waiter{PID: os.Getpid(), Class: ClassMedia, SinceMs: tok.SinceMs + 5000, Devices: []string{"gpu-aaaa"}, Token: tok.ID, path: filepath.Join(m.waitersDir(), "self.json")}
	if !m.isFrontOfQueue(self) {
		t.Fatal("a waiter must not queue behind its own token")
	}
}

// ---------------------------------------------------------------------------
// Position
// ---------------------------------------------------------------------------

func TestQueuePositionCountsLiveWaitersAndTokensAheadThatConflict(t *testing.T) {
	m, now := tokenManager(t)
	_, _ = m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, now.Add(-30*time.Second)) // ahead, conflicts (live)
	_, _ = m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-bbbb"}}, now.Add(-29*time.Second)) // ahead, other card
	registerAt(t, m, Options{Devices: []string{"gpu-aaaa"}, QueuedSince: now.Add(-20 * time.Second)}) // ahead, conflicts
	registerAt(t, m, Options{Devices: []string{"gpu-aaaa"}, QueuedSince: now.Add(5 * time.Second)})   // behind
	if got := m.QueuePosition([]string{"gpu-aaaa"}, *now, ""); got != 3 {
		t.Fatalf("position = %d, want 3: the token and the waiter ahead on this card, then this call", got)
	}
	if got := m.QueuePosition([]string{"gpu-cccc"}, *now, ""); got != 1 {
		t.Fatalf("position = %d, want 1 on a card nobody waits for", got)
	}
	// A whole-node request queues behind everyone ahead of it.
	if got := m.QueuePosition(nil, *now, ""); got != 4 {
		t.Fatalf("position = %d, want 4 for the whole node (everything ahead conflicts)", got)
	}
}

func TestQueuePositionSkipsAbsentTokens(t *testing.T) {
	m, now := tokenManager(t)
	_, _ = m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	*now = now.Add(m.tokenGrace + time.Second)
	if got := m.QueuePosition([]string{"gpu-aaaa"}, *now, ""); got != 1 {
		t.Fatalf("position = %d, want 1: an absent token is not ahead of anyone", got)
	}
}

func TestTokensListsOnlyTheUnexpiredAndSurvivesGarbage(t *testing.T) {
	m, now := tokenManager(t)
	a, _ := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	if err := os.WriteFile(filepath.Join(m.tokensDir(), "garbage.json"), []byte("{"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.tokensDir(), "notes.txt"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	got := m.Tokens()
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("tokens = %+v, want only the real one", got)
	}
	if _, err := os.Stat(filepath.Join(m.tokensDir(), "garbage.json")); !os.IsNotExist(err) {
		t.Errorf("a malformed token record is debris and is removed on read (stat err: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(m.tokensDir(), "notes.txt")); err != nil {
		t.Errorf("a file that is not a token record is left alone: %v", err)
	}
	m.DropToken(a.ID)
	if len(m.Tokens()) != 0 {
		t.Fatal("a dropped token is gone")
	}
}

// ---------------------------------------------------------------------------
// End to end through Acquire
// ---------------------------------------------------------------------------

// A real queued Acquire is held back by a live token ahead of it on its card, and runs once the
// token's grace has passed with the card free.
func TestAcquireWaitsBehindALiveTokenThenPassesAnAbsentOne(t *testing.T) {
	m := realTimeTokenManager(t, 400*time.Millisecond)
	m.SetCardScoped(true)
	holder, err := m.TryAcquire(ClassMedia, Options{Devices: []string{"gpu-aaaa"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassMedia, Options{Devices: []string{"gpu-aaaa"}, TTL: time.Hour, Wait: 10 * time.Second, WaitOut: true})
		if aerr == nil {
			_ = l.Release()
		}
		got <- aerr
	}()
	time.Sleep(100 * time.Millisecond)
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	if err := <-got; err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if el := time.Since(start); el < 350*time.Millisecond {
		t.Fatalf("acquired after %v: it must have waited out the live token's grace (400 ms)", el)
	}
}

func TestAcquireResumingATokenIsServedBeforeALaterWaiter(t *testing.T) {
	m := realTimeTokenManager(t, 5*time.Second)
	m.SetCardScoped(true)
	holder, err := m.TryAcquire(ClassMedia, Options{Devices: []string{"gpu-aaaa"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 2)
	acquire := func(name string, opts Options) {
		opts.Devices, opts.TTL, opts.Wait, opts.WaitOut = []string{"gpu-aaaa"}, time.Hour, 20*time.Second, true
		l, aerr := m.Acquire(ClassMedia, opts)
		if aerr != nil {
			order <- name + ":" + aerr.Error()
			return
		}
		order <- name
		time.Sleep(50 * time.Millisecond)
		_ = l.Release()
	}
	go acquire("later", Options{})
	time.Sleep(150 * time.Millisecond)
	go acquire("resumed", Options{ResumeToken: tok.ID})
	time.Sleep(150 * time.Millisecond)
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	first, second := <-order, <-order
	if first != "resumed" || second != "later" {
		t.Fatalf("served %q then %q, want the resumed place first", first, second)
	}
}

// realTimeTokenManager is a Manager on the real clock with a short grace and poll, for the
// tests that run real Acquire loops.
func realTimeTokenManager(t *testing.T, grace time.Duration) *Manager {
	t.Helper()
	m, err := OpenAt("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.pollEvery = 20 * time.Millisecond
	m.tokenGrace = grace
	return m
}

func firstTokenID(t *testing.T, m *Manager) string {
	t.Helper()
	ts := m.Tokens()
	if len(ts) == 0 {
		t.Fatal("no token on disk")
	}
	return ts[0].ID
}

// ---------------------------------------------------------------------------
// A wait that ends without ever reaching the front is a place in line, not a failure
// ---------------------------------------------------------------------------

// A waiter ahead of this call that never claims (a seat admission, say) keeps it off the front
// for its whole window while the card sits free. That is still "queued", and a caller that
// answers with a token must be able to tell it from a configuration fault.
func TestAnAcquireThatNeverReachedTheFrontIsStillQueued(t *testing.T) {
	m := realTimeTokenManager(t, 5*time.Second)
	m.SetCardScoped(true)
	ahead, unreg := m.registerWaiter(ClassSeat, Options{Reason: "seat admission"})
	defer unreg()
	if ahead.path == "" {
		t.Fatal("waiter registration failed")
	}
	stop := make(chan struct{})
	go func() { // a live waiter proves it is polling
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				m.refreshWaiter(ahead)
			}
		}
	}()
	defer close(stop)
	// "Ahead" means an earlier arrival stamp. Two registrations in the same millisecond tie, and
	// the tie is broken by a random file name, so without this pause the seat waiter was ahead of
	// the call only about two runs in three (measured: 9 failures in 30 runs of this test alone).
	time.Sleep(5 * time.Millisecond)

	_, err := m.Acquire(ClassMedia, Options{Devices: []string{"gpu-aaaa"}, TTL: time.Hour, Wait: 150 * time.Millisecond, WaitOut: true})
	if !errors.Is(err, ErrStillQueued) {
		t.Fatalf("err = %v, want ErrStillQueued", err)
	}
	if !strings.Contains(err.Error(), "seat admission") {
		t.Errorf("the error should name who is ahead: %v", err)
	}
}

func TestAnAcquireBehindOnlyALiveTokenIsStillQueuedAndNamesIt(t *testing.T) {
	m := realTimeTokenManager(t, 5*time.Second)
	m.SetCardScoped(true)
	tok, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}, Reason: "image-gen"}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Acquire(ClassMedia, Options{Devices: []string{"gpu-aaaa"}, TTL: time.Hour, Wait: 150 * time.Millisecond, WaitOut: true})
	if !errors.Is(err, ErrStillQueued) {
		t.Fatalf("err = %v, want ErrStillQueued", err)
	}
	if !strings.Contains(err.Error(), tok.ID) {
		t.Errorf("the error should name the place held ahead (%s): %v", tok.ID, err)
	}
}

// A ReadOnly view reads the line the way every reader does and writes nothing: a record the ordinary readers prune
// (an expired token, a token that is not one, a waiter whose process is gone, a waiter that stopped polling) is skipped
// and LEFT on disk. The view is what the media lane probe asks its question through, so asking never changes the line.
func TestAReadOnlyViewSkipsDeadRecordsAndLeavesThemOnDisk(t *testing.T) {
	m, now := tokenManager(t)
	expired, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(TokenTTL + time.Second)
	fresh, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-bbbb"}}, *now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.tokensDir(), "garbage.json"), []byte("{"), 0o666); err != nil {
		t.Fatal(err)
	}
	wdir := m.waitersDir()
	if err := os.MkdirAll(wdir, 0o777); err != nil {
		t.Fatal(err)
	}
	dead, _ := json.Marshal(Waiter{PID: 2147483000, Class: ClassText, SinceMs: now.UnixMilli()})
	if err := os.WriteFile(filepath.Join(wdir, "2147483000.1.json"), dead, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "garbage.json"), []byte("{"), 0o666); err != nil {
		t.Fatal(err)
	}
	live, _ := json.Marshal(Waiter{PID: os.Getpid(), Class: ClassMedia, SinceMs: now.UnixMilli()})
	livePath := filepath.Join(wdir, "live.json")
	if err := os.WriteFile(livePath, live, 0o666); err != nil {
		t.Fatal(err)
	}
	// A live pid whose record stopped being re-stamped: the heartbeat check prunes it for an ordinary reader.
	stale, _ := json.Marshal(Waiter{PID: os.Getpid(), Class: ClassMedia, SinceMs: now.UnixMilli()})
	stalePath := filepath.Join(wdir, "stale.json")
	if err := os.WriteFile(stalePath, stale, 0o666); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour) // the Manager's clock is the test's, not the wall clock
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}
	files := func() string {
		var names []string
		for _, d := range []string{m.tokensDir(), wdir} {
			entries, _ := os.ReadDir(d)
			for _, e := range entries {
				names = append(names, filepath.Base(d)+"/"+e.Name())
			}
		}
		sort.Strings(names)
		return strings.Join(names, " ")
	}
	before := files()

	v := m.ReadOnly()
	if _, ok := v.ResumeToken(expired.ID); ok {
		t.Error("through the view an expired token still does not resume")
	}
	if got := v.Tokens(); len(got) != 1 || got[0].ID != fresh.ID {
		t.Errorf("through the view only the live token is listed, got %+v", got)
	}
	ws := v.Waiters()
	if len(ws) != 1 || ws[0].PID != os.Getpid() || filepath.Base(ws[0].path) != "live.json" {
		t.Errorf("through the view only the live waiter is listed, got %+v", ws)
	}
	if after := files(); after != before {
		t.Fatalf("the view wrote: the line held [%s] before the reads and [%s] after", before, after)
	}

	// The ordinary reader of the same root still does its housekeeping: the view changed nothing about it.
	if _, ok := m.ResumeToken(expired.ID); ok {
		t.Error("an expired token does not resume")
	}
	_ = m.Tokens()
	_ = m.Waiters()
	if after := files(); after == before {
		t.Errorf("the ordinary readers must still prune what they find dead, the line is unchanged: %s", after)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("the live waiter must survive the housekeeping: %v", err)
	}
}
