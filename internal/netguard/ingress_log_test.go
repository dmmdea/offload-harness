package netguard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The warn log exists so the operator can run warn until it shows no legitimate client
// tripping the rule, then promote to enforce. A log that goes blind defeats that: the dedupe set
// was never evicted and any peer could fill it, after which a legitimate client's violation was
// only counted. These tests pin the four ways it stays useful: the dedupe set ages out, one peer
// cannot take the whole budget, a pending overflow count is written without another request,
// and two different long values are two offenders.

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)} }
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testLog builds an offenderLog on a fake clock. summaryEvery is an hour unless the test
// overrides it, so a test that does not wait for a summary never arms a timer that fires.
func testLog(t *testing.T, mutate func(*offenderLogConfig)) (*offenderLog, *logSink, *fakeClock) {
	t.Helper()
	sink := &logSink{}
	clk := newFakeClock()
	cfg := offenderLogConfig{
		key: "fleet_ingress_guard", logf: sink.logf, now: clk.now,
		maxOffenders: 3, maxPerPeer: 1000, window: 5 * time.Minute, summaryEvery: time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return newOffenderLog(cfg), sink, clk
}

// countLines counts the log lines that BEGIN with prefix: a summary line quotes the latest
// offender it counted, so a substring match would count it as an individual line.
func countLines(sink *logSink, prefix string) int {
	n := 0
	for _, l := range strings.Split(sink.all(), "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// A full dedupe set is emptied when its window ends, so a legitimate client that trips the rule
// after a flood is logged in the next window instead of being counted forever, and an offender
// that is still tripping the rule is written again once per window.
func TestOffenderLogDedupeWindowAgesOut(t *testing.T) {
	l, sink, clk := testLog(t, nil)
	for _, o := range []string{"a", "b", "c"} {
		l.once("Host", "198.51.100.9", o, "offender "+o)
	}
	l.once("Host", "198.51.100.9", "d", "offender d") // the set is full: counted, not logged one by one
	l.once("Host", "192.0.2.7", "late", "offender late")
	if got := countLines(sink, "offender late"); got != 0 {
		t.Fatalf("setup: with the set full, a new offender must be counted, not logged (%d lines)", got)
	}

	clk.advance(5*time.Minute + time.Second)
	l.once("Host", "192.0.2.7", "late", "offender late")
	if got := countLines(sink, "offender late"); got != 1 {
		t.Errorf("a legitimate client's violation after the window was logged %d times, want once: the dedupe set was never emptied\n%s", got, sink.all())
	}
	l.once("Host", "198.51.100.9", "a", "offender a")
	if got := countLines(sink, "offender a"); got != 2 {
		t.Errorf("an offender still tripping the rule a window later was logged %d times in all, want twice (once per window)", got)
	}
	l.once("Host", "198.51.100.9", "a", "offender a")
	if got := countLines(sink, "offender a"); got != 2 {
		t.Errorf("a repeat inside the same window was logged again (%d lines in all)", got)
	}
}

// One peer's distinct Host values cannot use up the budget the others log from: a rebinding
// page sends a unique name per request, and the legitimate client that trips the rule is
// another peer.
func TestOffenderLogOnePeerCannotTakeTheWholeBudget(t *testing.T) {
	l, sink, _ := testLog(t, func(c *offenderLogConfig) { c.maxOffenders = 20; c.maxPerPeer = 5 })
	for i := 0; i < 50; i++ {
		l.once("Host", "198.51.100.9", fmt.Sprintf("rebind%d.example", i), fmt.Sprintf("offender rebind%d", i))
	}
	if got := countLines(sink, "offender rebind"); got != 5 {
		t.Errorf("one peer's 50 distinct offenders were logged %d times, want its share of 5", got)
	}
	l.once("Host", "192.0.2.7", "node-b.other.ts.example", "offender legit")
	if got := countLines(sink, "offender legit"); got != 1 {
		t.Errorf("another peer's violation was logged %d times after the flood, want once: one peer took the whole budget\n%s", got, sink.all())
	}
}

var summaryCount = regexp.MustCompile(`(\d+) violation\(s\)`)

// A burst that ends inside the summary interval leaves a count nobody has written. It is
// written when the interval ends, without waiting for another violation that may never come.
func TestOffenderLogWritesAPendingOverflowWithoutAnotherRequest(t *testing.T) {
	sink := &logSink{}
	l := newOffenderLog(offenderLogConfig{
		key: "fleet_ingress_guard", logf: sink.logf, now: time.Now,
		maxOffenders: 2, maxPerPeer: 100, window: time.Hour, summaryEvery: 60 * time.Millisecond,
	})
	for i := 0; i < 7; i++ {
		l.once("Host", "198.51.100.9", fmt.Sprintf("h%d.example", i), fmt.Sprintf("offender h%d", i))
	}
	// 2 logged one by one, 5 counted. No more calls: only the log may write the rest.
	counted := func() int {
		n := 0
		for _, m := range summaryCount.FindAllStringSubmatch(sink.all(), -1) {
			v, _ := strconv.Atoi(m[1])
			n += v
		}
		return n
	}
	deadline := time.Now().Add(2 * time.Second)
	for counted() < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := counted(); got != 5 {
		t.Errorf("summaries account for %d of the 5 counted violations after the interval, want all 5 written without another request\n%s", got, sink.all())
	}
}

// Two different Hosts that share their first 200 bytes and their length are two offenders: the
// dedupe key is the whole value, not the clipped one, so neither is silently dropped.
func TestOffenderLogKeyIsTheWholeValue(t *testing.T) {
	l, sink, _ := testLog(t, func(c *offenderLogConfig) { c.maxOffenders = 100 })
	prefix := strings.Repeat("a", 300)
	first, second := prefix+"-one.example", prefix+"-two.example"
	if len(first) != len(second) {
		t.Fatal("test setup: the two values must have the same length")
	}
	l.once("Host", "192.0.2.7", first, "offender first")
	l.once("Host", "192.0.2.7", second, "offender second")
	if countLines(sink, "offender first") != 1 || countLines(sink, "offender second") != 1 {
		t.Errorf("two different long Hosts sharing a 200-byte prefix and a length were logged as %d line(s), want 2:\n%s", countLines(sink, "offender "), sink.all())
	}
}

// The whole guard, with the real budget: a rebinding page that varies the Host on every request
// from one peer cannot stop the log from showing another peer's violation.
func TestIngressGuardWarnStaysUsefulAfterAFlood(t *testing.T) {
	sink := &logSink{}
	h := IngressGuard(IngressOptions{Mode: "warn", Key: "fleet_ingress_guard", Logf: sink.logf}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	send := func(peer, host string) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.RemoteAddr = peer + ":4000"
		r.Host = host
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	for i := 0; i < maxLoggedOffenders+10; i++ {
		send("198.51.100.9", fmt.Sprintf("rebind%d.attacker.example", i))
	}
	send("192.0.2.7", "node-b.other.example")
	individual := 0
	for _, l := range strings.Split(sink.all(), "\n") {
		if strings.HasPrefix(l, "ingress guard (fleet_ingress_guard=warn): would refuse") && strings.Contains(l, "node-b.other.example") {
			individual++
		}
	}
	if got := individual; got != 1 {
		t.Errorf("a legitimate client's violation after %d distinct Hosts from one peer was logged %d times, want once", maxLoggedOffenders+10, got)
	}
}

// The guard runs its log on the package budget and the real clock. The windows cannot be waited
// out in a test, so this pins what the guard hands the log: every bound is set. A zero window
// would never age the set out, a zero per-peer share would log nothing.
func TestGuardLogConfigSetsEveryBound(t *testing.T) {
	c := guardLogConfig("fleet_ingress_guard", func(string, ...any) {})
	if c.key != "fleet_ingress_guard" || c.logf == nil || c.now == nil {
		t.Errorf("the guard's log config lacks its key, sink or clock: %+v", c)
	}
	if c.maxOffenders != maxLoggedOffenders || c.maxPerPeer != maxLoggedPerPeer || c.maxPerPeer <= 0 || c.maxPerPeer >= c.maxOffenders {
		t.Errorf("offender budget %d, per peer %d: want the package constants, with a peer's share a part of the whole", c.maxOffenders, c.maxPerPeer)
	}
	if c.window != dedupeWindow || c.window <= 0 || c.summaryEvery != overflowSummaryEvery || c.summaryEvery <= 0 || c.summaryEvery >= c.window {
		t.Errorf("window %s, summary every %s: want the package constants, both positive, the summary more often than the window", c.window, c.summaryEvery)
	}
}
