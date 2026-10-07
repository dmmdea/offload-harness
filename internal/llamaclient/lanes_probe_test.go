package llamaclient

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// withLaneProbeTimeout compresses the lane probe bound for one test and restores it. The gates read
// it when they are built and when they probe, so set it before FleetLaneGates.
func withLaneProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := laneProbeTimeout
	laneProbeTimeout = d
	t.Cleanup(func() { laneProbeTimeout = prev })
}

// captureLog points the standard logger at w for one test and returns the restore.
func captureLog(w io.Writer) (restore func()) {
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(w)
	log.SetFlags(0)
	return func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) }
}

// liveLane is a plain llama-swap lane base: it 404s /fleet/health like one and lists its models.
func liveLane(t *testing.T, models ...string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var rosterHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		rosterHits.Add(1)
		data := make([]map[string]string, 0, len(models))
		for _, m := range models {
			data = append(data, map[string]string{"id": m})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &rosterHits
}

// A dead lane base used to cost TWO probe bounds under the lock: the health GET failed after a whole
// timeout and the roster GET then waited another. A base that does not answer the health GET at all
// does not serve a roster, so the second request is not made: one bound, one dial.
func TestADeadLaneBaseCostsOneProbeNotTwo(t *testing.T) {
	const bound = 300 * time.Millisecond
	withLaneProbeTimeout(t, bound)
	hole := rostertest.NewBlackHole(t)
	resident := RosterResident()

	start := time.Now()
	if resident(hole.URL(), "offload-e4b") {
		t.Fatal("a base that never answers must not read as resident")
	}
	wall := time.Since(start)
	if hole.Dials() != 1 {
		t.Errorf("a dead lane base was dialled %d times for one reading, want 1 (the roster probe must be skipped)", hole.Dials())
	}
	// Two bounds in series is 600 ms; one is 300 ms. The ceiling sits between them.
	if wall >= 500*time.Millisecond {
		t.Errorf("a dead lane base took %s to read, want about one bound (%s)", wall, bound)
	}
	// And it is cached like any other answer: a second read inside the window dials nothing.
	if resident(hole.URL(), "offload-e4b") || hole.Dials() != 1 {
		t.Errorf("second read dialled again (%d dials): a dead base is cached for the window", hole.Dials())
	}
}

// One dead lane base must not hold up a call that only needs another: the probe runs outside the
// cache lock. Under the old single mutex the live base's read waited behind the hung base's two
// probes.
func TestADeadLaneBaseDoesNotDelayALiveOne(t *testing.T) {
	withLaneProbeTimeout(t, 800*time.Millisecond)
	hole := rostertest.NewBlackHole(t)
	live, _ := liveLane(t, "offload-e4b")
	resident := RosterResident()

	probing := make(chan struct{})
	go func() {
		defer close(probing)
		resident(hole.URL(), "offload-e4b")
	}()
	deadline := time.Now().Add(2 * time.Second)
	for hole.Dials() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the dead base's probe never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	ok := resident(live.URL, "offload-e4b")
	wall := time.Since(start)
	if !ok {
		t.Fatal("the live lane base must read as resident")
	}
	if wall >= 300*time.Millisecond {
		t.Errorf("the live base waited %s behind the dead base's probe (its bound is 800 ms): the probe must not hold the lock", wall)
	}
	<-probing
}

// Many calls on one dead base share one probe: the first dials, the rest wait for its answer, and
// they all read it from the cache.
func TestConcurrentCallsOnOneDeadLaneBaseShareOneProbe(t *testing.T) {
	const bound = 300 * time.Millisecond
	withLaneProbeTimeout(t, bound)
	hole := rostertest.NewBlackHole(t)
	resident := RosterResident()

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resident(hole.URL(), "offload-e4b") {
				t.Error("a dead base read as resident")
			}
		}()
	}
	wg.Wait()
	if hole.Dials() != 1 {
		t.Errorf("6 concurrent calls dialled the dead base %d times, want 1 (one probe in flight per base)", hole.Dials())
	}
	if wall := time.Since(start); wall >= 2*bound {
		t.Errorf("6 concurrent calls took %s, want about one bound (%s): waiters must not each pay a probe", wall, bound)
	}
}

// A base that drops the connection on the health GET answered nothing: it is not asked for a roster
// either, whatever it would have said.
func TestAHealthProbeThatDropsTheConnectionSkipsTheRosterProbe(t *testing.T) {
	var rosterHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			rosterHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "offload-e4b"}}})
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server cannot hijack")
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close() // no answer at all: a reset
	}))
	defer srv.Close()

	if RosterResident()(srv.URL, "offload-e4b") {
		t.Fatal("a base whose health probe was dropped must not read as resident")
	}
	if n := rosterHits.Load(); n != 0 {
		t.Errorf("the roster was probed %d times after the health probe got no answer, want 0", n)
	}
}

// A body that stalls after the headers is no more an answer than silence: the bound ends the read,
// and the roster probe is skipped.
func TestAHealthBodyThatStallsIsNotARoster(t *testing.T) {
	withLaneProbeTimeout(t, 300*time.Millisecond)
	var rosterHits atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			rosterHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "offload-e4b"}}})
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release // headers sent, body never comes
	}))
	defer srv.Close()
	defer close(release)

	if RosterResident()(srv.URL, "offload-e4b") {
		t.Fatal("a base that never finished its health reply must not read as resident")
	}
	if n := rosterHits.Load(); n != 0 {
		t.Errorf("the roster was probed %d times after a health body that stalled, want 0", n)
	}
}

// What did answer but is not a node (a llama-swap 404s the health route) is still read through its
// roster, exactly as before: only silence skips it.
func TestABaseThatAnswersNotFoundIsStillReadThroughItsRoster(t *testing.T) {
	live, rosterHits := liveLane(t, "offload-e4b")
	resident := RosterResident()
	if !resident(live.URL, "offload-e4b") {
		t.Fatal("a llama-swap lane that lists the model must read as resident")
	}
	if rosterHits.Load() != 1 {
		t.Errorf("roster probed %d times, want 1", rosterHits.Load())
	}
	if resident(live.URL, "not-listed") {
		t.Error("a model the roster does not list must not read as resident")
	}
}

// The unreachable line names the base, says what the health probe found and that the roster probe
// was skipped, so a lane that never engages leaves a reason.
func TestADeadLaneBaseSaysWhyOnce(t *testing.T) {
	buf := &syncBuffer{}
	restore := captureLog(buf)
	t.Cleanup(restore)
	withLaneProbeTimeout(t, 200*time.Millisecond)
	hole := rostertest.NewBlackHole(t)
	resident := RosterResident()
	for i := 0; i < 3; i++ {
		resident(hole.URL(), "offload-e4b")
	}
	out := buf.String()
	if n := strings.Count(out, "nothing answered "+hole.URL()); n != 1 {
		t.Errorf("lines naming the dead base = %d, want 1 per window (log: %s)", n, out)
	}
	for _, want := range []string{"nothing answered", "roster probe is skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q must say %q", out, want)
		}
	}
}
