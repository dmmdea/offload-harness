// residency_stale_test.go: the residency cache's staleness bound (0.128.2).
// A health read past one extra TTL window, never probed, or invalidated by an
// agent job that finished with an error WAITS (bounded) for a fresh /running
// answer instead of serving the previous window's — the 0.128.1 slot census
// charged a 28 s cold load in the eta for a seat whose admission wait was
// 6 ms because the first read after a two-minute quiet period served the
// answer taken before the last job. A job that finished without an error
// writes what it proves straight into the cache, and a probe that started
// before such a write is discarded when it lands.

package fleetnode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mutableSwap is a fake llama-swap whose /running answer the test flips, and
// whose /running can be held open (hang) or slowed to model a slow box.
type mutableSwap struct {
	srv       *httptest.Server
	mu        sync.Mutex
	state     string // "" = nothing running, else the state of the canonical model
	hang      chan struct{}
	delay     time.Duration
	canonical string
	running   atomic.Int64 // /running hits
}

func newMutableSwap(t *testing.T, canonical, alias string) *mutableSwap {
	t.Helper()
	m := &mutableSwap{canonical: canonical}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","meta":{"llamaswap":{"aliases":[%q]}}}]}`, canonical, alias)
		case "/running":
			m.running.Add(1)
			m.mu.Lock()
			state, hang, delay := m.state, m.hang, m.delay
			m.mu.Unlock()
			if hang != nil {
				select {
				case <-hang:
				case <-r.Context().Done():
					return
				}
			}
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
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
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mutableSwap) set(state string) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

func (m *mutableSwap) slow(d time.Duration) {
	m.mu.Lock()
	m.delay = d
	m.mu.Unlock()
}

// holdRunning makes every /running read block until the returned release
// runs; release is also registered as a cleanup so the server can close.
func (m *mutableSwap) holdRunning(t *testing.T) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	m.mu.Lock()
	m.hang = ch
	m.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return release
}

// ageResidency backdates the cached residency answer so the next read sees
// it as `age` old.
func ageResidency(s *Server, age time.Duration) {
	s.agentRes.mu.Lock()
	s.agentRes.at = time.Now().Add(-age)
	s.agentRes.mu.Unlock()
}

func residencyStamped(s *Server) bool {
	s.agentRes.mu.Lock()
	defer s.agentRes.mu.Unlock()
	return !s.agentRes.at.IsZero()
}

func residencyInflight(s *Server) bool {
	s.agentRes.mu.Lock()
	defer s.agentRes.mu.Unlock()
	return s.agentRes.inflight
}

func shortWaitBound(t *testing.T, d time.Duration) {
	t.Helper()
	old := residencyWaitBound
	residencyWaitBound = d
	t.Cleanup(func() { residencyWaitBound = old })
}

func healthOnce(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d (body %s)", rec.Code, rec.Body.String())
	}
	return decodeMap(t, rec)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitJobTerminal(t *testing.T, jobs *Jobs, id string) {
	t.Helper()
	waitUntil(t, "job "+id+" to reach a terminal state", func() bool {
		jobs.mu.Lock()
		defer jobs.mu.Unlock()
		jb := jobs.m[id]
		return jb != nil && (jb.state == JobDone || jb.state == JobError)
	})
}

func finishAgentJob(t *testing.T, jobs *Jobs, id string, err error) {
	t.Helper()
	if !jobs.Admit(id, AcceptSpec{Agent: true, Task: "agent-run"}, func(ctx context.Context) (json.RawMessage, error) {
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{"ok":true}`), nil
	}) {
		t.Fatalf("admit %s: refused", id)
	}
	waitJobTerminal(t, jobs, id)
}

// TestHealthWaitsForAFreshSeatStateBeyondTheStaleBand: the previous answer
// says the seat is not loaded; it is two windows old; the seat has loaded
// since. ONE health read must report loaded — not the stale false that the
// pre-0.128.2 read served while refreshing behind it.
func TestHealthWaitsForAFreshSeatStateBeyondTheStaleBand(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	s, _ := newTestServer(t, agentHealthCfg(swap.srv.URL), &fakeRunner{}, authOpts(true))
	if m := healthAfterProbe(t, s); m["seat_loaded"] != false {
		t.Fatalf("fixture: seat_loaded = %v, want false with nothing running", m["seat_loaded"])
	}
	swap.set("ready")
	ageResidency(s, agentResidencyMaxStale+time.Second)

	m := healthOnce(t, s)
	if m["seat_loaded"] != true {
		t.Fatalf("seat_loaded = %v on the first read after a too-stale cache, want true: the read must wait for the fresh /running, not serve the answer taken before the seat loaded (payload %v)", m["seat_loaded"], m)
	}
}

// TestHealthServesThePreviousSeatStateInsideTheStaleBand is the control: one
// window past the TTL is still stale-while-revalidate — the first read serves
// the previous answer, the refresh lands behind it.
func TestHealthServesThePreviousSeatStateInsideTheStaleBand(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	s, _ := newTestServer(t, agentHealthCfg(swap.srv.URL), &fakeRunner{}, authOpts(true))
	if m := healthAfterProbe(t, s); m["seat_loaded"] != false {
		t.Fatalf("fixture: seat_loaded = %v, want false with nothing running", m["seat_loaded"])
	}
	swap.set("ready")
	ageResidency(s, agentResidencyTTL+time.Second)

	if m := healthOnce(t, s); m["seat_loaded"] != false {
		t.Fatalf("seat_loaded = %v inside the stale band, want the previous false: a read one window past the TTL keeps serving while it refreshes", m["seat_loaded"])
	}
	waitUntil(t, "the background refresh to publish the loaded seat", func() bool {
		return healthOnce(t, s)["seat_loaded"] == true
	})
}

// TestHealthBoundsItsWaitOnAHungSeatRead: a too-stale read on a box whose
// /running hangs costs at most residencyWaitBound, then serves what it has
// (an unread seat state = both fields absent, residency absent = fail
// closed) — never an open-ended block — and says so ONCE per refresh cycle.
func TestHealthBoundsItsWaitOnAHungSeatRead(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	swap.holdRunning(t)
	shortWaitBound(t, 150*time.Millisecond)
	s, _ := newTestServer(t, agentHealthCfg(swap.srv.URL), &fakeRunner{}, authOpts(true))
	buf := captureLog(t)

	t0 := time.Now()
	m := healthOnce(t, s) // never probed: the read waits for the probe it kicks
	took := time.Since(t0)
	if took > 2*time.Second {
		t.Fatalf("health took %s on a hung /running, want at most about residencyWaitBound (%s)", took, residencyWaitBound)
	}
	if took < residencyWaitBound/2 {
		t.Fatalf("health took %s, want it to have actually waited about %s for the probe", took, residencyWaitBound)
	}
	if _, present := m["seat_loaded"]; present {
		t.Fatalf("seat_loaded = %v while /running is still hung, want the field absent (unread)", m["seat_loaded"])
	}
	if v, present := m["agent_seat_resident"]; present {
		t.Fatalf("agent_seat_resident = %v while the probe has not landed, want the field absent (fail closed)", v)
	}
	const said = "serving the previous answer"
	if n := strings.Count(buf.String(), said); n != 1 {
		t.Fatalf("the timed-out wait was logged %d times after one read, want exactly 1 (log: %s)", n, buf.String())
	}
	healthOnce(t, s) // same refresh cycle, still hung: waits again, must NOT log again
	if n := strings.Count(buf.String(), said); n != 1 {
		t.Fatalf("the timed-out wait was logged %d times after two reads in ONE refresh cycle, want 1 — health is polled by every delegator", n)
	}
}

// TestAFinishedAgentJobWritesItsProofIntoTheResidencyCache: /running says
// nothing is loaded (so the ONLY possible source of "loaded" is the job); a
// successful agent job finishes; the very next read reports loaded and
// resident WITHOUT touching /running. An errored agent job proves nothing:
// it invalidates instead, and the next read probes fresh.
func TestAFinishedAgentJobWritesItsProofIntoTheResidencyCache(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	s, jobs := newTestServer(t, agentHealthCfg(swap.srv.URL), &fakeRunner{}, authOpts(true))
	if m := healthAfterProbe(t, s); m["seat_loaded"] != false {
		t.Fatalf("fixture: seat_loaded = %v, want false with nothing running", m["seat_loaded"])
	}

	// An errored agent job: the cache is invalidated, and the next read probes.
	finishAgentJob(t, jobs, "agent-err", fmt.Errorf("wall timeout after 300s"))
	if residencyStamped(s) {
		t.Fatal("an errored agent job left the residency cache stamped: the node cannot tell a wall timeout from a seat that never answered, so it must probe again")
	}
	before := swap.running.Load()
	if m := healthOnce(t, s); m["seat_loaded"] != false {
		t.Fatalf("seat_loaded = %v after an errored job, want the freshly probed false (nothing is running)", m["seat_loaded"])
	}
	if swap.running.Load() == before {
		t.Fatal("the read after an errored job served the cache without probing /running")
	}

	// A successful agent job: its proof is written, no probe.
	finishAgentJob(t, jobs, "agent-ok", nil)
	if !residencyStamped(s) {
		t.Fatal("a finished agent job did not stamp the residency cache")
	}
	before = swap.running.Load()
	m := healthOnce(t, s)
	if m["seat_loaded"] != true || m["seat_starting"] != false || m["agent_seat_resident"] != true {
		t.Fatalf("after a finished agent job: seat_loaded=%v seat_starting=%v agent_seat_resident=%v, want true/false/true — the job is the proof (payload %v)", m["seat_loaded"], m["seat_starting"], m["agent_seat_resident"], m)
	}
	if swap.running.Load() != before {
		t.Fatal("the read after a finished agent job probed /running: the job's own proof must be served without a probe")
	}
}

// TestBackToBackFinishesNeverWaitOnASlowSeatRead: jobs finishing every few
// milliseconds on a node whose /running is slow must NOT make each following
// health read pay the wait bound — a finished job writes the cache itself.
func TestBackToBackFinishesNeverWaitOnASlowSeatRead(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	s, jobs := newTestServer(t, agentHealthCfg(swap.srv.URL), &fakeRunner{}, authOpts(true))
	healthAfterProbe(t, s)
	shortWaitBound(t, 150*time.Millisecond)
	swap.slow(300 * time.Millisecond)

	for i := 0; i < 5; i++ {
		finishAgentJob(t, jobs, fmt.Sprintf("agent-%d", i), nil)
		t0 := time.Now()
		m := healthOnce(t, s)
		if took := time.Since(t0); took > 75*time.Millisecond {
			t.Fatalf("read %d after a finished job took %s, want no wait at all (a finished job writes the cache; the slow /running must never be on this path)", i, took)
		}
		if m["seat_loaded"] != true {
			t.Fatalf("read %d after a finished job: seat_loaded = %v, want true", i, m["seat_loaded"])
		}
	}
}

// TestAProbeThatStartedBeforeAJobFinishedIsDiscarded: a refresh reads
// /running while the seat is still empty, a job finishes on the seat before
// that refresh publishes, the refresh lands — and must NOT overwrite the
// job's proof nor re-stamp the pre-job reading as fresh.
func TestAProbeThatStartedBeforeAJobFinishedIsDiscarded(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	release := swap.holdRunning(t)
	shortWaitBound(t, 100*time.Millisecond)
	s, jobs := newTestServer(t, agentHealthCfg(swap.srv.URL), &fakeRunner{}, authOpts(true))

	healthOnce(t, s) // never probed: kicks the refresh, which hangs on /running
	if !residencyInflight(s) {
		t.Fatal("fixture: the refresh should still be in flight, hung on /running")
	}
	finishAgentJob(t, jobs, "agent-ok", nil) // the fact lands while the probe is stuck
	release()                                // the stale reading ("nothing running") now publishes
	waitUntil(t, "the held refresh to land", func() bool { return !residencyInflight(s) })

	m := healthOnce(t, s)
	if m["seat_loaded"] != true || m["agent_seat_resident"] != true {
		t.Fatalf("seat_loaded=%v agent_seat_resident=%v after a stale probe landed behind a finished job, want true/true: the reading predates the job and must be discarded (payload %v)", m["seat_loaded"], m["agent_seat_resident"], m)
	}
}
