package llamaclient

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- the cascade seat guard's two client hooks ---
//
// The pipeline decides, per rung, that a call must not load on this box (the
// rung would evict a loaded vLLM seat — internal/seatguard). Two things must
// then hold on the client: the pipeline can ask, without side effects,
// whether a lane would carry the call (OffBoxFor), and the send honours the
// decision (WithLocalBusy) even though neither lane busy gate fires — a loaded
// seat with nothing in flight is not "busy" to C-41, which is exactly the
// eviction the guard exists to stop. Residency is still required: routing
// never changes WHICH model answers, only WHERE.

const e4bRoster = `{"object":"list","data":[{"id":"gemma-4-e4b","meta":{"llamaswap":{"aliases":["offload-e4b"]}}}]}`

func quietGates() (func() bool, func(string) (bool, string)) {
	return func() bool { return false }, func(string) (bool, string) { return false, "" }
}

func TestWithLocalBusyRidesAResidentLane(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	var laneChat, laneRoster, localChat, localRoster atomic.Int64
	lane := laneServer(t, &laneChat, &laneRoster, e4bRoster)
	defer lane.Close()
	local := laneServer(t, &localChat, &localRoster, e4bRoster)
	defer local.Close()
	busy, busyFor := quietGates()
	c := New(local.URL, "", "gemma-4-e4b", 5*time.Second).
		WithRemoteLanes(map[string]string{"gemma-4-e4b": lane.URL}, busy, busyFor, RosterResident())

	// Control: no gate fires and no option is passed — the call stays home.
	if _, err := c.Generate(context.Background(), "gemma-4-e4b", "s", "u", "", 16, 0, 0); err != nil {
		t.Fatalf("control Generate: %v", err)
	}
	if localChat.Load() != 1 || laneChat.Load() != 0 {
		t.Fatalf("control: local=%d lane=%d, want the call on this box", localChat.Load(), laneChat.Load())
	}

	const why = "cascade seat guard: model=gemma-4-e4b evict=[qwen3.8-27b-vllm-3card]"
	if _, err := c.Generate(context.Background(), "gemma-4-e4b", "s", "u", "", 16, 0, 0, WithLocalBusy(why)); err != nil {
		t.Fatalf("guarded Generate: %v", err)
	}
	if laneChat.Load() != 1 || localChat.Load() != 1 {
		t.Fatalf("guarded: local=%d lane=%d, want the call on the lane", localChat.Load(), laneChat.Load())
	}
	if out := buf.String(); !strings.Contains(out, "cascade remote lane: gemma-4-e4b -> "+lane.URL) || !strings.Contains(out, why) {
		t.Fatalf("serve log = %q, want the lane line carrying the guard's reason", out)
	}
}

// TestWithLocalBusyNeverFallsThroughToThisBox (review HIGH 2): the pipeline
// planned the lane from the cached residency, and by the send the lane is no
// longer resident. The old send fell through to the LOCAL endpoint — the very
// eviction the caller had ruled out — without a word. Now the call is refused
// with ErrLaneUnavailable before any request is made, so the caller can take
// its own non-evicting door (the loaded seat) and log the divergence.
func TestWithLocalBusyNeverFallsThroughToThisBox(t *testing.T) {
	var laneChat, laneRoster, localChat, localRoster atomic.Int64
	lane := laneServer(t, &laneChat, &laneRoster, `{"object":"list","data":[{"id":"some-other-model"}]}`)
	defer lane.Close()
	local := laneServer(t, &localChat, &localRoster, e4bRoster)
	defer local.Close()
	busy, busyFor := quietGates()
	c := New(local.URL, "", "gemma-4-e4b", 5*time.Second).
		WithRemoteLanes(map[string]string{"gemma-4-e4b": lane.URL}, busy, busyFor, RosterResident())
	_, err := c.Generate(context.Background(), "gemma-4-e4b", "s", "u", "", 16, 0, 0, WithLocalBusy("guard"))
	if !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("err = %v, want ErrLaneUnavailable", err)
	}
	if laneChat.Load()+localChat.Load() != 0 {
		t.Fatalf("local=%d lane=%d: a refused call must reach no server at all", localChat.Load(), laneChat.Load())
	}
}

// TestWithLocalBusyWithoutALaneIsRefused: a client with no lane for the model
// cannot honour "not on this box" either, so it refuses rather than serving
// the call locally. (The pipeline only sends the option after OffBoxFor said
// a lane would carry it.) A seat pinned to another node is off-box already
// and goes there as always.
func TestWithLocalBusyWithoutALaneIsRefused(t *testing.T) {
	var localChat, localRoster, pinChat, pinRoster atomic.Int64
	local := laneServer(t, &localChat, &localRoster, e4bRoster)
	defer local.Close()
	pin := laneServer(t, &pinChat, &pinRoster, e4bRoster)
	defer pin.Close()
	c := New(local.URL, "", "gemma-4-e4b", 5*time.Second)
	if _, err := c.Generate(context.Background(), "gemma-4-e4b", "s", "u", "", 16, 0, 0, WithLocalBusy("guard")); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("no lane table: err = %v, want ErrLaneUnavailable", err)
	}
	if localChat.Load() != 0 {
		t.Fatalf("local=%d: the call must not be served on this box", localChat.Load())
	}
	pinned := New(local.URL, "", "gemma-4-e4b", 5*time.Second).WithSeatEndpoints(map[string]string{"gemma-4-e4b": pin.URL})
	pinned.safeHTTP = pinned.http // the pin fake is loopback; the tailnet dial gate is not what is under test
	if _, err := pinned.Generate(context.Background(), "gemma-4-e4b", "s", "u", "", 16, 0, 0, WithLocalBusy("guard")); err != nil {
		t.Fatalf("pinned seat: %v", err)
	}
	if pinChat.Load() != 1 || localChat.Load() != 0 {
		t.Fatalf("pinned seat: pin=%d local=%d, want the pinned node", pinChat.Load(), localChat.Load())
	}
}

// TestOffBoxFor: the side-effect-free question the pipeline asks before it
// keeps a guarded rung. A seat pinned to another node and a resident lane are
// off-box; a lane that does not serve the model, an unlaned model and a
// client with no lanes are not. It never consults the busy gates.
func TestOffBoxFor(t *testing.T) {
	var laneChat, laneRoster, deadChat, deadRoster atomic.Int64
	lane := laneServer(t, &laneChat, &laneRoster, e4bRoster)
	defer lane.Close()
	notServing := laneServer(t, &deadChat, &deadRoster, `{"object":"list","data":[{"id":"x"}]}`)
	defer notServing.Close()
	gateCalls := atomic.Int64{}
	busy := func() bool { gateCalls.Add(1); return false }
	busyFor := func(string) (bool, string) { gateCalls.Add(1); return false, "" }

	c := New("http://127.0.0.1:11436", "", "gemma-4-e4b", time.Second).
		WithSeatEndpoints(map[string]string{"pinned-seat": "http://node-c:11436"}).
		WithRemoteLanes(map[string]string{"gemma-4-e4b": lane.URL, "gemma-4-e2b": notServing.URL}, busy, busyFor, RosterResident())
	for model, want := range map[string]bool{
		"gemma-4-e4b": true,  // resident lane
		"offload-e4b": false, // no lane is keyed by the alias
		"gemma-4-e2b": false, // lane does not serve it
		"gemma-4-12b": false, // no lane at all
		"pinned-seat": true,  // seat_endpoints pins it to another node
	} {
		if got := c.OffBoxFor(model); got != want {
			t.Errorf("OffBoxFor(%s) = %v, want %v", model, got, want)
		}
	}
	if gateCalls.Load() != 0 {
		t.Fatalf("OffBoxFor consulted the busy gates %d time(s); it asks residency only", gateCalls.Load())
	}
	if laneChat.Load()+deadChat.Load() != 0 {
		t.Fatal("OffBoxFor sent a chat request; it must only read residency")
	}
	if got := New("http://127.0.0.1:11436", "", "m", time.Second).OffBoxFor("m"); got {
		t.Fatal("a client with no lanes and no pins: OffBoxFor must be false")
	}
}
