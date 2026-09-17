// The node tells the truth (register S-09 / S-17 / S-19 / S-36, diagnosis
// "2026-09-17-harness-scheduling-diagnosis.md" §2(b)(d)(e), §5.2-5.3).
//
// Three defects, one subject: what this node SAYS about itself while work is in
// flight.
//
//  1. A poll had no completion event. The job store already broadcast on every
//     terminal transition, and the only way to learn a job had finished was to
//     ask again in three seconds — 236 measured rows spent exactly 300 s doing
//     nothing else. `?wait=` turns the existing broadcast into an answer.
//  2. The blanket WriteTimeout bounded EVERY handler at header-read, including
//     the chat lane whose own budget is ten minutes. A handler that outlives it
//     is cut mid-write and reads to the caller as a broken box.
//  3. Health published `jobs_running` for a job parked in ADMISSION with the
//     card idle, no seat load state at all, and no lease facts beyond "busy" —
//     so a delegator could not tell a cordon-waiter from a generating seat.
package fleetnode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// ---- 1. the long poll -------------------------------------------------------

// blockingJob admits a job that finishes when the returned func is called.
func blockingJob(t *testing.T, jobs *Jobs, id string, spec AcceptSpec) func() {
	t.Helper()
	release := make(chan struct{})
	started := make(chan struct{})
	if !jobs.Admit(id, spec, func(ctx context.Context) (json.RawMessage, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`{"ok":true}`), nil
	}) {
		t.Fatalf("admit %s: refused", id)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("job %s never started", id)
	}
	return func() { close(release) }
}

// TestJobLongPollAnswersWhenTheJobFinishes is S-19's red test: a job that
// finishes 200 ms from now, polled with wait=10, must answer AT THE FINISH with
// the terminal state — not immediately with `running`, and not at the wait.
func TestJobLongPollAnswersWhenTheJobFinishes(t *testing.T) {
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	finish := blockingJob(t, jobs, "lp-1", AcceptSpec{Task: "image-gen"})
	time.AfterFunc(200*time.Millisecond, finish)

	start := time.Now()
	rec := do(t, s, http.MethodGet, "/fleet/jobs/lp-1?wait=10", "", nil)
	waited := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["state"] != "done" {
		t.Fatalf("state = %v after %s with wait=10: the poll must BLOCK until the job is terminal, not answer `running` at once (the 3 s re-poll is exactly the 300 s of pure polling S-19 measured)", m["state"], waited)
	}
	if waited > 5*time.Second {
		t.Fatalf("the poll answered after %s: it must wake on the store's terminal broadcast, not sit out the wait", waited)
	}
}

// TestJobPollWithoutWaitStaysImmediate is the compatibility half: absent the
// parameter the route is byte-identical, so every deployed delegator keeps its
// 3 s cadence and its non-blocking poll.
func TestJobPollWithoutWaitStaysImmediate(t *testing.T) {
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	finish := blockingJob(t, jobs, "np-1", AcceptSpec{Task: "image-gen"})
	defer finish()

	start := time.Now()
	rec := do(t, s, http.MethodGet, "/fleet/jobs/np-1", "", nil)
	waited := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if m := decodeMap(t, rec); m["state"] != "running" {
		t.Fatalf("state = %v, want running", m["state"])
	}
	if waited > time.Second {
		t.Fatalf("a poll with no wait= took %s: it must not block at all", waited)
	}
}

// ---- 2. per-handler write deadlines ----------------------------------------

// slowSwap is a fake llama-swap whose chat completion answers only after
// `delay` — the shape of a cold swap plus a real generation, which is what
// ChatProxyTimeout (10 min) budgets for and what the blanket WriteTimeout
// (30 s) cuts off.
func slowSwap(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"complete"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveOn runs the node's own routed handler behind a REAL http.Server whose
// blanket WriteTimeout is `blanket`, and returns its base URL. The blanket is
// the production table's 30 s compressed: the assertion is that a handler's own
// deadline EXCEEDS whatever blanket the server was built with, which is the
// exact property `http.ResponseController.SetWriteDeadline` buys and the only
// one a test can observe in under 30 seconds.
func serveOn(t *testing.T, s *Server, blanket time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: blanket}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// TestChatLaneOutlivesTheBlanketWriteTimeout is S-09's red test. Go arms the
// write deadline at header-read, so a 30 s blanket bounds a handler that
// budgets ChatProxyTimeout = 10 minutes: the answer is cut mid-write and the
// caller reads a healthy node as broken infrastructure.
func TestChatLaneOutlivesTheBlanketWriteTimeout(t *testing.T) {
	up := slowSwap(t, 700*time.Millisecond)
	s := servingNode(t, chatCfg(up.URL, ""), true, "gemma-4-e4b")
	base := serveOn(t, s, 250*time.Millisecond)

	resp, err := http.Post(base+ChatLanePath, "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("chat forward died at the blanket write timeout: %v — the handler must extend its own deadline to ChatProxyTimeout", err)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("reading the forwarded answer: %v (got %q)", rerr, body)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "complete") {
		t.Fatalf("status %d body %q: the whole upstream answer must come back", resp.StatusCode, body)
	}
}

// TestJobLongPollOutlivesTheBlanketWriteTimeout is the same defect on the new
// route: a poll that blocks for its wait is a handler that outlives a blanket
// shorter than the wait, and would be cut exactly like the chat lane.
func TestJobLongPollOutlivesTheBlanketWriteTimeout(t *testing.T) {
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	finish := blockingJob(t, jobs, "lp-2", AcceptSpec{Task: "image-gen"})
	time.AfterFunc(700*time.Millisecond, finish)
	base := serveOn(t, s, 250*time.Millisecond)

	resp, err := http.Get(base + "/fleet/jobs/lp-2?wait=10")
	if err != nil {
		t.Fatalf("long poll died at the blanket write timeout: %v", err)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("reading the poll answer: %v (got %q)", rerr, body)
	}
	if !strings.Contains(string(body), `"state":"done"`) {
		t.Fatalf("poll body %q: the long poll must answer with the terminal state, whole", body)
	}
}

// ---- 3. honest health ------------------------------------------------------

// fakeSwapWithRunning serves BOTH endpoints the node reads: /v1/models (the
// roster, which is how an alias-bound seat resolves at all) and /running (what
// llama-swap says is loaded RIGHT NOW). /running is the only seat read allowed
// here: /upstream/<seat>/… would LOAD an unloaded seat (register C-05).
func fakeSwapWithRunning(t *testing.T, canonical, alias, state string, runningOK bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","meta":{"llamaswap":{"aliases":[%q]}}}]}`, canonical, alias)
		case "/running":
			if !runningOK {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if state == "" {
				fmt.Fprint(w, `{"running":[]}`)
				return
			}
			fmt.Fprintf(w, `{"running":[{"model":%q,"state":%q}]}`, canonical, state)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// healthAfterProbe drives health until the background residency refresh has
// published, then returns the decoded payload — the same discipline every
// residency test uses (production's ONLY probe trigger is the health path).
func healthAfterProbe(t *testing.T, s *Server) map[string]any {
	t.Helper()
	do(t, s, http.MethodGet, "/fleet/health", "", nil)
	waitForResidencyProbe(t, s)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d (body %s)", rec.Code, rec.Body.String())
	}
	return decodeMap(t, rec)
}

// TestHealthPublishesSeatLoadedFromRunning (S-17, seat state): llama-swap says
// the seat is loaded under its CANONICAL id while the harness binds the ALIAS.
// An alias-blind read reports a loaded seat as absent — the exact defect
// register C-11/S-08 fixed for the drain and nowhere else.
func TestHealthPublishesSeatLoadedFromRunning(t *testing.T) {
	swap := fakeSwapWithRunning(t, "gemma-4-e4b", "offload-e4b", "ready", true)
	s, _ := newTestServer(t, agentHealthCfg(swap.URL), &fakeRunner{}, authOpts(true))

	m := healthAfterProbe(t, s)
	if m["seat_loaded"] != true {
		t.Fatalf("seat_loaded = %v, want true: /running lists the seat's canonical id and the node must resolve the alias (payload %v)", m["seat_loaded"], m)
	}
	if m["seat_starting"] != false {
		t.Fatalf("seat_starting = %v, want an explicit false: a READ seat state must be distinguishable from an unread one", m["seat_starting"])
	}
}

// TestHealthPublishesSeatStarting: a seat mid-load is loaded-but-not-yet-usable
// (register D-92). A delegator that cannot see it either waits blind or places
// work on a seat that will hold its request for four minutes.
func TestHealthPublishesSeatStarting(t *testing.T) {
	swap := fakeSwapWithRunning(t, "gemma-4-e4b", "offload-e4b", "starting", true)
	s, _ := newTestServer(t, agentHealthCfg(swap.URL), &fakeRunner{}, authOpts(true))

	m := healthAfterProbe(t, s)
	if m["seat_loaded"] != true || m["seat_starting"] != true {
		t.Fatalf("seat_loaded/seat_starting = %v/%v, want true/true for a seat llama-swap lists as `starting`", m["seat_loaded"], m["seat_starting"])
	}
}

// TestHealthOmitsSeatStateWhenRunningIsUnreadable: a failed read is UNKNOWN,
// never "not loaded". Absent ≠ idle is the same rule the VRAM snapshot and the
// reclaim verdict already follow.
func TestHealthOmitsSeatStateWhenRunningIsUnreadable(t *testing.T) {
	swap := fakeSwapWithRunning(t, "gemma-4-e4b", "offload-e4b", "ready", false)
	s, _ := newTestServer(t, agentHealthCfg(swap.URL), &fakeRunner{}, authOpts(true))

	m := healthAfterProbe(t, s)
	if _, ok := m["seat_loaded"]; ok {
		t.Fatalf("seat_loaded = %v after a failed /running read: an unread seat state must be ABSENT", m["seat_loaded"])
	}
	if _, ok := m["seat_starting"]; ok {
		t.Fatalf("seat_starting = %v after a failed /running read: an unread seat state must be ABSENT", m["seat_starting"])
	}
}

// TestHealthCountsAdmittingJobsOutOfTheSaturationScore is S-17's red test.
// jobs.go flips a job to `running` BEFORE execute(), and the run then spends up
// to 300 s in admission (cordon → pre-flight → warm → coherence probe) with the
// card idle. Through all of it the job holds a capped slot and drives
// saturation.score to 1.0 while gpu_util_pct reads 0 — a cordon-waiter holds no
// card, and a delegator ranking on that number routes away from an idle box.
func TestHealthCountsAdmittingJobsOutOfTheSaturationScore(t *testing.T) {
	state := t.TempDir()
	cfg := agentHealthCfg(fakeSwapWithRunning(t, "gemma-4-e4b", "offload-e4b", "ready", true).URL)
	cfg.FleetMaxConcurrentJobs = 1
	// Unlimited DEPTH on purpose: max_queue_depth bounds every admitted job
	// whatever its phase, so the depth term is deliberately untouched by this
	// change and would otherwise be the only thing in the score. What must move
	// is the CONCURRENCY term.
	cfg.FleetMaxQueueDepth = -1
	cfg.StateDir = state
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))

	finish := blockingJob(t, jobs, "adm-1", AcceptSpec{Agent: true, Task: "agent-run"})
	defer finish()
	// The worker's OWN registration, as internal/pipeline writes it when an
	// agent contract starts: phase "admission" until the first planner step.
	act := gpuactivity.Start(cfg.GPULockPath, cfg.StateDir, gpuactivity.Run{
		Seat: "offload-e4b", Kind: "contract", Origin: "testnode", Phase: "admission"})
	if act == nil {
		t.Fatal("could not register the admission run")
	}
	defer act.End()

	m := healthAfterProbe(t, s)
	if m["jobs_running"] != float64(1) {
		t.Fatalf("jobs_running = %v, want 1 (the claimed job is real; only its PHASE is the news)", m["jobs_running"])
	}
	if m["jobs_admitting"] != float64(1) {
		t.Fatalf("jobs_admitting = %v, want 1: a worker parked in admission must be published, not hidden inside jobs_running", m["jobs_admitting"])
	}
	sat, _ := m["saturation"].(map[string]any)
	if sat == nil {
		t.Fatalf("no saturation block: %v", m)
	}
	if sat["score"] != float64(0) {
		t.Fatalf("saturation.score = %v, want 0 with max_concurrent_jobs=1 and the one running job still in ADMISSION — a cordon-waiter holds no card", sat["score"])
	}
}

// TestHealthReportsTheRecentAgentWall is S-36's red test: a node with no
// seat_rate sample publishes no completion signal at all, so a fresh box is
// indistinguishable from a fast one. The median of the last finished agent jobs
// is a number this node already has, in the very map the /fleet/jobs listing
// walks.
func TestHealthReportsTheRecentAgentWall(t *testing.T) {
	cfg := agentHealthCfg(fakeSwapWithRunning(t, "gemma-4-e4b", "offload-e4b", "ready", true).URL)
	cfg.FleetMaxConcurrentJobs = -1 // all three run at once; the walls are what matter
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))

	done := make(chan struct{}, 3)
	for i, d := range []time.Duration{100 * time.Millisecond, 600 * time.Millisecond, 300 * time.Millisecond} {
		sleep := d
		if !jobs.Admit(fmt.Sprintf("wall-%d", i), AcceptSpec{Agent: true, Task: "agent-run"}, func(ctx context.Context) (json.RawMessage, error) {
			time.Sleep(sleep)
			done <- struct{}{}
			return json.RawMessage(`{}`), nil
		}) {
			t.Fatalf("admit wall-%d refused", i)
		}
	}
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("agent jobs never finished")
		}
	}
	// The store stamps finishedAt inside finish(); give the last one a moment
	// to land before reading health.
	time.Sleep(50 * time.Millisecond)

	m := healthAfterProbe(t, s)
	v, ok := m["recent_agent_wall_sec"].(float64)
	if !ok {
		t.Fatalf("recent_agent_wall_sec absent after three FINISHED agent jobs: %v", m)
	}
	if v < 0.2 || v > 0.5 {
		t.Fatalf("recent_agent_wall_sec = %v, want the MEDIAN of 0.1/0.6/0.3 s (~0.3) — not the mean, not the newest", v)
	}
}

// TestHealthPublishesLeaseExclusiveAndDraining (S-15's inputs): `busy` is a
// verdict computed from a DECLARED expiry. Whether the lease FENCES the cards
// (exclusive) and whether it is still draining are different facts, and they
// are the ones that decide whether a job could start at all.
func TestHealthPublishesLeaseExclusiveAndDraining(t *testing.T) {
	opts := authOpts(true)
	opts.Lease = func() gpulease.Info {
		return gpulease.Info{Held: true, Class: gpulease.ClassText, PID: 4242,
			ExpiresAt: time.Now().Add(time.Hour), Exclusive: true, Draining: true}
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)

	m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if m["lease_exclusive"] != true {
		t.Fatalf("lease_exclusive = %v, want true: an exclusive lease FENCES the cards, which `busy` alone never said", m["lease_exclusive"])
	}
	if m["lease_draining"] != true {
		t.Fatalf("lease_draining = %v, want true", m["lease_draining"])
	}
}

// TestHealthNewFieldsAbsentOnAPlainNode is the additive-wire proof: a node with
// no agent lane, no lease and no admission holds emits none of the six new
// keys, so an older delegator decodes a byte-identical payload.
func TestHealthNewFieldsAbsentOnAPlainNode(t *testing.T) {
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, nil)
	body := do(t, s, http.MethodGet, "/fleet/health", "", nil).Body.String()
	for _, key := range []string{"jobs_admitting", "seat_loaded", "seat_starting",
		"lease_exclusive", "lease_draining", "recent_agent_wall_sec"} {
		if strings.Contains(body, key) {
			t.Fatalf("a plain node leaks %q: every new field is omitempty so older delegators ignore them — %s", key, body)
		}
	}
}

// ---- helpers the timeout-table test shares ---------------------------------

// recordWriteDeadlines swaps the server's deadline seam for a recorder and
// returns a reader of what each handler ASKED FOR. The ask is the fixable
// thing: a ResponseWriter that cannot carry a deadline (a recorder, a wrapping
// middleware) silently returns ErrNotSupported in production, and a test that
// only watched the wire could not tell "the handler asked and the writer
// refused" from "the handler never asked".
func recordWriteDeadlines(s *Server) func() []time.Duration {
	var mu sync.Mutex
	var asked []time.Duration
	s.setWriteDeadline = func(w http.ResponseWriter, at time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, time.Until(at))
		return nil
	}
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), asked...)
	}
}

// compressJobWaitUnit shortens the second a `wait=` value is measured in, so a
// test can observe MaxJobWaitSec without spending twelve real seconds. It
// returns the previous value for restoreJobWaitUnit.
func compressJobWaitUnit(t *testing.T, unit time.Duration) time.Duration {
	t.Helper()
	prev := jobWaitUnit
	jobWaitUnit = unit
	return prev
}

func restoreJobWaitUnit(prev time.Duration) { jobWaitUnit = prev }

// TestJobLongPollIsBoundedByTheCap: a caller asking for 60 seconds gets
// MaxJobWaitSec, and the handler returns the job's LIVE state when the wait
// runs out rather than an error or an empty body.
//
// The cap is not a preference. A wait at or above the delegator's
// pollRequestTimeout (15 s, internal/delegate/run.go) is cancelled CLIENT-side:
// the delegator would drop a connection this node was about to answer, and the
// completion event would read as a transport failure. The pairing itself is
// pinned across the package boundary in healthwire_compat_test.go.
func TestJobLongPollIsBoundedByTheCap(t *testing.T) {
	defer restoreJobWaitUnit(compressJobWaitUnit(t, 50*time.Millisecond))
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	release := blockingJob(t, jobs, "cap-1", AcceptSpec{Task: "image-gen"})
	defer release()

	start := time.Now()
	rec := do(t, s, http.MethodGet, "/fleet/jobs/cap-1?wait=60", "", nil)
	waited := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if m := decodeMap(t, rec); m["state"] != "running" {
		t.Fatalf("state = %v, want the job's LIVE state when the wait elapses", m["state"])
	}
	capped := MaxJobWaitSec * jobWaitUnit
	if waited < capped/2 {
		t.Fatalf("the poll answered after %s: a wait longer than the cap is CAPPED, not ignored", waited)
	}
	if waited > capped+500*time.Millisecond {
		t.Fatalf("the poll blocked %s, past the cap of %s (wait=60 was taken at face value — every long poll would then be cancelled client-side at the delegator's 15 s pollRequestTimeout)", waited, capped)
	}
}

// TestJobLongPollNeverOutlivesTheCallersContext: a caller that hangs up must
// release the handler at once, not at the wait. The store's waiter is bounded
// by the request context for exactly that reason.
func TestJobLongPollNeverOutlivesTheCallersContext(t *testing.T) {
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	release := blockingJob(t, jobs, "ctx-1", AcceptSpec{Task: "image-gen"})
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/fleet/jobs/ctx-1?wait=10", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.AfterFunc(50*time.Millisecond, cancel)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the long poll outlived its caller: a hung-up client must release the handler, not hold it for the whole wait")
	}
}

// TestWaitTerminalWakesOnTheDrainsVerdict: shutdown is a terminal transition
// like any other. Before the store broadcast on it, a poll parked here would
// have waited out its whole wait against a process that was already exiting.
func TestWaitTerminalWakesOnTheDrainsVerdict(t *testing.T) {
	jobs := NewJobs(time.Hour, 1)
	if !jobs.Admit("drain-1", AcceptSpec{Task: "image-gen"}, func(ctx context.Context) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}) {
		t.Fatal("admit refused")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		jobs.DrainAndStop(50 * time.Millisecond)
	}()
	start := time.Now()
	view, ok := jobs.WaitTerminal(context.Background(), "drain-1", 10*time.Second)
	if !ok || !Terminal(view.State) {
		t.Fatalf("view = %+v ok = %v, want a terminal state from the drain", view, ok)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("the waiter woke after %s: the drain's mark must broadcast", el)
	}
}

// TestFinishedAgentWallsAreTheAgentJobsNewestFirst pins the sample
// recent_agent_wall_sec is the median of: agent jobs only, finished only,
// newest FINISH first, capped at n. A deterministic clock, so the arithmetic is
// asserted rather than sampled.
func TestFinishedAgentWallsAreTheAgentJobsNewestFirst(t *testing.T) {
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	jobs := newJobs(time.Hour, func() time.Time { return base }, time.Hour, 0)
	t.Cleanup(func() { jobs.DrainAndStop(time.Second) })

	// Hand-built records: the store stamps its own times, and this test is
	// about the READER's filter and order, not about the stamping.
	jobs.mu.Lock()
	jobs.m["a"] = &job{state: JobDone, agent: true, startedAt: base, finishedAt: base.Add(2 * time.Second)}
	jobs.m["b"] = &job{state: JobDone, agent: true, startedAt: base, finishedAt: base.Add(8 * time.Second)}
	jobs.m["c"] = &job{state: JobError, agent: true, startedAt: base, finishedAt: base.Add(4 * time.Second)}
	jobs.m["media"] = &job{state: JobDone, startedAt: base, finishedAt: base.Add(60 * time.Second)}
	jobs.m["live"] = &job{state: JobRunning, agent: true, startedAt: base}
	jobs.mu.Unlock()

	walls := jobs.FinishedAgentWalls(8)
	want := []time.Duration{8 * time.Second, 4 * time.Second, 2 * time.Second}
	if len(walls) != len(want) {
		t.Fatalf("walls = %v, want the three FINISHED agent jobs (a render is not an agent job; a running one has no wall yet)", walls)
	}
	for i := range want {
		if walls[i] != want[i] {
			t.Fatalf("walls = %v, want %v (newest finish first)", walls, want)
		}
	}
	if med := medianSeconds(walls); med != 4 {
		t.Fatalf("medianSeconds = %v, want 4 — the MEDIAN of 2/4/8, not the mean (4.67) and not the newest (8)", med)
	}
	if n := len(jobs.FinishedAgentWalls(2)); n != 2 {
		t.Fatalf("FinishedAgentWalls(2) returned %d rows, want the cap", n)
	}
	// An even sample averages the two middle values, and an empty one makes no
	// claim at all (the payload omits the field).
	if med := medianSeconds([]time.Duration{2 * time.Second, 5 * time.Second}); med != 3.5 {
		t.Fatalf("medianSeconds(2s,5s) = %v, want 3.5", med)
	}
	if med := medianSeconds(nil); med != 0 {
		t.Fatalf("medianSeconds(nil) = %v, want 0 = no claim", med)
	}
}
