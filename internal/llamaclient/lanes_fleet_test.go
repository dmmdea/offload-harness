// Fleet-node cascade lanes (C-41b). C-41's first half taught the lane to fire
// when a local swap would WAIT; this half is what makes the lane reach
// anything at all in the real deployment. Both fleet nodes bind llama-swap to
// 127.0.0.1:11436 and nothing else, so the roster probe that backed every lane
// could never answer from another box: the lane would log "not resident" for
// ever and the measured C-41 symptom would still reproduce with the feature
// "shipped". These tests are therefore written against a node-shaped base —
// health on :18811, the chat door behind a bearer — and the last one pins that
// a plain llama-swap lane still behaves exactly as it did.
package llamaclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fleetNode fakes a harness fleet node: GET /fleet/health as the delegator
// reads it, and POST /fleet/chat recording the credential it was handed.
type fleetNode struct {
	srv       *httptest.Server
	chatHits  atomic.Int64
	authSeen  atomic.Value // string
	bodySeen  atomic.Value // string
	healthHit atomic.Int64
}

func newFleetNode(t *testing.T, nodeID string, chatLane bool, served []string) *fleetNode {
	t.Helper()
	n := &fleetNode{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/fleet/health":
			n.healthHit.Add(1)
			payload := map[string]any{"node_id": nodeID, "served_models": served}
			if chatLane {
				payload["chat_lane"] = true
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(payload); err != nil {
				t.Errorf("encode health: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == FleetChatPath:
			n.chatHits.Add(1)
			n.authSeen.Store(r.Header.Get("Authorization"))
			b := make([]byte, r.ContentLength)
			if _, err := r.Body.Read(b); err != nil && len(b) == 0 {
				t.Errorf("read chat body: %v", err)
			}
			n.bodySeen.Store(string(b))
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(cannedChatResp)); err != nil {
				t.Errorf("write canned response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fleetNode) auth() string {
	v, _ := n.authSeen.Load().(string)
	return v
}

// busyClient builds the production-shaped client: a busy local swap, one lane,
// and the REAL FleetLaneGates pair over the given token.
func busyClient(t *testing.T, laneBase, token string) *Client {
	t.Helper()
	s := newLocalSwap(t)
	s.set([]string{"qwen3.8-27b-vllm:ready"}, map[string]int{"qwen3.8-27b-vllm": 1})
	resident, route := FleetLaneGates(token)
	return New("http://127.0.0.1:11436", "", "gemma-4-e4b", 5*time.Second).
		WithRemoteLanes(map[string]string{"gemma-4-e4b": laneBase},
			func() bool { return false }, // no GPU lease: only the C-41 seat gate may fire
			LocalSwapBusy(s.srv.URL),
			resident).WithLaneRoute(route)
}

// TestFleetNodeLaneRidesTheChatDoor is the end-to-end proof this register item
// turns on: with the local seat busy and a NODE-shaped lane that serves the
// model, the call leaves the box through POST /fleet/chat with the fleet
// bearer — not through a llama-swap port nothing on this machine can reach.
func TestFleetNodeLaneRidesTheChatDoor(t *testing.T) {
	node := newFleetNode(t, "node-b", true, []string{"gemma4-e4b", "gemma-4-e4b"})
	c := busyClient(t, node.srv.URL, "s3cret")

	ep := c.resolveEndpoint("gemma-4-e4b")
	if ep.base != node.srv.URL {
		t.Fatalf("base = %q, want the node %q", ep.base, node.srv.URL)
	}
	if ep.path != FleetChatPath {
		t.Fatalf("path = %q, want %q — a node lane's llama-swap is loopback-only", ep.path, FleetChatPath)
	}
	if ep.token != "s3cret" {
		t.Fatalf("token = %q, want the fleet token", ep.token)
	}
	if ep.client != c.safeHTTP {
		t.Error("a lane call must ride the tailnet-guarded client")
	}

	res, err := c.Generate(context.Background(), "gemma-4-e4b", "sys", "user", "", 32, 0, 0)
	if err != nil {
		t.Fatalf("Generate through the node lane: %v", err)
	}
	if res.Content == "" {
		t.Fatal("no content came back from the node lane")
	}
	if node.chatHits.Load() != 1 {
		t.Fatalf("chat hits = %d, want exactly 1 on the node's door", node.chatHits.Load())
	}
	if node.auth() != "Bearer s3cret" {
		t.Fatalf("Authorization = %q, want the fleet bearer", node.auth())
	}
}

// TestFleetNodeLaneFailsClosed covers every way a node lane must resolve back
// to LOCAL instead of being called: the node does not advertise the door, the
// node does not serve the model, or this box holds no token to present past
// loopback. Each would otherwise be a hard failure on a cascade call that
// could simply have queued at home.
func TestFleetNodeLaneFailsClosed(t *testing.T) {
	const local = "http://127.0.0.1:11436"

	t.Run("no chat_lane advertised: stays local", func(t *testing.T) {
		node := newFleetNode(t, "node-c", false, []string{"gemma-4-e4b"})
		c := busyClient(t, node.srv.URL, "s3cret")
		if ep := c.resolveEndpoint("gemma-4-e4b"); ep.base != local || ep.client != c.http {
			t.Fatalf("base = %q, want the local base %q on the default client", ep.base, local)
		}
		if node.chatHits.Load() != 0 {
			t.Fatal("a node that does not advertise the door must never be called")
		}
	})

	t.Run("node does not serve the model: stays local", func(t *testing.T) {
		node := newFleetNode(t, "node-b", true, []string{"gemma-4-e2b"})
		c := busyClient(t, node.srv.URL, "s3cret")
		if ep := c.resolveEndpoint("gemma-4-e4b"); ep.base != local {
			t.Fatalf("base = %q, want the local base %q", ep.base, local)
		}
		if node.chatHits.Load() != 0 {
			t.Fatal("a node that does not serve the model must never be called")
		}
	})

	t.Run("no fleet token for a non-loopback node: stays local", func(t *testing.T) {
		// The base cannot be a live httptest server here (those are loopback,
		// where a tokenless lane is legitimate), so the gate itself is driven
		// on a tailnet-shaped base — exactly the production shape.
		resident, route := FleetLaneGates("")
		if fleetTokenUsable("http://node-b:18811", "") {
			t.Fatal("a tailnet node base with no token must not be considered usable")
		}
		if !fleetTokenUsable("http://127.0.0.1:18811", "") {
			t.Fatal("a loopback node admits the lane tokenless, as the node itself does")
		}
		if !fleetTokenUsable("http://node-b:18811", "s3cret") {
			t.Fatal("a token makes a tailnet node base usable")
		}
		// And the gates agree: an unreachable/non-node base is never resident
		// and never routed.
		if resident("http://node-b:18811", "gemma-4-e4b") {
			t.Fatal("a lane with no credential must not be resident")
		}
		if p, tok := route("http://node-b:18811"); p != "" || tok != "" {
			t.Fatalf("route = (%q, %q), want the empty llama-swap route", p, tok)
		}
	})
}

// TestPlainSwapLaneKeepsItsOwnRoute is the regression half: a lane base that
// is a plain llama-swap (it 404s /fleet/health) still resolves through
// /v1/models and still rides the client's own generation path with NO
// Authorization header. The node lane is additive, not a replacement.
func TestPlainSwapLaneKeepsItsOwnRoute(t *testing.T) {
	var chatHits, rosterHits atomic.Int64
	lane := laneServer(t, &chatHits, &rosterHits,
		`{"object":"list","data":[{"id":"gemma-4-e4b"}]}`)
	defer lane.Close()

	c := busyClient(t, lane.URL, "s3cret")
	ep := c.resolveEndpoint("gemma-4-e4b")
	if ep.base != lane.URL {
		t.Fatalf("base = %q, want the lane %q", ep.base, lane.URL)
	}
	if ep.path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want the client's own generation path", ep.path)
	}
	if ep.token != "" {
		t.Fatalf("token = %q, want no credential on a plain llama-swap lane", ep.token)
	}
	if _, err := c.Generate(context.Background(), "gemma-4-e4b", "sys", "user", "", 32, 0, 0); err != nil {
		t.Fatalf("Generate through the swap lane: %v", err)
	}
	if chatHits.Load() != 1 {
		t.Fatalf("chat hits = %d, want exactly 1 on the lane's llama-swap", chatHits.Load())
	}
}

// TestFleetLaneProbeIsCachedPerBase: one probe per base per window, whichever
// shape the base turns out to be — the residency gate and the router must
// share it, or a node lane would cost two health GETs per cascade call.
func TestFleetLaneProbeIsCachedPerBase(t *testing.T) {
	node := newFleetNode(t, "node-b", true, []string{"gemma-4-e4b"})
	resident, route := FleetLaneGates("s3cret")
	for i := 0; i < 5; i++ {
		if !resident(node.srv.URL, "gemma-4-e4b") {
			t.Fatalf("call %d: want resident", i)
		}
		if p, _ := route(node.srv.URL); p != FleetChatPath {
			t.Fatalf("call %d: route = %q, want %q", i, p, FleetChatPath)
		}
	}
	if got := node.healthHit.Load(); got != 1 {
		t.Fatalf("health probes = %d, want exactly 1 for ten gate calls in one window", got)
	}
}

// TestFleetLaneIgnoresA200ThatIsNotANode: a 200 at /fleet/health with no
// node_id (a captive portal, a reverse proxy, anything) must not be mistaken
// for a fleet node — the lane falls through to the llama-swap roster, which is
// what the base actually is.
func TestFleetLaneIgnoresA200ThatIsNotANode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fleet/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gemma-4-e4b"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	resident, route := FleetLaneGates("s3cret")
	if !resident(srv.URL, "gemma-4-e4b") {
		t.Fatal("the base is a plain llama-swap and serves the model: want resident")
	}
	if p, tok := route(srv.URL); p != "" || tok != "" {
		t.Fatalf("route = (%q, %q), want the empty llama-swap route", p, tok)
	}
}
