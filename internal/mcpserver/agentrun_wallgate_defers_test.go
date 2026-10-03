// agentrun_wallgate_defers_test.go pins that the agent_run door's wall sizing rides
// on EVERY deferral past the wall gate (register D-102, review of the fix).
//
// The sizing (wall_estimate_sec / min_turn_sec / wall_note) was stamped on the wall
// refusal, on a run that died mid-loop and on an answer, and on nothing else: the
// fence, cordon, seat-cap, served-roster, coherence, window-probe and profile
// deferrals returned bare, while the docs and the tool description promised the
// numbers on every result. A caller re-placing a capacity defer is the reader who
// most wants the estimate, so the door now stamps all of them; what still carries
// nothing is the part of the call that never reached the gate (a malformed call, a
// placement guard's refusal, a remote route), which the docs say.

package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// The run every test below asks for: two steps on a 5 tok/s seat under a 60 s wall.
// The smallest answer needs 45 s there, so the wall gate admits it, and the whole run
// is estimated well above 60 s, so the published note says the wall is BELOW the
// estimate. Any exit past the gate then has numbers to carry.
const (
	deferWallTokS  = 5
	deferWallSteps = 2
	deferWallSec   = 60
)

// requireDeferCarriesSizing fails unless m is a deferral that carries the run's D-03
// sizing under the delegation wire's names, for the run every test here asks for.
func requireDeferCarriesSizing(t *testing.T, m map[string]any) {
	t.Helper()
	if m["deferred"] != true {
		t.Fatalf("want a deferral, got %v", m)
	}
	est := fullRunSizing(deferWallTokS, deferWallSteps, deferWallSec)
	if est.TotalSec <= deferWallSec {
		t.Fatalf("test premise: the estimate %d s must exceed the %d s wall", est.TotalSec, deferWallSec)
	}
	if got, _ := m["wall_estimate_sec"].(float64); int(got) != est.TotalSec {
		t.Errorf("wall_estimate_sec = %v, want %d on a deferral (reason: %v)", m["wall_estimate_sec"], est.TotalSec, m["reason"])
	}
	if got, _ := m["min_turn_sec"].(float64); int(got) != est.MinTurnSec {
		t.Errorf("min_turn_sec = %v, want %d on a deferral (reason: %v)", m["min_turn_sec"], est.MinTurnSec, m["reason"])
	}
	if note, _ := m["wall_note"].(string); !strings.Contains(note, fmt.Sprintf("wall %d s is BELOW the estimate", deferWallSec)) {
		t.Errorf("wall_note = %q, want the arithmetic on a deferral (reason: %v)", note, m["reason"])
	}
}

// callSizedRun makes the call every test here makes, with extra JSON members spliced in.
func callSizedRun(t *testing.T, ctx context.Context, s *Server, extra string) map[string]any {
	t.Helper()
	res, err := s.handleAgentRun(ctx, callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":%d,"timeout_sec":%d%s}`, t.TempDir(), deferWallSteps, deferWallSec, extra)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	return decodeResult(t, res)
}

// armedLeaseRoot points the cordon at a fresh state root and restores the inert default
// after the test, the way the admission tests do.
func armedLeaseRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	return root
}

// leasedWallGateServer is the door over a state root that holds a text lease of the
// shape opts describes, with a 1 s admission budget so a held cordon costs the suite
// a second and not five minutes.
func leasedWallGateServer(t *testing.T, endpoint string, opts gpulease.Options) *Server {
	t.Helper()
	root := armedLeaseRoot(t)
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := m.TryAcquire(gpulease.ClassText, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })
	return wallGateServerFor(t, endpoint, deferWallTokS, func(c *config.Config) {
		c.StateDir = root
		c.AgentAdmissionWaitSec = 1
	})
}

// TestAgentRunServedRosterDeferCarriesTheSizing: the box's agent seat is not served,
// so the run defers before the loop is even built.
func TestAgentRunServedRosterDeferCarriesTheSizing(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServerSeats(t, &hits, "some-other-model")
	s := wallGateServerFor(t, srv.URL, deferWallTokS, nil)

	m := callSizedRun(t, context.Background(), s, "")
	if reason, _ := m["reason"].(string); !strings.Contains(reason, "served roster") {
		t.Fatalf("test premise: want the roster defer, got %v", m)
	}
	requireDeferCarriesSizing(t, m)
}

// TestAgentRunFenceDeferCarriesTheSizing: another process holds an exclusive lease on
// the card, so the door answers capacity at once, and a caller who re-places the run
// reads what it was estimated to need.
func TestAgentRunFenceDeferCarriesTheSizing(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := leasedWallGateServer(t, srv.URL, gpulease.Options{Reason: "bench", Exclusive: true, TTL: time.Hour})

	m := callSizedRun(t, context.Background(), s, "")
	if m["defer_class"] != string(core.DeferClassCapacity) || !strings.Contains(fmt.Sprint(m["reason"]), "no new run is admitted") {
		t.Fatalf("test premise: want the fence defer, got %v", m)
	}
	requireDeferCarriesSizing(t, m)
}

// TestAgentRunCordonDeferCarriesTheSizing: a draining hold keeps the cordon shut for
// the whole admission budget (the pre-check is blinded so the hold reaches it).
func TestAgentRunCordonDeferCarriesTheSizing(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := leasedWallGateServer(t, srv.URL, gpulease.Options{Reason: "drain", Draining: true, TTL: time.Hour})
	s.foreignFence = func(gpulease.Info) (bool, string) { return false, "" }

	m := callSizedRun(t, context.Background(), s, "")
	if note, _ := m["admission_note"].(string); !strings.Contains(note, "cordon") {
		t.Fatalf("test premise: want the cordon defer, got %v", m)
	}
	requireDeferCarriesSizing(t, m)
}

// TestAgentRunSeatCapDeferCarriesTheSizing: the seat is at its run cap and the caller's
// context ends the wait in line.
func TestAgentRunSeatCapDeferCarriesTheSizing(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	root := armedLeaseRoot(t)
	reg, err := gpuactivity.Open("", root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reg.Begin(gpuactivity.Run{Seat: wallGateSeat, Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.End)
	s := wallGateServerFor(t, srv.URL, deferWallTokS, func(c *config.Config) {
		c.StateDir = root
		c.AgentAdmissionWaitSec = 1
		c.FleetMaxConcurrentJobs = 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond) // ends the wait in line
	defer cancel()

	m := callSizedRun(t, ctx, s, "")
	if reason, _ := m["reason"].(string); !strings.Contains(reason, "seat busy") {
		t.Fatalf("test premise: want the seat-cap defer, got %v", m)
	}
	requireDeferCarriesSizing(t, m)
}

// TestAgentRunIncoherentSeatDefersCarrySizing: a cold seat that answers the post-warm
// probe with garbage defers, and so does the next, warm run on the remembered verdict.
func TestAgentRunIncoherentSeatDefersCarrySizing(t *testing.T) {
	srv, _ := coherenceSeatServer(t, wallGateSeat, func(body map[string]any) string {
		if isProbeBody(body) {
			return `{"choices":[{"message":{"role":"assistant","content":"<tool_call>` + strings.Repeat("!", 40) + `"},"finish_reason":"length"}]}`
		}
		return `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`
	})
	defer srv.Close()
	cfg := coherenceCfg(t, srv.URL, wallGateSeat, "")
	cfg.AgentSeatTokS = deferWallTokS
	s := New(pipeline.New(cfg, nil, nil, nil))

	cold := callSizedRun(t, context.Background(), s, "")
	if reason, _ := cold["reason"].(string); !strings.HasPrefix(reason, core.IncoherentSeatReason) || strings.Contains(reason, "remembered") {
		t.Fatalf("test premise: want the live probe's verdict, got %v", cold)
	}
	requireDeferCarriesSizing(t, cold)

	warm := callSizedRun(t, context.Background(), s, "")
	if reason, _ := warm["reason"].(string); !strings.Contains(reason, "remembered") {
		t.Fatalf("test premise: want the remembered verdict, got %v", warm)
	}
	requireDeferCarriesSizing(t, warm)
}

// TestAgentRunWindowProbeFenceDeferCarriesTheSizing: a render takes the card after the
// cordon, while the seat is cold, so the window probe is never sent.
func TestAgentRunWindowProbeFenceDeferCarriesTheSizing(t *testing.T) {
	root := armedLeaseRoot(t)
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/running":
			once.Do(func() { // the render starts while this run is being admitted
				l, lerr := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "video render", TTL: time.Hour})
				if lerr != nil {
					t.Errorf("acquire the render's lease: %v", lerr)
					return
				}
				t.Cleanup(func() { _ = l.Release() })
			})
			fmt.Fprint(w, `{"running":[]}`)
		case r.URL.Path == "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, wallGateSeat)
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, wallGateSeat)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := wallGateServerFor(t, srv.URL, deferWallTokS, func(c *config.Config) {
		c.StateDir = root
		c.AgentAdmissionWaitSec = 2
	})

	got := callSizedRun(t, context.Background(), s, "")
	if got["defer_class"] != string(core.DeferClassCapacity) || !strings.Contains(fmt.Sprint(got["reason"]), "video render") {
		t.Fatalf("test premise: want the window-probe fence defer, got %v", got)
	}
	requireDeferCarriesSizing(t, got)
}

// TestAgentRunUnknownProfileDeferCarriesTheSizing: a profile the door does not know is
// a clean defer, and it comes after the whole admission sequence.
func TestAgentRunUnknownProfileDeferCarriesTheSizing(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := wallGateServerFor(t, srv.URL, deferWallTokS, nil)

	m := callSizedRun(t, context.Background(), s, `,"profile":"no-such-profile"`)
	if reason, _ := m["reason"].(string); !strings.Contains(reason, "no-such-profile") {
		t.Fatalf("test premise: want the unknown-profile defer, got %v", m)
	}
	requireDeferCarriesSizing(t, m)
}

// TestEveryAgentRunDeferralPastTheWallGateGoesThroughTheSizingStamp is the drift guard
// for the exits no test above can reach (the agent builder failing, a browse grant the
// builder withheld) and for the next one somebody adds. A deferral that builds its own
// result with jsonResult drops the sizing, and nothing in a passing suite shows it; so
// the source is the fixture, the way TestEveryToolHandlerRequestCarriesADoor makes it.
//
// After the sizing is computed, handleAgentRun may call jsonResult exactly twice: once
// inside deferSized, which stamps the numbers on whatever deferral it is handed, and
// once for the answer.
func TestEveryAgentRunDeferralPastTheWallGateGoesThroughTheSizingStamp(t *testing.T) {
	src, err := os.ReadFile("mcpserver.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	start := strings.Index(text, "func (s *Server) handleAgentRun(")
	if start < 0 {
		t.Fatal("handleAgentRun not found in mcpserver.go")
	}
	body := text[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	gate := strings.Index(body, "sizing := s.p.SizeRun(")
	if gate < 0 {
		t.Fatal("the wall sizing call not found in handleAgentRun")
	}
	var raw []string
	for _, ln := range strings.Split(body[gate:], "\n") {
		if strings.Contains(ln, "jsonResult(") {
			raw = append(raw, strings.TrimSpace(ln))
		}
	}
	if len(raw) != 2 {
		t.Errorf("handleAgentRun calls jsonResult %d times after the wall sizing, want 2 (deferSized's own, and the answer): "+
			"a deferral must return through deferSized so it carries wall_estimate_sec / min_turn_sec / wall_note\n  %s",
			len(raw), strings.Join(raw, "\n  "))
	}
}
