package llamaclient

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- roast delta 7: busy-aware cascade remote lanes ---

// laneServer serves BOTH halves of what a remote lane is: chat completions
// (counted into chatHits) and the llama-swap roster on /v1/models (counted
// into rosterHits) listing the given model ids — so the real RosterResident
// prober can verify residency against it, end to end.
func laneServer(t *testing.T, chatHits, rosterHits *atomic.Int64, rosterJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			rosterHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(rosterJSON)); err != nil {
				t.Errorf("write roster: %v", err)
			}
		case "/v1/chat/completions":
			chatHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(cannedChatResp)); err != nil {
				t.Errorf("write canned response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestResolveEndpointOrder locks the per-request resolution order (delta 7):
// the static seat_endpoints override wins unconditionally; else a configured
// lane is taken only when the local GPU is busy AND the lane roster-serves the
// model; every other combination — lane idle, lane non-resident, no lanes at
// all — stays on the default base with the default (unguarded) client.
func TestResolveEndpointOrder(t *testing.T) {
	const (
		defBase  = "http://127.0.0.1:11436"
		seatBase = "http://node-c:11436"
		laneBase = "http://workstation:11436"
	)
	mk := func(withSeat, withLanes, busy, resident bool) *Client {
		c := New(defBase, "", "offload-e4b", time.Second)
		if withSeat {
			c = c.WithSeatEndpoints(map[string]string{"offload-e4b": seatBase})
		}
		if withLanes {
			c = c.WithRemoteLanes(
				map[string]string{"offload-e4b": laneBase},
				func() bool { return busy },
				func(base, model string) bool { return resident },
			)
		}
		return c
	}
	cases := []struct {
		name                string
		withSeat, withLanes bool
		busy, resident      bool
		wantBase            string
		wantSafe            bool // true = must ride the tailnet-guarded client
	}{
		{"static override wins even over a busy resident lane", true, true, true, true, seatBase, true},
		{"lane busy and resident routes to the lane", false, true, true, true, laneBase, true},
		{"lane busy but non-resident stays local", false, true, true, false, defBase, false},
		{"lane resident but idle stays local", false, true, false, true, defBase, false},
		{"no lanes stays local", false, false, false, false, defBase, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mk(tc.withSeat, tc.withLanes, tc.busy, tc.resident)
			base, hc := c.resolveEndpoint("offload-e4b")
			if base != tc.wantBase {
				t.Errorf("base = %q, want %q", base, tc.wantBase)
			}
			if tc.wantSafe && hc != c.safeHTTP {
				t.Error("resolved client is not the tailnet-guarded one")
			}
			if !tc.wantSafe && hc != c.http {
				t.Error("resolved client is not the default one")
			}
		})
	}
}

// TestWithRemoteLanesEmptyIsIdentity pins the additive-off contract (the
// absent-key byte-identical guarantee): an empty/nil lane map — or missing
// gate funcs, which could never fire safely — installs NOTHING: no lane
// table, no gates, no second HTTP client.
func TestWithRemoteLanesEmptyIsIdentity(t *testing.T) {
	busy := func() bool { return true }
	resident := func(base, model string) bool { return true }
	pristine := func(t *testing.T, c *Client) {
		t.Helper()
		if c.remoteLanes != nil || c.laneBusy != nil || c.laneResident != nil || c.safeHTTP != nil {
			t.Fatal("client must stay byte-identical to a pre-lanes build")
		}
	}

	c := New("http://127.0.0.1:11436", "", "offload-e4b", time.Second)
	if got := c.WithRemoteLanes(nil, busy, resident); got != c {
		t.Fatal("WithRemoteLanes(nil map) must return the same client")
	}
	pristine(t, c)
	if got := c.WithRemoteLanes(map[string]string{}, busy, resident); got != c {
		t.Fatal("WithRemoteLanes(empty map) must return the same client")
	}
	pristine(t, c)
	if got := c.WithRemoteLanes(map[string]string{"m": "http://workstation:11436"}, nil, resident); got != c {
		t.Fatal("WithRemoteLanes(nil busy) must return the same client")
	}
	pristine(t, c)
	if got := c.WithRemoteLanes(map[string]string{"m": "http://workstation:11436"}, busy, nil); got != c {
		t.Fatal("WithRemoteLanes(nil resident) must return the same client")
	}
	pristine(t, c)
}

// TestCascadeRemoteLaneMovesWithBusy is the end-to-end proof: two live fake
// servers, the REAL RosterResident prober against the lane's roster endpoint,
// and a flip-able busy func. Requests actually move to the lane when busy
// flips on and come home when it flips off — and the roster is probed ONCE
// for both busy calls (the 30s per-base cache).
func TestCascadeRemoteLaneMovesWithBusy(t *testing.T) {
	var defaultHits atomic.Int64
	defSrv := countingServer(t, &defaultHits)
	defer defSrv.Close()

	var laneChat, laneRoster atomic.Int64
	lane := laneServer(t, &laneChat, &laneRoster,
		`{"object":"list","data":[{"id":"canonical-e4b","meta":{"llamaswap":{"aliases":["offload-e4b"]}}}]}`)
	defer lane.Close()

	var mu sync.Mutex
	busy := false
	setBusy := func(v bool) { mu.Lock(); busy = v; mu.Unlock() }
	isBusy := func() bool { mu.Lock(); defer mu.Unlock(); return busy }

	c := New(defSrv.URL, "", "offload-e4b", 5*time.Second).
		WithRemoteLanes(map[string]string{"offload-e4b": lane.URL}, isBusy, RosterResident())

	gen := func(step string) {
		t.Helper()
		if _, err := c.Generate(context.Background(), "", "sys", "hi", "", 16, 0, 0); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	gen("idle call")
	if defaultHits.Load() != 1 || laneChat.Load() != 0 {
		t.Fatalf("idle: hits = (default %d, lane %d), want (1, 0)", defaultHits.Load(), laneChat.Load())
	}

	setBusy(true)
	gen("busy call 1")
	gen("busy call 2")
	if defaultHits.Load() != 1 || laneChat.Load() != 2 {
		t.Fatalf("busy: hits = (default %d, lane %d), want (1, 2)", defaultHits.Load(), laneChat.Load())
	}
	if laneRoster.Load() != 1 {
		t.Fatalf("roster probes = %d, want exactly 1 (cached across the second busy call)", laneRoster.Load())
	}

	setBusy(false)
	gen("idle again")
	if defaultHits.Load() != 2 || laneChat.Load() != 2 {
		t.Fatalf("idle again: hits = (default %d, lane %d), want (2, 2)", defaultHits.Load(), laneChat.Load())
	}
}

// TestCascadeRemoteLaneFailsClosedOnProbeError: a lane whose roster cannot be
// read (here: a 404 on /v1/models) is NOT resident — the busy call stays on
// the default base instead of gambling on an unverified lane. "The roster is
// empty" and "the roster could not be read" both fail toward local.
func TestCascadeRemoteLaneFailsClosedOnProbeError(t *testing.T) {
	var defaultHits atomic.Int64
	defSrv := countingServer(t, &defaultHits)
	defer defSrv.Close()

	var laneChat atomic.Int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			laneChat.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer dead.Close()

	c := New(defSrv.URL, "", "offload-e4b", 5*time.Second).
		WithRemoteLanes(map[string]string{"offload-e4b": dead.URL},
			func() bool { return true }, RosterResident())

	if _, err := c.Generate(context.Background(), "", "sys", "hi", "", 16, 0, 0); err != nil {
		t.Fatalf("busy call with an unprobeable lane must still answer locally: %v", err)
	}
	if defaultHits.Load() != 1 || laneChat.Load() != 0 {
		t.Fatalf("hits = (default %d, lane %d), want (1, 0) — fail-closed to local", defaultHits.Load(), laneChat.Load())
	}
}

// TestRosterResidentRefusesOffTailnetAtDial closes the one outbound hole in
// the delegation lanes. Five of six outbound paths ride netguard.SafeTransport;
// this lane's ROSTER PROBE did not — swapclient.New passed only a Timeout, so
// the vendored client built a plain &http.Client{} on the DEFAULT transport,
// with ProxyFromEnvironment live and no dial-time address check. That made
// ADR 0023's "re-checked at every dial" claim false for this path: a dotless
// MagicDNS name admitted by SHAPE alone (TailnetURL cannot resolve at config
// time, by design) could resolve off-tailnet and the probe would happily
// connect to it.
//
// 203.0.113.0/24 is RFC 5737 TEST-NET-3: unroutable, so an unguarded dial
// merely times out (and the failure message never names the guard), while a
// guarded one is refused at the socket with zero network activity.
func TestRosterResidentRefusesOffTailnetAtDial(t *testing.T) {
	buf := &syncBuffer{}
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	resident := RosterResident()
	start := time.Now()
	if resident("http://203.0.113.9:11436", "offload-e4b") {
		t.Fatal("an off-tailnet lane must never read as resident")
	}
	if elapsed := time.Since(start); elapsed > laneProbeTimeout/2 {
		t.Errorf("refusal took %s — a dial-time refusal costs zero network activity, so it must be immediate", elapsed)
	}
	if out := buf.String(); !strings.Contains(out, "tailnet guard") {
		t.Fatalf("probe-failure log = %q, want the tailnet guard's refusal — an off-tailnet base must die at DIAL, not merely fail to connect", out)
	}
}

// syncBuffer is a log sink safe to write from a probe goroutine and read from
// the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRosterResidentLogsProbeFailureOncePerWindow (M-1): fail-closed is right,
// fail-SILENT is not. A successful reroute logs a line, so a lane that quietly
// never engages leaves no trace at all — the operator sees cascade calls
// staying local during a render and has nothing to look at. The failure is
// announced once per TTL window per base (the probe itself runs at most that
// often), never once per call.
func TestRosterResidentLogsProbeFailureOncePerWindow(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer dead.Close()

	resident := RosterResident()
	for i := 0; i < 3; i++ {
		if resident(dead.URL, "offload-e4b") {
			t.Fatal("an unprobeable lane must never read as resident")
		}
	}
	out := buf.String()
	if n := strings.Count(out, dead.URL); n != 1 {
		t.Fatalf("probe-failure lines naming the lane = %d, want exactly 1 for the window (log: %s)", n, out)
	}
	if !strings.Contains(out, "roster probe") {
		t.Fatalf("log = %q, want it to name the failed roster probe", out)
	}
}
