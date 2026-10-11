package pipeline

// F55: the lane probe read the card table once under 4 s, with the caller's context wrapped in that bound, so the retry
// gpualloc.Deps.CardTable makes for a read that ran out of time never fired (a retry is for an attempt that ran out
// while the CALLER's context was still good, and the wrapper was the caller's context): a slow nvidia-smi under load
// made the probe answer "not judged" at 4 s although the admission, with its 5 s + 15 s, would have read the table.
// And it said nothing: the lane's reason went into a verdict the router does not log, so "why did my call wait while
// the other node sat idle" had no answer anywhere. The probe now retries once under a longer bound, like the
// admission, and logs one line when it still could not read.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// shortProbeBounds makes the probe's two bounds small enough for a test to run a timeout, restoring them after.
func shortProbeBounds(t *testing.T, first, retry time.Duration) {
	t.Helper()
	oldFirst, oldRetry := laneProbeBound, laneProbeRetryBound
	laneProbeBound, laneProbeRetryBound = first, retry
	t.Cleanup(func() { laneProbeBound, laneProbeRetryBound = oldFirst, oldRetry })
}

// slowThenFast is a card-table reader whose first read hangs until its context ends (a nvidia-smi under load that does not
// answer in time) and whose later reads answer after 3x the first bound (slow, but inside the retry's longer one), honouring
// their context: a retry given the same short bound as the first read would not get its answer.
func slowThenFast(cards []gpuprobe.Card, calls *atomic.Int64) func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
	return func(ctx context.Context, _ config.Config) ([]gpuprobe.Card, string, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return nil, "", ctx.Err()
		}
		select {
		case <-time.After(3 * laneProbeBound):
			return cards, "", nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
}

func neverAnswers(calls *atomic.Int64) func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
	return func(ctx context.Context, _ config.Config) ([]gpuprobe.Card, string, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
}

func captureLogLines(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(flags) })
	return &buf
}

func linesWith(buf *bytes.Buffer, sub string) []string {
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// A read that runs out of time is read once more under the longer bound, and the lane is then judged as it would have been.
func TestTheLaneProbeReadsASlowCardTableOnceMoreBeforeGivingUp(t *testing.T) {
	A := leaseIDOf(admitUUIDA)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	hold(t, f, "a bench render", A)
	shortProbeBounds(t, 60*time.Millisecond, 5*time.Second)
	var calls atomic.Int64
	f.p.alloc.Cards = slowThenFast(f.cards, &calls)
	buf := captureLogLines(t)

	v := f.p.MediaLaneFree(context.Background(), laneReq(nil))
	if v.Unjudged || v.Free {
		t.Fatalf("the second read answers, so the lane is judged, and the card is held: %+v", v)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("one read that ran out of time and one retry, got %d reads", n)
	}
	if got := linesWith(buf, "media lane probe"); len(got) != 0 {
		t.Errorf("a probe that read the table on its retry has nothing to report: %v", got)
	}
}

// A table that still does not read leaves ONE line saying so, and the verdict is the unjudged one it always was.
func TestTheLaneProbeLogsOneLineWhenItCouldNotReadTheCardTable(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	shortProbeBounds(t, 40*time.Millisecond, 80*time.Millisecond)
	var calls atomic.Int64
	f.p.alloc.Cards = neverAnswers(&calls)
	buf := captureLogLines(t)

	v := f.p.MediaLaneFree(context.Background(), laneReq(nil))
	if !v.Free || !v.Unjudged || !strings.Contains(v.Why, "card table") {
		t.Fatalf("an unreadable table is the unjudged verdict it always was: %+v", v)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("two reads (the bound, then the retry), got %d", n)
	}
	got := linesWith(buf, "media lane probe")
	if len(got) != 1 {
		t.Fatalf("exactly one line must say the probe could not read the table, got %d:\n%s", len(got), buf.String())
	}
	for _, want := range []string{"card table could not be read", "not judged"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the line %q lacks %q", got[0], want)
		}
	}
}

// A table that reads but lists no card is no reading either, and says so the same way.
func TestTheLaneProbeTreatsAnEmptyCardTableAsUnread(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return nil, "", nil }
	buf := captureLogLines(t)
	v := f.p.MediaLaneFree(context.Background(), laneReq(nil))
	if !v.Free || !v.Unjudged || !strings.Contains(v.Why, "listed no card") {
		t.Fatalf("an empty table is unjudged, with its reason: %+v", v)
	}
	if got := linesWith(buf, "media lane probe"); len(got) != 1 {
		t.Errorf("one line: %v", got)
	}
}

// A failure that is not a timeout fails the same way again, so it is not read twice (and still leaves its line).
func TestTheLaneProbeDoesNotRetryAnErrorThatIsNotATimeout(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	var calls atomic.Int64
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		calls.Add(1)
		return nil, "", context.Canceled
	}
	buf := captureLogLines(t)
	v := f.p.MediaLaneFree(context.Background(), laneReq(nil))
	if !v.Unjudged {
		t.Fatalf("an unreadable table is unjudged: %+v", v)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("an immediate failure is not retried, got %d reads", n)
	}
	if got := linesWith(buf, "media lane probe"); len(got) != 1 {
		t.Errorf("the failure still leaves its one line, got %v", got)
	}
}

// A read that ended because the CALLER went away (a client that disconnected mid-probe) is not a fault of the card table, so it
// leaves no line saying the table "could not be read": that line is the only trace of an nvidia-smi fault and a cancelled call
// would write it on every probe. The verdict is still the not-judged one (the caller is gone, nothing acts on it).
func TestTheLaneProbeLogsNothingWhenTheCallerWentAway(t *testing.T) {
	buf := captureLogLines(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, c := range map[string]struct {
		spec admitSpec
		req  core.Request
	}{
		"image": {admitSpec{order: admitOrder, mutate: pinA}, laneReq(nil)},
		"graph": {admitSpec{order: admitOrder}, graphLaneReq(nil)},
	} {
		f := newAdmitFixtureWith(t, c.spec)
		var calls atomic.Int64
		f.p.alloc.Cards = neverAnswers(&calls)
		if v := f.p.MediaLaneFree(ctx, c.req); !v.Free || !strings.Contains(v.Why, "card table") {
			t.Errorf("%s: a probe whose caller left reads as not judged, with its reason: %+v", name, v)
		}
	}
	if got := linesWith(buf, "media lane probe"); len(got) != 0 {
		t.Errorf("a cancelled caller is not a card-table fault, but the log says so: %v", got)
	}
}

// A card table that stays unreadable fails every question the same way, and the lane question is asked once per waiting ticket on
// every scan of the ticket queue (every 10 s): a persistent fault would write a line per ticket per scan. The line is written for
// the first occurrence (and kept), then not again for the same fault of the same probe inside a minute. Another fault is another
// line at once, the other probe's question is its own line (the line names the probe), and a fault that outlasts the minute is
// said again. The verdict is the not-judged one every time.
func TestTheLaneProbeLogsAPersistentCardTableFaultOncePerMinute(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: pinA})
	cause := errors.New("nvidia-smi: exit status 9") // not a timeout: read once, no retry
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return nil, "", cause }
	now := time.Unix(1_700_000_000, 0)
	f.p.nowFn = func() time.Time { return now }
	buf := captureLogLines(t)
	ask := func(req core.Request) {
		t.Helper()
		if v := f.p.MediaLaneFree(context.Background(), req); !v.Free || !strings.Contains(v.Why, "card table") {
			t.Fatalf("an unreadable table is the not-judged verdict however often it is asked: %+v", v)
		}
	}
	lines := func() []string { return linesWith(buf, "media lane probe") }

	// A caller that went away is told nothing and tells nothing: it writes no line and does not use up the first one.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if v := f.p.MediaLaneFree(gone, laneReq(nil)); !v.Free || !strings.Contains(v.Why, "card table") {
		t.Fatalf("a probe whose caller left reads as not judged: %+v", v)
	}
	if got := lines(); len(got) != 0 {
		t.Fatalf("a cancelled caller writes no line, got %v", got)
	}

	for i := 0; i < 5; i++ { // a scan with five tickets waiting
		ask(laneReq(nil))
	}
	if got := lines(); len(got) != 1 {
		t.Fatalf("five questions about one fault leave ONE line, and it is the first, got %d:\n%s", len(got), buf.String())
	}
	now = now.Add(time.Minute - time.Second)
	ask(laneReq(nil))
	if got := lines(); len(got) != 1 {
		t.Errorf("inside the minute the same fault is not said again, got %d lines", len(got))
	}
	ask(graphLaneReq(nil))
	if got := lines(); len(got) != 2 || !strings.Contains(got[1], "(run_graph)") {
		t.Errorf("the run_graph probe's question is its own line, naming the probe: %v", got)
	}
	cause = errors.New("nvidia-smi: exit status 255")
	ask(laneReq(nil))
	if got := lines(); len(got) != 3 || !strings.Contains(got[2], "exit status 255") {
		t.Errorf("another fault is another line at once: %v", got)
	}
	now = now.Add(time.Second) // a minute since the first fault's line
	cause = errors.New("nvidia-smi: exit status 9")
	ask(laneReq(nil))
	if got := lines(); len(got) != 4 || !strings.Contains(got[3], "exit status 9") {
		t.Errorf("a fault that outlasts the minute is said again: %v", got)
	}
	ask(graphLaneReq(nil))
	if got := lines(); len(got) != 4 {
		t.Errorf("the graph probe wrote its line a second ago and stays quiet: %v", got)
	}

	// A fault whose text changes every time (a pid or a time in the tool's own message) must not grow the memory without bound:
	// what is older than the window is dropped when the next fault is judged.
	for i := 0; i < 50; i++ {
		cause = fmt.Errorf("nvidia-smi: failed at attempt %d", i)
		ask(laneReq(nil))
	}
	now = now.Add(2 * time.Minute)
	cause = errors.New("nvidia-smi: exit status 9")
	ask(laneReq(nil))
	f.p.laneFaults.mu.Lock()
	kept := len(f.p.laneFaults.last)
	f.p.laneFaults.mu.Unlock()
	if kept != 1 {
		t.Errorf("only the fault of the last minute is remembered, got %d entries", kept)
	}
}

// The run_graph probe reads the table the same way: the retry, and the line.
func TestTheGraphLaneProbeRetriesAndLogsLikeTheImageOne(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	hold(t, f, "a bench render", leaseIDOf(admitUUIDA))
	shortProbeBounds(t, 60*time.Millisecond, 5*time.Second)
	var calls atomic.Int64
	f.p.alloc.Cards = slowThenFast(f.cards, &calls)
	buf := captureLogLines(t)
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); v.Free || strings.Contains(v.Why, "not judged") {
		t.Fatalf("the retry reads the table, so the whole-node lease held by the bench is seen: %+v", v)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("a read that ran out of time and one retry, got %d", n)
	}

	shortProbeBounds(t, 40*time.Millisecond, 80*time.Millisecond)
	var dead atomic.Int64
	f.p.alloc.Cards = neverAnswers(&dead)
	buf.Reset()
	if v := f.p.MediaLaneFree(context.Background(), graphLaneReq(nil)); !v.Free || !strings.Contains(v.Why, "card table") {
		t.Fatalf("an unreadable table reads as free with its reason: %+v", v)
	}
	if got := linesWith(buf, "media lane probe"); len(got) != 1 {
		t.Errorf("exactly one line for the graph probe too, got %d:\n%s", len(got), buf.String())
	}
}
