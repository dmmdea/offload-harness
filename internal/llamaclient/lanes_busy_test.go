package llamaclient

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- C-41: the lane also fires when a SWAP would wait ---
//
// The lease gate (delegate.LocalBusy) sees only the harness's own machine-wide
// GPU lease. The measured C-41 symptom has no lease at all: another session
// holds the Qube's `qwen3.8-27b-vllm` seat through llama-swap, the
// `interactive` set is mutually exclusive, so every cascade tier needs a swap
// — and llama-swap swaps only after the loaded model's in-flight requests
// finish (300–900 s contracts). The cascade call sits in that queue until its
// own HTTP deadline and the tier defers. These tests pin the second gate:
// "a model OTHER than the one I asked for is loaded and cannot be swapped out
// yet" is busy, per model, alias-aware, and fail-closed.

// localSwap is a fake of THIS box's llama-swap: the roster (/v1/models, with
// llama-swap's alias shape), /running, and the per-model metrics exposition
// behind /upstream/<id>/metrics that seatload reads the in-flight count from.
type localSwap struct {
	srv *httptest.Server

	mu       sync.Mutex
	running  []string // "<id>:<state>"
	inflight map[string]int
	broken   bool // /running answers 500 — the probe cannot read the box

	runningHits atomic.Int64
}

const localSwapRoster = `{"object":"list","data":[
	{"id":"qwen3.8-27b-vllm","meta":{"llamaswap":{"aliases":["agent-pool"]}}},
	{"id":"gemma-4-e4b","meta":{"llamaswap":{"aliases":["offload-e4b"]}}}
]}`

func newLocalSwap(t *testing.T) *localSwap {
	t.Helper()
	s := &localSwap{inflight: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		broken, running := s.broken, append([]string(nil), s.running...)
		inflight := make(map[string]int, len(s.inflight))
		for k, v := range s.inflight {
			inflight[k] = v
		}
		s.mu.Unlock()

		switch {
		case r.URL.Path == "/v1/models":
			if broken {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(localSwapRoster)); err != nil {
				t.Errorf("write roster: %v", err)
			}
		case r.URL.Path == "/running":
			s.runningHits.Add(1)
			if broken {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			rows := make([]string, 0, len(running))
			for _, e := range running {
				id, state, _ := strings.Cut(e, ":")
				rows = append(rows, fmt.Sprintf(`{"model":%q,"state":%q}`, id, state))
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprintf(w, `{"running":[%s]}`, strings.Join(rows, ",")); err != nil {
				t.Errorf("write running: %v", err)
			}
		case strings.HasPrefix(r.URL.Path, "/upstream/") && strings.HasSuffix(r.URL.Path, "/metrics"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upstream/"), "/metrics")
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "# HELP vllm:num_requests_running running\nvllm:num_requests_running %d\n", inflight[id])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *localSwap) set(running []string, inflight map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = running
	s.inflight = inflight
}

func (s *localSwap) breakIt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken = true
}

// TestLocalSwapBusyForReadsTheSwapQueue is the gate itself: "would a swap have
// to wait?" answered per model against this box's llama-swap.
func TestLocalSwapBusyForReadsTheSwapQueue(t *testing.T) {
	cases := []struct {
		name     string
		running  []string
		inflight map[string]int
		ask      string
		want     bool
		wantWhy  []string
	}{
		{
			name:     "another model loaded and working: a swap would wait",
			running:  []string{"qwen3.8-27b-vllm:ready"},
			inflight: map[string]int{"qwen3.8-27b-vllm": 1},
			ask:      "gemma-4-e4b",
			want:     true,
			wantWhy:  []string{"qwen3.8-27b-vllm", "1 in flight"},
		},
		{
			name:     "another model loaded but IDLE: llama-swap swaps at once",
			running:  []string{"qwen3.8-27b-vllm:ready"},
			inflight: map[string]int{"qwen3.8-27b-vllm": 0},
			ask:      "gemma-4-e4b",
			want:     false,
		},
		{
			name:     "another model STARTING: the swap is already under way",
			running:  []string{"qwen3.8-27b-vllm:starting"},
			inflight: map[string]int{},
			ask:      "gemma-4-e4b",
			want:     true,
			wantWhy:  []string{"qwen3.8-27b-vllm", "starting"},
		},
		{
			name:     "the requested model is the loaded one: no swap at all",
			running:  []string{"gemma-4-e4b:ready"},
			inflight: map[string]int{"gemma-4-e4b": 3},
			ask:      "gemma-4-e4b",
			want:     false,
		},
		{
			name:     "the requested model is the loaded one under its ALIAS",
			running:  []string{"gemma-4-e4b:ready"}, // /running lists canonical ids
			inflight: map[string]int{"gemma-4-e4b": 3},
			ask:      "offload-e4b", // the harness binds the alias
			want:     false,
		},
		{
			name:     "nothing loaded: the cascade tier loads on a free card",
			running:  nil,
			inflight: map[string]int{},
			ask:      "gemma-4-e4b",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newLocalSwap(t)
			s.set(tc.running, tc.inflight)
			busy, why := LocalSwapBusy(s.srv.URL)(tc.ask)
			if busy != tc.want {
				t.Fatalf("busy = %v (%q), want %v", busy, why, tc.want)
			}
			for _, w := range tc.wantWhy {
				if !strings.Contains(why, w) {
					t.Errorf("why = %q, want it to contain %q", why, w)
				}
			}
			if !busy && why != "" {
				t.Errorf("an idle box must carry no reason, got %q", why)
			}
		})
	}
}

// TestLocalSwapBusyForFailsClosed: a box whose llama-swap cannot be read is
// NOT busy — exactly like RosterResident, every probe error resolves to local.
// "Could not tell" must never move a call off this machine.
func TestLocalSwapBusyForFailsClosed(t *testing.T) {
	s := newLocalSwap(t)
	s.set([]string{"qwen3.8-27b-vllm:ready"}, map[string]int{"qwen3.8-27b-vllm": 1})
	s.breakIt()
	if busy, why := LocalSwapBusy(s.srv.URL)("gemma-4-e4b"); busy {
		t.Fatalf("an unreadable llama-swap must not read as busy (why = %q)", why)
	}
	if busy, why := LocalSwapBusy("")("gemma-4-e4b"); busy {
		t.Fatalf("an empty endpoint must not read as busy (why = %q)", why)
	}
}

// TestLocalSwapBusyForCachesPerBase: a burst of cascade calls costs ONE
// /running probe per window, not one each — the RosterResident discipline.
func TestLocalSwapBusyForCachesPerBase(t *testing.T) {
	s := newLocalSwap(t)
	s.set([]string{"qwen3.8-27b-vllm:ready"}, map[string]int{"qwen3.8-27b-vllm": 2})
	busyFor := LocalSwapBusy(s.srv.URL)
	if busy, _ := busyFor("gemma-4-e4b"); !busy {
		t.Fatal("first call: want busy")
	}
	first := s.runningHits.Load()
	if first == 0 {
		t.Fatal("the first call must actually probe /running")
	}
	for i := 0; i < 3; i++ {
		if busy, _ := busyFor("gemma-4-e4b"); !busy {
			t.Fatalf("call %d: want busy", i)
		}
	}
	// A DIFFERENT model asked in the same window reuses the same snapshot.
	if busy, _ := busyFor("gemma-4-e2b"); !busy {
		t.Fatal("a second cascade tier must read busy from the cached snapshot")
	}
	if n := s.runningHits.Load(); n != first {
		t.Fatalf("/running probes = %d after four more calls, want still %d — one probe per window", n, first)
	}
}

// TestResolveEndpointTakesLaneWhenLocalSeatBusy is the C-41 shape end to end
// through the resolver: no GPU lease anywhere, another session's seat loaded
// and working on the local llama-swap, a lane that roster-serves the SAME
// cascade model → the call rides the lane on the tailnet-guarded client, and
// the serve log names the busy model.
func TestResolveEndpointTakesLaneWhenLocalSeatBusy(t *testing.T) {
	var buf syncBuffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	var laneChat, laneRoster atomic.Int64
	lane := laneServer(t, &laneChat, &laneRoster,
		`{"object":"list","data":[{"id":"gemma-4-e4b","meta":{"llamaswap":{"aliases":["offload-e4b"]}}}]}`)
	defer lane.Close()
	var otherChat, otherRoster atomic.Int64
	notServing := laneServer(t, &otherChat, &otherRoster,
		`{"object":"list","data":[{"id":"qwen3.8-27b-vllm"}]}`)
	defer notServing.Close()

	const defBase = "http://127.0.0.1:11436"
	mk := func(laneBase string, s *localSwap) *Client {
		return New(defBase, "", "gemma-4-e4b", time.Second).
			WithRemoteLanes(map[string]string{"gemma-4-e4b": laneBase},
				func() bool { return false }, // no GPU lease held: the OLD gate is silent
				LocalSwapBusy(s.srv.URL),
				RosterResident())
	}

	t.Run("busy seat plus a serving lane routes to the lane", func(t *testing.T) {
		s := newLocalSwap(t)
		s.set([]string{"qwen3.8-27b-vllm:ready"}, map[string]int{"qwen3.8-27b-vllm": 1})
		c := mk(lane.URL, s)
		ep := c.resolveEndpoint("gemma-4-e4b")
		if ep.base != lane.URL {
			t.Fatalf("base = %q, want the lane %q", ep.base, lane.URL)
		}
		if ep.client != c.safeHTTP {
			t.Error("a lane call must ride the tailnet-guarded client")
		}
		if out := buf.String(); !strings.Contains(out, "qwen3.8-27b-vllm") || !strings.Contains(out, lane.URL) {
			t.Errorf("serve log = %q, want one line naming the busy model and the lane", out)
		}
	})

	t.Run("idle local seat stays local", func(t *testing.T) {
		s := newLocalSwap(t)
		s.set([]string{"qwen3.8-27b-vllm:ready"}, map[string]int{"qwen3.8-27b-vllm": 0})
		c := mk(lane.URL, s)
		if ep := c.resolveEndpoint("gemma-4-e4b"); ep.base != defBase || ep.client != c.http {
			t.Fatalf("base = %q, want the default base %q on the default client", ep.base, defBase)
		}
	})

	t.Run("busy local seat but the lane does not serve the model stays local", func(t *testing.T) {
		s := newLocalSwap(t)
		s.set([]string{"qwen3.8-27b-vllm:ready"}, map[string]int{"qwen3.8-27b-vllm": 1})
		c := mk(notServing.URL, s)
		if ep := c.resolveEndpoint("gemma-4-e4b"); ep.base != defBase || ep.client != c.http {
			t.Fatalf("base = %q, want the default base %q on the default client", ep.base, defBase)
		}
		if otherChat.Load() != 0 {
			t.Fatal("a non-serving lane must never receive a chat call")
		}
	})

	t.Run("the requested model is the loaded one: stays local", func(t *testing.T) {
		s := newLocalSwap(t)
		s.set([]string{"gemma-4-e4b:ready"}, map[string]int{"gemma-4-e4b": 2})
		c := mk(lane.URL, s)
		if ep := c.resolveEndpoint("gemma-4-e4b"); ep.base != defBase || ep.client != c.http {
			t.Fatalf("base = %q, want the default base %q on the default client", ep.base, defBase)
		}
	})
}
