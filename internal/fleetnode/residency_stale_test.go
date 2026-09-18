// residency_stale_test.go: the residency cache's staleness bound (0.128.2).
// A health read past one extra TTL window, never probed, or invalidated by an
// agent job that just finished on the seat WAITS (bounded) for a fresh
// /running answer instead of serving the previous window's — the 0.128.1
// slot census charged a 28 s cold load in the eta for a seat whose admission
// wait was 6 ms because the first read after a two-minute quiet period served
// the answer taken before the last job.

package fleetnode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// mutableSwap is a fake llama-swap whose /running answer the test flips, and
// whose /running can be held open (hang) to model a slow box.
type mutableSwap struct {
	srv       *httptest.Server
	mu        sync.Mutex
	state     string // "" = nothing running, else the state of the canonical model
	hang      chan struct{}
	canonical string
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
			m.mu.Lock()
			state, hang := m.state, m.hang
			m.mu.Unlock()
			if hang != nil {
				select {
				case <-hang:
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

func healthOnce(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d (body %s)", rec.Code, rec.Body.String())
	}
	return decodeMap(t, rec)
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m := healthOnce(t, s); m["seat_loaded"] == true {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never published the loaded seat")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHealthBoundsItsWaitOnAHungSeatRead: a too-stale read on a box whose
// /running hangs costs at most residencyWaitBound, then serves what it has
// (an unread seat state = both fields absent) — never an open-ended block.
func TestHealthBoundsItsWaitOnAHungSeatRead(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	swap.holdRunning(t)
	old := residencyWaitBound
	residencyWaitBound = 150 * time.Millisecond
	t.Cleanup(func() { residencyWaitBound = old })
	s, _ := newTestServer(t, agentHealthCfg(swap.srv.URL), &fakeRunner{}, authOpts(true))

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
	// Fail-closed survives the bound: nothing has been published, so the
	// residency verdict is still the unread one (absent = false), never a
	// guess that the seat is resident.
	if v, present := m["agent_seat_resident"]; present {
		t.Fatalf("agent_seat_resident = %v while the probe has not landed, want the field absent (fail closed)", v)
	}
}

// TestAFinishedAgentJobInvalidatesTheResidencyCache: the cache is FRESH and
// says not loaded; an agent job finishes on the seat; the next read must
// probe again and report loaded. An agent job that ERRORS is no proof and
// leaves the cache alone.
func TestAFinishedAgentJobInvalidatesTheResidencyCache(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	s, jobs := newTestServer(t, agentHealthCfg(swap.srv.URL), &fakeRunner{}, authOpts(true))
	if m := healthAfterProbe(t, s); m["seat_loaded"] != false {
		t.Fatalf("fixture: seat_loaded = %v, want false with nothing running", m["seat_loaded"])
	}
	swap.set("ready")

	// An errored agent job: the cache must stay stamped.
	if !jobs.Admit("agent-err", AcceptSpec{Agent: true, Task: "agent-run"}, func(ctx context.Context) (json.RawMessage, error) {
		return nil, fmt.Errorf("seat never answered")
	}) {
		t.Fatal("admit agent-err: refused")
	}
	waitJobTerminal(t, jobs, "agent-err")
	s.agentRes.mu.Lock()
	stamped := !s.agentRes.at.IsZero()
	s.agentRes.mu.Unlock()
	if !stamped {
		t.Fatal("an ERRORED agent job invalidated the residency cache: a job that failed because the seat never answered proves nothing about the seat")
	}
	if m := healthOnce(t, s); m["seat_loaded"] != false {
		t.Fatalf("seat_loaded = %v after an errored job on a fresh cache, want the cached false (nothing invalidated it)", m["seat_loaded"])
	}

	// A finished agent job: the next read probes fresh.
	finish := blockingJob(t, jobs, "agent-ok", AcceptSpec{Agent: true, Task: "agent-run"})
	finish()
	waitJobTerminal(t, jobs, "agent-ok")
	s.agentRes.mu.Lock()
	stamped = !s.agentRes.at.IsZero()
	s.agentRes.mu.Unlock()
	if stamped {
		t.Fatal("a finished agent job left the residency cache stamped: the next health read would serve the pre-job seat state")
	}
	if m := healthOnce(t, s); m["seat_loaded"] != true {
		t.Fatalf("seat_loaded = %v on the first read after an agent job finished on the seat, want true (payload %v)", m["seat_loaded"], m)
	}
}

func waitJobTerminal(t *testing.T, jobs *Jobs, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs.mu.Lock()
		jb := jobs.m[id]
		terminal := jb != nil && (jb.state == JobDone || jb.state == JobError)
		jobs.mu.Unlock()
		if terminal {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached a terminal state", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
