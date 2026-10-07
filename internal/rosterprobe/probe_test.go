package rosterprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// Timing in these tests is never a measurement. A probe bound is a ceiling a node is held to, so a loaded
// runner can only make a bound fire LATE, never early; what the tests assert is therefore counted (dials,
// sources, which caller got which answer), the windows are driven by a fake clock on the test's own cache
// (no package variable is touched, so nothing needs restoring and no test can leak into another), and a
// node that must be "slow" waits on a gate the test opens instead of sleeping for a guessed time.

// healthNode is a fleet node that answers /fleet/health, counting how many times it was asked.
type healthNode struct {
	srv    *httptest.Server
	hits   atomic.Int64
	status int           // 0 = 200
	gate   chan struct{} // when non-nil an answer waits for it to be closed, or for the caller to leave
}

func newHealthNode(t *testing.T, id string, gate chan struct{}, status int) *healthNode {
	t.Helper()
	n := &healthNode{gate: gate, status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		n.hits.Add(1)
		if n.gate != nil {
			select {
			case <-n.gate:
			case <-r.Context().Done():
				return
			}
		}
		if n.status != 0 {
			w.WriteHeader(n.status)
			_, _ = w.Write([]byte(`{"error":"not today"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"node_id": id, "supported_task_types": []string{"vision"}, "accelerators": []string{"dev-" + id}})
	})
	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

func (n *healthNode) url() string { return n.srv.URL }

// newGate returns a closed-on-demand gate; it is opened when the test ends too, so a held handler never
// outlives the test.
func newGate(t *testing.T) (gate chan struct{}, open func()) {
	t.Helper()
	gate = make(chan struct{})
	var once sync.Once
	open = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	return gate, open
}

// fakeClock is the time a test's cache reads.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// newTestCache is a cache with its own windows and a fake clock. The transient cap is off (equal to the
// negative window) so a test that is not about the cap sees one window; the cap's own tests set it.
func newTestCache(memo, negative time.Duration) (*Cache, *fakeClock) {
	c, clk := NewCache(), newFakeClock()
	c.now = clk.Now
	c.memoTTL, c.negativeTTL, c.transientTTL = memo, negative, negative
	return c, clk
}

// describe prints what matters about a reading when a test fails, not the whole NodeView.
func describe(r Reading) string {
	return fmt.Sprintf("{base %s, source %q, err %v, node %q}", r.Base, r.Source, r.Err, r.View.NodeID)
}

// waitFor polls cond until it holds, failing the test (not hanging it) if it does not within a generous limit.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (c *Cache) deadFor(base string) (deadEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.dead[base]
	return d, ok
}

// k dead members used to cost k probe bounds in series on the critical path of every call. Probed at once,
// the roster costs the slowest member. That is proved without a stopwatch: every member's answer waits for
// ALL members to have been asked, so a prober that went one at a time would never get an answer and every
// member would time out.
func TestProbeAsksEveryMemberAtOnce(t *testing.T) {
	const members = 4
	var arrived atomic.Int64
	allAsked := make(chan struct{})
	var once sync.Once
	var remotes []string
	for i := 0; i < members; i++ {
		id := fmt.Sprintf("node-%d", i)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if arrived.Add(1) == members {
				once.Do(func() { close(allAsked) })
			}
			select {
			case <-allAsked:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"node_id": id})
		}))
		t.Cleanup(srv.Close)
		remotes = append(remotes, srv.URL)
	}

	got := NewCache().Probe(context.Background(), remotes, "", 5*time.Second)
	if len(got) != members {
		t.Fatalf("got %d readings, want %d", len(got), members)
	}
	for i, r := range got {
		if r.Err != nil || r.View.NodeID != fmt.Sprintf("node-%d", i) {
			t.Errorf("reading %d = %s: members must be asked at once, and answered in configured order", i, describe(r))
		}
	}
}

// Hung members are dialled once each and come back in the order the roster is written, whichever gives up first.
func TestProbeDialsEachHungMemberOnceAndKeepsConfiguredOrder(t *testing.T) {
	holes := []*rostertest.Hole{rostertest.NewBlackHole(t), rostertest.NewBlackHole(t), rostertest.NewBlackHole(t)}
	remotes := []string{holes[0].URL(), "", holes[1].URL(), holes[2].URL()}
	c, _ := newTestCache(0, 0)

	got := c.Probe(context.Background(), remotes, "", 150*time.Millisecond)
	if len(got) != 3 {
		t.Fatalf("got %d readings, want 3 (the blank slot is not a member)", len(got))
	}
	wantIndex := []int{0, 2, 3}
	for i, h := range holes {
		if got[i].Err == nil || got[i].Base != h.URL() || got[i].Index != wantIndex[i] {
			t.Errorf("reading %d = %s (slot %d), want the hung member's failure in slot %d", i, describe(got[i]), got[i].Index, wantIndex[i])
		}
		if h.Dials() != 1 {
			t.Errorf("hung member %d was dialled %d times, want once", i, h.Dials())
		}
	}
}

// Completion order is not an ordering any caller can use: the accelerator lane takes the FIRST listing node
// and the compose lane breaks ties by slot, so the readings come back in configured order even when the first
// member answers last.
func TestProbeReturnsConfiguredOrderWhateverTheCompletionOrder(t *testing.T) {
	gate, open := newGate(t)
	slow := newHealthNode(t, "slow", gate, 0)
	fast := newHealthNode(t, "fast", nil, 0)
	c, _ := newTestCache(0, 0)
	done := make(chan []Reading, 1)
	go func() {
		done <- c.Probe(context.Background(), []string{slow.url(), "", fast.url()}, "", 10*time.Second)
	}()
	waitFor(t, "both members to have been asked", func() bool { return slow.hits.Load() == 1 && fast.hits.Load() == 1 })
	open() // the first member answers only after the second has

	got := <-done
	if len(got) != 2 || got[0].View.NodeID != "slow" || got[1].View.NodeID != "fast" {
		t.Fatalf("readings = %s | %s, want slow then fast", describe(got[0]), describe(got[len(got)-1]))
	}
	if got[0].Index != 0 || got[1].Index != 2 {
		t.Errorf("slot numbers = %d, %d, want 0 and 2 (the blank slot counts)", got[0].Index, got[1].Index)
	}
}

// A node that failed at the transport is not dialled again inside the window: the second call costs nothing,
// replays the reason with how stale it is, and says where the answer came from.
func TestANegativeCacheStopsRedialingAHungNode(t *testing.T) {
	hole := rostertest.NewBlackHole(t)
	c, clk := newTestCache(0, time.Minute)

	first := c.Probe(context.Background(), []string{hole.URL()}, "", 150*time.Millisecond)
	if first[0].Err == nil || first[0].Source != SourceProbe {
		t.Fatalf("first reading = %s, want a dialled failure", describe(first[0]))
	}
	clk.Advance(20 * time.Second)
	second := c.Probe(context.Background(), []string{hole.URL()}, "", 150*time.Millisecond)
	if hole.Dials() != 1 {
		t.Errorf("the hung node was dialled %d times across two calls, want once", hole.Dials())
	}
	if second[0].Source != SourceNegative || second[0].Err == nil {
		t.Fatalf("second reading = %s, want the cached failure", describe(second[0]))
	}
	for _, want := range []string{"cached 20s ago", "re-dial in 40s"} {
		if !strings.Contains(second[0].Err.Error(), want) {
			t.Errorf("replayed reason %q must say %q", second[0].Err, want)
		}
	}
	if !strings.Contains(second[0].Miss(), hole.URL()) {
		t.Errorf("miss %q must still name the node", second[0].Miss())
	}
}

// The window is a window: once it has passed the node is dialled again, so one that rebooted is picked up.
func TestTheNegativeCacheExpires(t *testing.T) {
	hole := rostertest.NewBlackHole(t)
	c, clk := newTestCache(0, 30*time.Second)
	probe := func() Reading {
		return c.Probe(context.Background(), []string{hole.URL()}, "", 100*time.Millisecond)[0]
	}

	_ = probe()
	clk.Advance(29 * time.Second)
	if r := probe(); r.Source != SourceNegative {
		t.Fatalf("inside the window the failure must be replayed, got %s", describe(r))
	}
	clk.Advance(2 * time.Second)
	if r := probe(); r.Source != SourceProbe {
		t.Fatalf("after the window the node must be dialled again, got %s", describe(r))
	}
	if hole.Dials() != 2 {
		t.Errorf("dials = %d, want 2 (one per window)", hole.Dials())
	}
}

// What a busy or restarting node shows (it did not answer in time, or the dial was refused or found no route)
// is held out at most TransientNegativeTTL, not the whole negative window: a box that comes back inside half a
// minute is back in rotation within seconds. A failure that is not that kind (a name that does not resolve, a
// fault above the dial) keeps the full window.
func TestATransientFailureIsHeldOutAtMostTheTransientWindow(t *testing.T) {
	hole := rostertest.NewBlackHole(t)
	c := NewCache() // the production windows: 30 s negative, 5 s transient cap
	clk := newFakeClock()
	c.now = clk.Now
	if c.negativeTTL != NegativeTTL || c.transientTTL != TransientNegativeTTL || TransientNegativeTTL >= NegativeTTL {
		t.Fatalf("precondition: windows = %s / %s", c.negativeTTL, c.transientTTL)
	}

	// A timeout.
	probe := func() Reading {
		return c.Probe(context.Background(), []string{hole.URL()}, "", 100*time.Millisecond)[0]
	}
	if r := probe(); r.Err == nil || r.Source != SourceProbe {
		t.Fatalf("precondition: a black hole must time out, got %s", describe(r))
	}
	clk.Advance(4 * time.Second)
	if r := probe(); r.Source != SourceNegative {
		t.Fatalf("4 s in, a timeout must still be replayed, got %s", describe(r))
	}
	clk.Advance(2 * time.Second) // 6 s: past the transient window, well inside the 30 s one
	if r := probe(); r.Source != SourceProbe || hole.Dials() != 2 {
		t.Fatalf("6 s in, a timeout must be dialled again (dials %d), got %s", hole.Dials(), describe(r))
	}

	// A refused dial, off a real closed port.
	closed := "http://" + closedBase(t)
	if r := c.Probe(context.Background(), []string{closed}, "", time.Second)[0]; r.Err == nil || r.Source != SourceProbe {
		t.Fatalf("precondition: a closed port must refuse, got %s", describe(r))
	}
	if d, ok := c.deadFor(closed); !ok || d.ttl != TransientNegativeTTL {
		t.Errorf("a refused dial is held for %s (entry %+v), want the transient window %s", d.ttl, d, TransientNegativeTTL)
	}
	clk.Advance(6 * time.Second)
	if r := c.Probe(context.Background(), []string{closed}, "", time.Second)[0]; r.Source != SourceProbe {
		t.Errorf("6 s after a refusal the node must be dialled again, got %s", describe(r))
	}
}

// The classification the cap rests on, by error type (Windows does not report ECONNREFUSED as a syscall errno).
func TestTransientClassifiesByErrorType(t *testing.T) {
	get := func(inner error) error {
		return &url.Error{Op: "Get", URL: "http://node-a:18811/fleet/health", Err: inner}
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"deadline exceeded": {get(context.DeadlineExceeded), true},
		"i/o timeout":       {get(&net.OpError{Op: "read", Err: timeoutErr{}}), true},
		"dial refused":      {get(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connectex: connection refused")}), true},
		"dial unroutable":   {get(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: no route to host")}), true},
		"name not found":    {get(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "node-a", IsNotFound: true}}), false},
		"bare dns error":    {get(&net.DNSError{Err: "no such host", Name: "node-a", IsNotFound: true}), false},
		"tls fault":         {get(errors.New("tls: failed to verify certificate")), false},
		"reset after dial":  {get(&net.OpError{Op: "read", Err: errors.New("connection reset by peer")}), false},
	} {
		if got := transient(tc.err); got != tc.want {
			t.Errorf("%s: transient = %v, want %v", name, got, tc.want)
		}
	}

	// And the entry that gets stored carries the window the class earns.
	c, _ := newTestCache(0, 30*time.Second)
	c.transientTTL = 5 * time.Second
	ctx := context.Background()
	c.noteDead(ctx, "http://refused:1", get(&net.OpError{Op: "dial", Err: errors.New("connection refused")}), time.Second)
	c.noteDead(ctx, "http://nodns:1", get(&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}), time.Second)
	if d, _ := c.deadFor("http://refused:1"); d.ttl != 5*time.Second {
		t.Errorf("refused dial window = %s, want 5s", d.ttl)
	}
	if d, _ := c.deadFor("http://nodns:1"); d.ttl != 30*time.Second {
		t.Errorf("name-not-found window = %s, want the full 30s", d.ttl)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// A call that reached a node by another road (a dispatch that was accepted) tells the cache so, and the
// verdict "unreachable" held against that node is dropped at once instead of being replayed to the next call.
func TestForgetDropsTheNegativeVerdictHeldAgainstANode(t *testing.T) {
	live := newHealthNode(t, "node-b", nil, 0)
	c, _ := newTestCache(0, time.Minute)
	c.noteDead(context.Background(), live.url(), &url.Error{Op: "Get", URL: live.url(), Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, time.Second)

	if r := c.Probe(context.Background(), []string{live.url()}, "", time.Second)[0]; r.Source != SourceNegative || live.hits.Load() != 0 {
		t.Fatalf("precondition: the node is held out without a dial, got %s (hits %d)", describe(r), live.hits.Load())
	}
	c.Forget(live.url() + "/") // as a lane would hand it, with a stray slash
	r := c.Probe(context.Background(), []string{live.url()}, "", time.Second)[0]
	if r.Err != nil || r.Source != SourceProbe || live.hits.Load() != 1 {
		t.Fatalf("after Forget the node must be dialled and answer, got %s (hits %d)", describe(r), live.hits.Load())
	}
	// Forget touches only the verdict held against that node.
	other := "http://other:1"
	c.noteDead(context.Background(), other, &url.Error{Op: "Get", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}, time.Second)
	c.Forget(live.url())
	if _, ok := c.deadFor(other); !ok {
		t.Error("Forget removed another node's verdict")
	}
}

// A probe that gets an answer clears the verdict the same way, so a node that came back is not held out by an
// entry that has not yet aged out (here one observed under a 50 ms timeout, which a patient caller ignores).
func TestAProbeThatSucceedsClearsTheNegativeVerdict(t *testing.T) {
	live := newHealthNode(t, "node-b", nil, 0)
	c, clk := newTestCache(0, time.Minute)
	c.mu.Lock()
	c.dead[live.url()] = deadEntry{at: clk.Now(), why: "dial tcp: i/o timeout", ttl: time.Minute, bound: 50 * time.Millisecond}
	c.mu.Unlock()

	if r := c.Probe(context.Background(), []string{live.url()}, "", 10*time.Second)[0]; r.Err != nil || r.Source != SourceProbe {
		t.Fatalf("a patient caller must dial and answer, got %s", describe(r))
	}
	if _, ok := c.deadFor(live.url()); ok {
		t.Error("a successful probe left the negative verdict in place")
	}
}

// A timeout is a statement about the bound it was observed under: a caller willing to wait LONGER than the
// probe that failed has not been shown to fail, so the accelerator lane's 2 s failure is not held against a
// vision call that waits 5 s. A refusal (nothing listening) holds for everyone.
func TestANegativeEntryIsNotHeldAgainstACallerWillingToWaitLonger(t *testing.T) {
	gate, open := newGate(t)
	slow := newHealthNode(t, "slow", gate, 0)
	c, _ := newTestCache(0, time.Minute)

	short := c.Probe(context.Background(), []string{slow.url()}, "", 50*time.Millisecond)
	if short[0].Err == nil {
		t.Fatal("precondition: a 50 ms bound must time out a node that has not answered")
	}
	open() // the node answers from now on
	patient := c.Probe(context.Background(), []string{slow.url()}, "", 10*time.Second)
	if patient[0].Err != nil || patient[0].Source != SourceProbe {
		t.Fatalf("a caller willing to wait longer was turned away by a 50 ms failure: %s", describe(patient[0]))
	}

	// A closed port refuses at once: that holds whatever the caller would have waited.
	closed := "http://" + closedBase(t)
	_ = c.Probe(context.Background(), []string{closed}, "", 100*time.Millisecond)
	again := c.Probe(context.Background(), []string{closed}, "", time.Hour)
	if again[0].Source != SourceNegative {
		t.Fatalf("a refusal must hold for any bound, got %s", describe(again[0]))
	}
}

// A good answer is shared for the memo window by everything in the process; a zero window turns it off.
func TestAGoodAnswerIsMemoisedForTheTTL(t *testing.T) {
	n := newHealthNode(t, "node-b", nil, 0)
	c, clk := newTestCache(2*time.Second, 0)
	probe := func() Reading { return c.Probe(context.Background(), []string{n.url()}, "", 10*time.Second)[0] }

	if r := probe(); r.Err != nil || r.Source != SourceProbe {
		t.Fatalf("first read = %s", describe(r))
	}
	clk.Advance(time.Second)
	if r := probe(); r.Err != nil || r.Source != SourceMemo {
		t.Fatalf("second read = %s, want the memo", describe(r))
	}
	if n.hits.Load() != 1 {
		t.Errorf("node asked %d times for two reads inside the window, want 1", n.hits.Load())
	}
	clk.Advance(2 * time.Second)
	if r := probe(); r.Source != SourceProbe {
		t.Fatalf("after the window the node must be read again, got %s", describe(r))
	}

	off, _ := newTestCache(0, 0)
	for i := 0; i < 2; i++ {
		_ = off.Probe(context.Background(), []string{n.url()}, "", 10*time.Second)
	}
	if n.hits.Load() != 4 {
		t.Errorf("with the memo off every read is a probe: %d hits, want 4", n.hits.Load())
	}
}

// A node that ANSWERED is never negative-cached, whatever it answered: a 503 is a node that may well answer the
// next call, and skipping it for half a minute would turn a momentary refusal into ineligibility.
func TestANodeThatAnsweredIsNeverNegativeCached(t *testing.T) {
	n := newHealthNode(t, "node-b", nil, http.StatusServiceUnavailable)
	c, _ := newTestCache(0, time.Minute)
	for i := 0; i < 2; i++ {
		r := c.Probe(context.Background(), []string{n.url()}, "", 10*time.Second)[0]
		if r.Err == nil || !strings.Contains(r.Err.Error(), "503") || r.Source != SourceProbe {
			t.Fatalf("read %d = %s, want a dialled 503", i, describe(r))
		}
	}
	if n.hits.Load() != 2 {
		t.Errorf("node asked %d times, want 2 (an answer is never cached as a failure)", n.hits.Load())
	}
}

// Nothing is cached once the CALLER's context is done: a cancelled call fails every probe at the same instant,
// and that is a fact about the caller, not about any node.
func TestACancelledCallCachesNothing(t *testing.T) {
	hole := rostertest.NewBlackHole(t)
	c, _ := newTestCache(0, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	r := c.Probe(ctx, []string{hole.URL()}, "", time.Hour)[0]
	cancel()
	if r.Err == nil {
		t.Fatal("precondition: the caller's deadline must fail the probe")
	}
	second := c.Probe(context.Background(), []string{hole.URL()}, "", 100*time.Millisecond)[0]
	if second.Source != SourceProbe {
		t.Fatalf("a cancelled call left a negative entry behind: %s", describe(second))
	}
	if hole.Dials() != 2 {
		t.Errorf("dials = %d, want 2 (the cancelled call's failure must not have been cached)", hole.Dials())
	}
}

// A refused member is returned flagged and never dialled, and its miss says so.
func TestProbeNeverDialsARefusedMember(t *testing.T) {
	rostertest.Zones(t)
	live := newHealthNode(t, "node-b", nil, 0)
	got := NewCache().Probe(context.Background(), []string{"http://node-x." + otherZone + ":18811", live.url()}, "", 10*time.Second)
	if len(got) != 2 || got[0].Refused == nil || got[0].Err != got[0].Refused || got[0].Source != SourceProbe {
		t.Fatalf("refused reading = %s: a refused member's error must be the refusal itself, never a dial result", describe(got[0]))
	}
	if m := got[0].Miss(); !strings.Contains(m, "not dialled") || !strings.Contains(m, got[0].Base) {
		t.Errorf("miss %q must say the entry was not dialled and name it", m)
	}
	if got[1].Err != nil {
		t.Errorf("the entry after a refused one still serves: %v", got[1].Err)
	}
}

// Reset forgets both caches.
func TestResetForgetsBothCaches(t *testing.T) {
	n := newHealthNode(t, "node-b", nil, 0)
	hole := rostertest.NewBlackHole(t)
	c, _ := newTestCache(time.Minute, time.Minute)
	_ = c.Probe(context.Background(), []string{n.url()}, "", 10*time.Second)
	_ = c.Probe(context.Background(), []string{hole.URL()}, "", 100*time.Millisecond)
	c.Reset()
	live := c.Probe(context.Background(), []string{n.url()}, "", 10*time.Second)[0]
	dead := c.Probe(context.Background(), []string{hole.URL()}, "", 100*time.Millisecond)[0]
	if live.Source != SourceProbe || dead.Source != SourceProbe {
		t.Fatalf("after Reset both members must be dialled again: %s | %s", describe(live), describe(dead))
	}
}

// The lanes of one process call Probe at once; the cache must be safe under that (run with -race).
func TestProbeIsSafeForConcurrentCallers(t *testing.T) {
	n := newHealthNode(t, "node-b", nil, 0)
	closed := "http://" + closedBase(t)
	c, clk := newTestCache(50*time.Millisecond, 50*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%4 == 0 {
				clk.Advance(30 * time.Millisecond) // windows open and close while callers are in flight
			}
			got := c.Probe(context.Background(), []string{n.url(), closed}, "", 10*time.Second)
			if len(got) != 2 || got[0].Err != nil || got[1].Err == nil {
				t.Errorf("readings = %s | %s", describe(got[0]), describe(got[len(got)-1]))
			}
		}(i)
	}
	wg.Wait()
}

// A burst of callers on a cold cache costs the node ONE request: the first caller probes, the rest wait for
// its answer. The memo and the negative cache are off, so nothing but the in-flight sharing can explain a
// single hit, and the node holds its answer until every other caller is provably waiting.
func TestConcurrentColdProbesOfOneNodeShareOneDial(t *testing.T) {
	const callers = 16
	gate, open := newGate(t)
	n := newHealthNode(t, "node-b", gate, 0)
	c, _ := newTestCache(0, 0)
	var joined atomic.Int64
	c.onJoin = func() { joined.Add(1) }

	results := make(chan Reading, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- c.Probe(context.Background(), []string{n.url()}, "", 30*time.Second)[0] }()
	}
	waitFor(t, "the other callers to be waiting on the one probe", func() bool { return joined.Load() == callers-1 })
	open()
	for i := 0; i < callers; i++ {
		if r := <-results; r.Err != nil || r.View.NodeID != "node-b" {
			t.Errorf("caller %d got %s, want the shared answer", i, describe(r))
		}
	}
	if n.hits.Load() != 1 {
		t.Errorf("the node was asked %d times for %d concurrent callers, want 1", n.hits.Load(), callers)
	}
}

// A shared failure is shared too: callers joining a probe that ends in an answer other than health get that
// answer, not a redial each.
func TestConcurrentColdProbesOfANodeThatAnswers503ShareOneAnswer(t *testing.T) {
	const callers = 8
	gate, open := newGate(t)
	n := newHealthNode(t, "node-b", gate, http.StatusServiceUnavailable)
	c, _ := newTestCache(0, 0)
	var joined atomic.Int64
	c.onJoin = func() { joined.Add(1) }

	results := make(chan Reading, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- c.Probe(context.Background(), []string{n.url()}, "", 30*time.Second)[0] }()
	}
	waitFor(t, "the other callers to be waiting on the one probe", func() bool { return joined.Load() == callers-1 })
	open()
	for i := 0; i < callers; i++ {
		if r := <-results; r.Err == nil || !strings.Contains(r.Err.Error(), "503") {
			t.Errorf("caller %d got %s, want the shared 503", i, describe(r))
		}
	}
	if n.hits.Load() != 1 {
		t.Errorf("the node was asked %d times for %d concurrent callers, want 1", n.hits.Load(), callers)
	}
}

// A caller that joined a probe is not held hostage by it: its own context ends its wait, and the result is its
// own context's error, not whatever the prober eventually gets.
func TestAJoinerWaitsOnItsOwnContext(t *testing.T) {
	gate, open := newGate(t)
	n := newHealthNode(t, "node-b", gate, 0)
	c, _ := newTestCache(0, 0)
	var joined atomic.Int64
	c.onJoin = func() { joined.Add(1) }

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_ = c.Probe(context.Background(), []string{n.url()}, "", 20*time.Second)
	}()
	waitFor(t, "the first caller's probe to reach the node", func() bool { return n.hits.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	joinerDone := make(chan Reading, 1)
	go func() { joinerDone <- c.Probe(ctx, []string{n.url()}, "", 20*time.Second)[0] }()
	select {
	case r := <-joinerDone:
		if !errors.Is(r.Err, context.DeadlineExceeded) {
			t.Errorf("the joiner's error = %v, want its own deadline", r.Err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the joiner is still waiting on someone else's probe long after its own deadline")
	}
	if joined.Load() != 1 {
		t.Errorf("joined = %d, want the joiner to have waited on the existing probe", joined.Load())
	}
	open() // release the held probe so the first caller can finish
	<-leaderDone
}

// The caller that is probing can leave (its context ends) while others wait on its probe. That is a fact about
// the caller, not the node: a waiter whose context is alive asks again and gets the node's real answer, instead
// of inheriting "context canceled".
func TestAJoinerDoesNotInheritTheProbersCancellation(t *testing.T) {
	gate, open := newGate(t)
	n := newHealthNode(t, "node-b", gate, 0)
	c, _ := newTestCache(0, 0)
	var joined atomic.Int64
	c.onJoin = func() { joined.Add(1) }

	leaderCtx, leaderLeaves := context.WithCancel(context.Background())
	leaderDone := make(chan Reading, 1)
	go func() { leaderDone <- c.Probe(leaderCtx, []string{n.url()}, "", 20*time.Second)[0] }()
	waitFor(t, "the prober's request to reach the node", func() bool { return n.hits.Load() == 1 })

	joinerDone := make(chan Reading, 1)
	go func() { joinerDone <- c.Probe(context.Background(), []string{n.url()}, "", 20*time.Second)[0] }()
	waitFor(t, "the second caller to be waiting on the probe", func() bool { return joined.Load() == 1 })

	leaderLeaves()
	open() // from here the node answers
	<-leaderDone
	r := <-joinerDone
	if r.Err != nil || r.View.NodeID != "node-b" {
		t.Fatalf("the joiner got %s, want the node's answer (not the prober's cancellation)", describe(r))
	}
	if n.hits.Load() != 2 {
		t.Errorf("node asked %d times, want 2 (the cancelled probe, then the joiner's own)", n.hits.Load())
	}
}

// Callers with different bounds do not share a probe: a caller willing to wait 20 s must not be handed the
// timeout a 600 ms caller observed.
func TestCallersWithDifferentBoundsDoNotShareAProbe(t *testing.T) {
	gate, open := newGate(t)
	n := newHealthNode(t, "node-b", gate, 0)
	c, _ := newTestCache(0, 0)

	impatient := make(chan Reading, 1)
	go func() { impatient <- c.Probe(context.Background(), []string{n.url()}, "", 600*time.Millisecond)[0] }()
	patient := make(chan Reading, 1)
	waitFor(t, "the impatient probe to reach the node", func() bool { return n.hits.Load() == 1 })
	go func() { patient <- c.Probe(context.Background(), []string{n.url()}, "", 20*time.Second)[0] }()
	waitFor(t, "the patient caller to dial for itself", func() bool { return n.hits.Load() == 2 })

	if r := <-impatient; r.Err == nil {
		t.Fatalf("the 600 ms caller should have timed out, got %s", describe(r))
	}
	open()
	if r := <-patient; r.Err != nil {
		t.Fatalf("the patient caller inherited the impatient one's failure: %s", describe(r))
	}
}
