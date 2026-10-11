package mediaremote

// The second fix round of the fleet batch (P0 plan S3; ADR 0083 decision 12). The operator's rule: a busy card is a place in
// line, never a refusal, and a batch never hard-fails because the fleet is busy. So an answer that is a place NOT had (the
// queue's own ceiling, a card that stays held) ends the job `queued` with the true reason, never `failed`, and the queue's
// ceiling is not asked for again; but a fleet that can never take a job (no node serves it, a node cannot carry refine=false)
// is a configuration fact, which waiting does not clear, and ends the job at once. Once a job has waited out the ceiling, the
// jobs not yet sent are queued at once instead of each waiting the ceiling in turn.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// countingCall wraps the single-render call the batch makes, counting its calls and recording the out of each.
func countingCall(t *testing.T) (*atomic.Int32, func() map[string]int) {
	t.Helper()
	prev := batchCall
	t.Cleanup(func() { batchCall = prev })
	var n atomic.Int32
	var mu sync.Mutex
	asked := map[string]int{}
	batchCall = func(ctx context.Context, cfg config.Config, r Runner, req core.Request, route string, remotes []string, w Wait) core.Result {
		n.Add(1)
		out, _ := req.Params["out"].(string)
		mu.Lock()
		asked[out]++
		mu.Unlock()
		return prev(ctx, cfg, r, req, route, remotes, w)
	}
	return &n, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		cp := map[string]int{}
		for k, v := range asked {
			cp[k] = v
		}
		return cp
	}
}

// shortCeiling shortens the queue's ceiling (a ticket's wait, and a serial job's) for the test.
func shortCeiling(t *testing.T, d time.Duration) {
	t.Helper()
	prev := ticketMaxWait
	t.Cleanup(func() { ticketMaxWait = prev })
	ticketMaxWait = d
}

// collectLines returns a Notify that gathers what a human at the terminal is told, and a reader for it.
func collectLines() (func(string), func() string) {
	var mu sync.Mutex
	var lines []string
	return func(l string) {
			mu.Lock()
			lines = append(lines, l)
			mu.Unlock()
		}, func() string {
			mu.Lock()
			defer mu.Unlock()
			return strings.Join(lines, "\n")
		}
}

// ---- a place not had is `queued`, never `failed` ----------------------------------------------------------------------------

// The queue's own ceiling ends a ticket that found no lane anywhere. The job is then queued with that reason, and the ceiling
// is NOT asked for again as a "lane taken a moment after it read free": that would send the request as a new ticket and wait
// the whole ceiling again, twice, before failing the job (the defect this pins).
func TestBatchTicketThatExpiresIsQueuedAndNotSentAgain(t *testing.T) {
	e := newQEnv(t, 1, true) // the lane is held and the one node is busy: the job waits as a ticket
	shortCeiling(t, 150*time.Millisecond)
	calls, _ := countingCall(t)
	notify, lines := collectLines()
	b, err := RunBatch(bctx(t), e.cfg, e.lr, manyJobs(t, 1), neverWarm(t), batchOpts(func(o *BatchOpts) { o.Notify = notify }))
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode != ModeTickets {
		t.Fatalf("mode = %q, want the ticket path", b.Mode)
	}
	r := b.Rows[0]
	if calls.Load() != 1 {
		t.Errorf("%d call(s): the queue's ceiling must not be sent again as a new ticket", calls.Load())
	}
	if b.Failed() != 0 || b.Queued() != 1 || r.Verdict != VerdictQueued || r.OK || b.ExitCode() != 1 {
		t.Errorf("failed=%d queued=%d exit=%d row=%+v, want a queued row, no failed one, exit 1", b.Failed(), b.Queued(), b.ExitCode(), r)
	}
	if !b.Ceiling {
		t.Errorf("the batch must say a job waited the queue's ceiling")
	}
	if !strings.Contains(r.Error, "ticket") || !strings.Contains(r.Error, "without a lane") || !strings.Contains(r.Error, "--resume") {
		t.Errorf("row error = %q, want the queue's own reason and the way to render the job", r.Error)
	}
	if strings.Contains(r.Error, "lane was taken") || strings.Contains(lines(), "lane was taken") {
		t.Errorf("the race words were used for the queue's ceiling: row %q lines %q", r.Error, lines())
	}
}

// Once one job has waited out the queue's ceiling, no node gave the fleet's lanes to anyone for that long: the jobs not yet
// sent are queued at once, not sent to wait the ceiling again one after another (200 jobs would be held 200 ceilings).
func TestBatchTicketCeilingQueuesTheJobsNotYetSent(t *testing.T) {
	e := newQEnv(t, 1, true)
	shortCeiling(t, 150*time.Millisecond)
	calls, asked := countingCall(t)
	jobs := manyJobs(t, 3)
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), batchOpts(func(o *BatchOpts) { o.Jobs = 1 }))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || asked()[jobs[1].Out] != 0 || asked()[jobs[2].Out] != 0 {
		t.Errorf("%d call(s), asked %v: only the first job should have been sent", calls.Load(), asked())
	}
	if b.Queued() != 3 || b.Failed() != 0 || b.ExitCode() != 1 {
		t.Fatalf("queued=%d failed=%d exit=%d rows=%+v, want all three queued, none failed, exit 1", b.Queued(), b.Failed(), b.ExitCode(), b.Rows)
	}
	for _, r := range b.Rows[1:] {
		if !strings.Contains(r.Error, "not started") || !strings.Contains(r.Error, "--resume") {
			t.Errorf("row %+v, want it queued as not started, with the way to render it", r)
		}
	}
}

// The serial path says the same: the first job that waited the ceiling queues the rest at once.
func TestBatchSerialCeilingQueuesTheJobsNotYetSent(t *testing.T) {
	e := newQEnv(t, 1, false)
	fastWaits(t)
	shortCeiling(t, 80*time.Millisecond)
	var mu sync.Mutex
	asked := map[string]int{}
	scriptedCall(t, func(_ int, req core.Request) core.Result {
		out, _ := req.Params["out"].(string)
		mu.Lock()
		asked[out]++
		mu.Unlock()
		return busyNode()
	})
	jobs := manyJobs(t, 3)
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), serialOpts())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	second, third := asked[jobs[1].Out], asked[jobs[2].Out]
	mu.Unlock()
	if second != 0 || third != 0 {
		t.Errorf("jobs 1 and 2 were asked %d and %d time(s): after the first waited the ceiling they should not be sent", second, third)
	}
	if b.Queued() != 3 || b.Failed() != 0 || b.ExitCode() != 1 || !b.Ceiling {
		t.Fatalf("queued=%d failed=%d exit=%d ceiling=%v rows=%+v, want all three queued, none failed, exit 1", b.Queued(), b.Failed(), b.ExitCode(), b.Ceiling, b.Rows)
	}
	if !strings.Contains(b.Rows[0].Error, "waited") || !strings.Contains(b.Rows[0].Error, "gpu busy") {
		t.Errorf("row 0 = %+v, want the wait and the node's own reason", b.Rows[0])
	}
	for _, r := range b.Rows[1:] {
		if !strings.Contains(r.Error, "not started") {
			t.Errorf("row %+v, want it queued as not started", r)
		}
	}
}

// ---- a place that can never be had is not waited for ---------------------------------------------------------------------------

// A fleet whose every node is excluded for a CONFIGURATION fact (here: a node that cannot carry refine=false) will not take
// the job however long it is waited for, so a job that asks for it ends at once as a failed row naming the node, and the batch
// does not hold the next job for the queue's ceiling (a 200-job file held 200 x 8 h). A job that does not ask for it renders.
func TestBatchSerialFailsAtOnceWhereNoNodeCanEverTakeTheJob(t *testing.T) {
	e := newQEnv(t, 1, false, func(_ int, o *nodeOpts) { o.healthEdit = without("refine_honoured") })
	fastWaits(t)
	shortCeiling(t, 400*time.Millisecond)
	calls, _ := countingCall(t)
	no := false
	jobs := manyJobs(t, 1)
	jobs[0].Refine = &no
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), serialOpts())
	if err != nil {
		t.Fatal(err)
	}
	r := b.Rows[0]
	if b.Mode != ModeSerial || b.Failed() != 1 || b.Queued() != 0 || r.Verdict != VerdictFailed || b.ExitCode() != 4 {
		t.Fatalf("mode=%q failed=%d queued=%d exit=%d row=%+v, want a failed row (exit 4): waiting does not clear a configuration fact", b.Mode, b.Failed(), b.Queued(), b.ExitCode(), r)
	}
	if calls.Load() != 1 {
		t.Errorf("%d call(s), want one: a configuration fact is not asked about again", calls.Load())
	}
	if !strings.Contains(r.Error, "node-a") || !strings.Contains(r.Error, "refine=false") {
		t.Errorf("row error = %q, want the node and its miss named", r.Error)
	}
	if strings.Contains(r.Error, "waited") {
		t.Errorf("row error = %q: the job did not wait", r.Error)
	}
}

// Only a fleet that EVERY node of excludes for configuration is final. A fleet with one node that is merely out of reach (or
// busy, or text-leased) may take the job later, so the job still waits for it, and ends queued at the ceiling.
func TestBatchSerialStillWaitsWhereAnyNodeMightTakeTheJobLater(t *testing.T) {
	e := newQEnv(t, 1, false, func(_ int, o *nodeOpts) { o.healthEdit = without("refine_honoured") })
	e.cfg.DelegateRemotes = append(e.cfg.DelegateRemotes, "http://127.0.0.1:1") // a node that does not answer
	fastWaits(t)
	shortCeiling(t, 150*time.Millisecond)
	calls, _ := countingCall(t)
	no := false
	jobs := manyJobs(t, 1)
	jobs[0].Refine = &no
	b, err := RunBatch(bctx(t), e.cfg, e.lr, jobs, neverWarm(t), serialOpts())
	if err != nil {
		t.Fatal(err)
	}
	if b.Queued() != 1 || b.Failed() != 0 || b.ExitCode() != 1 || calls.Load() < 2 {
		t.Errorf("queued=%d failed=%d exit=%d calls=%d row=%+v, want the job asked again and queued at the ceiling", b.Queued(), b.Failed(), b.ExitCode(), calls.Load(), b.Rows[0])
	}
}

// ---- the typed reason of a fleet that can never take a call -----------------------------------------------------------------------

// pickNode files core.ErrClassNoNodeConfigured exactly when it has no node and EVERY node was left out for a configuration
// fact (it does not serve the task, a route is not configured on it, the tailnet guard refused its entry). One node that
// is merely out of reach or text-leased may take the call later, so a roster with such a node is an ordinary capacity
// defer, which a caller that waits for a place keeps waiting for. The class changes nothing else about the answer.
func TestPickNodeFilesNoNodeConfiguredOnlyWhenEveryNodeIsLeftOutForConfiguration(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	routes := `[{"route":"generate_video","engine":"comfyui","state":"CONFIGURED"}]`
	otherTask := fakeNode(t, fakeOpts{tasks: []string{"image-gen"}, routes: routes}).URL
	unconfigured := fakeNode(t, fakeOpts{tasks: []string{"video-gen"}, routes: `[{"route":"generate_video","engine":"comfyui","state":"NOT_CONFIGURED"}]`}).URL
	textLeased := fakeNode(t, fakeOpts{tasks: []string{"video-gen"}, routes: routes, lease: `{"held":true,"class":"text","busy":true}`}).URL
	dead := "http://127.0.0.1:1"
	refused := "http://node-x.tailkkkkkk.ts.net:18811"
	for name, tc := range map[string]struct {
		bases []string
		want  bool
	}{
		"a node that does not serve the task":     {[]string{otherTask}, true},
		"a route that is not configured":          {[]string{unconfigured}, true},
		"an entry the tailnet guard refused":      {[]string{refused}, true},
		"every node left out for configuration":   {[]string{otherTask, unconfigured, refused}, true},
		"a node out of reach":                     {[]string{dead}, false},
		"a text lease on the card":                {[]string{textLeased}, false},
		"configuration and a node out of reach":   {[]string{otherTask, dead}, false},
		"configuration and a text-leased node":    {[]string{unconfigured, textLeased}, false},
		"a refused entry and a node out of reach": {[]string{refused, dead}, false},
		"a node out of reach and a text lease":    {[]string{dead, textLeased}, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := pickNode(context.Background(), config.Config{}, tc.bases, "video-gen", "video-gen", []string{"generate_video"})
			var pe *placementError
			if err == nil || !errors.As(err, &pe) || pe.class != core.DeferClassCapacity {
				t.Fatalf("err = %v, want a capacity placementError whatever the cause", err)
			}
			var nc *noNodeConfigured
			if got := errors.As(err, &nc); got != tc.want {
				t.Errorf("noNodeConfigured = %v, want %v: %v", got, tc.want, err)
			}
			res := Run(context.Background(), config.Config{MediaDir: t.TempDir(), DelegateRemotes: tc.bases}, &recordingRunner{}, video(nil), "remote", nil)
			wantClass := ""
			if tc.want {
				wantClass = core.ErrClassNoNodeConfigured
			}
			if res.OK || !res.Deferred || res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass != wantClass {
				t.Errorf("the call answers %+v, want a capacity defer with err_class %q", res, wantClass)
			}
			if core.CardHeld(res.Meta.ErrClass) {
				t.Errorf("a configuration fact is not a held card")
			}
		})
	}
}
