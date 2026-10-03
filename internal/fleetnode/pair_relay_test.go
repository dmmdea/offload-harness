package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// The member's side of the PAIR card relay (D26): POST /fleet/pair-relay. The relaying box is a
// view-only node (node-v) in these tests, at an RFC 5737 address.

const relayViewOnly = `[{"name":"node-v","address":"192.0.2.50","port":18811,"nodeUuid":"node-v-uuid"}]`

const relayBodyQueued = `{"jsonrpc":"2.0","method":"workload:submitted","params":{"workloadInfo":{` +
	`"id":"led-1-1","model":"gemma-4-e4b","engine":"llamacpp","runId":"led-1-1","state":"queued",` +
	`"originatedFrom":null,"scheduledOn":null,"createdAt":1000,"startedAt":null,"completedAt":null,` +
	`"error":null,"requesterId":"offload-harness/sess-9"}},"node":"node-v"}`

func relayOpts(pn *pairNode, loopback bool) *Options {
	o := pairOpts(pn)
	o.LoopbackListener = loopback
	return o
}

func relayCfg(token string) config.Config {
	c := imageCfg()
	c.FleetAuthToken = token
	return c
}

func viewOnlyNode(t *testing.T, pn *pairNode) {
	t.Helper()
	p := filepath.Join(pn.app, "configs", "view-only-nodes.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(relayViewOnly), 0o644); err != nil {
		t.Fatal(err)
	}
}

func relayHeaders(token, asker string) map[string]string {
	h := map[string]string{}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	if asker != "" {
		h[core.AskerHeader] = asker
	}
	return h
}

// The route rides the bearer rule of the other gated lanes, checked before the body is looked at:
// a wrong or missing token is a 401 whatever the body says, a tokenless node beyond loopback is a
// 403, and a tokenless loopback node stays open.
func TestPairRelayRouteIsTokenGated(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		loopback bool
		header   string
		want     int
	}{
		{"token set, no header", "s3cret", true, "", http.StatusUnauthorized},
		{"token set, wrong token", "s3cret", true, "wrong", http.StatusUnauthorized},
		{"token set, wrong token, beyond loopback", "s3cret", false, "wrong", http.StatusUnauthorized},
		{"token set, right token", "s3cret", true, "s3cret", http.StatusOK},
		{"token set, right token, beyond loopback", "s3cret", false, "s3cret", http.StatusOK},
		{"no token, loopback", "", true, "", http.StatusOK},
		{"no token, beyond loopback", "", false, "", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pn := newPairNode(t, true)
			s, _ := newTestServer(t, relayCfg(c.token), &fakeRunner{}, relayOpts(pn, c.loopback))
			rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, relayBodyQueued, relayHeaders(c.header, "node-v"))
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, c.want, rec.Body.String())
			}
			if c.want != http.StatusOK && pn.frameCount() != 0 {
				t.Fatal("a refused relay call still posted a card")
			}
		})
	}
	// The auth verdict comes before any validation: junk with the wrong token is a 401, not a 400.
	pn := newPairNode(t, true)
	s, _ := newTestServer(t, relayCfg("s3cret"), &fakeRunner{}, relayOpts(pn, true))
	if rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, `{not json`, relayHeaders("nope", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("junk body with a wrong token = %d, want 401", rec.Code)
	}
}

// A node with no PAIR identity of its own cannot take a relay, and a node that itself relays never
// relays for another: the route answers 503 and advertises nothing.
func TestPairRelayIsClosedWithoutALocalPairIdentity(t *testing.T) {
	off := newPairNode(t, false)
	s, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, relayOpts(off, true))
	if rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, relayBodyQueued, relayHeaders("t", "node-v")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("emitter off: status = %d, want 503", rec.Code)
	}
	if h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil)); h["pair_relay"] != nil {
		t.Errorf("health advertises pair_relay on a node that cannot take one: %v", h["pair_relay"])
	}

	// An emitter that itself relays (no identity, a relay configured) is not a member.
	chained := newPairNode(t, true)
	chained.e = pairworkloads.New(pairworkloads.Config{Enabled: true, AppDir: t.TempDir(), OpenDir: t.TempDir(),
		Relay: pairworkloads.RelayConfig{Bases: []string{"http://192.0.2.9:18811"}}})
	s2, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, relayOpts(chained, true))
	if rec := do(t, s2, http.MethodPost, pairworkloads.RelayPath, relayBodyQueued, relayHeaders("t", "node-v")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a relaying node took a relay: status = %d, want 503", rec.Code)
	}

	nilPair, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, &Options{NodeID: "testnode", Snapshot: goodSnapshot, Footprints: func() []FootprintEntry { return nil }, LoopbackListener: true})
	if rec := do(t, nilPair, http.MethodPost, pairworkloads.RelayPath, relayBodyQueued, relayHeaders("t", "node-v")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no emitter: status = %d, want 503", rec.Code)
	}
}

// Health publishes pair_relay exactly when the route would admit: a local PAIR identity and the
// lane's reachability rule (a token, or a loopback listener).
func TestPairRelayHealthAdvertisesOnlyWhenItAdmits(t *testing.T) {
	cases := []struct {
		name     string
		enabled  bool
		token    string
		loopback bool
		want     bool
	}{
		{"identity and token", true, "t", false, true},
		{"identity, loopback, no token", true, "", true, true},
		{"identity, beyond loopback, no token", true, "", false, false},
		{"no identity", false, "t", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pn := newPairNode(t, c.enabled)
			s, _ := newTestServer(t, relayCfg(c.token), &fakeRunner{}, relayOpts(pn, c.loopback))
			h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
			got, _ := h["pair_relay"].(bool)
			if got != c.want {
				t.Fatalf("pair_relay = %v (present %v), want %v", got, h["pair_relay"] != nil, c.want)
			}
			// The advertisement and the route are one predicate.
			rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, relayBodyQueued, relayHeaders(c.token, "node-v"))
			if admits := rec.Code == http.StatusOK; admits != c.want {
				t.Fatalf("health says %v but the route answered %d", c.want, rec.Code)
			}
		})
	}
}

// A relayed frame becomes ONE card on this member: the namespaced id, the member's own originatedFrom,
// the requester naming the asker, and scheduledOn resolved HERE from the hint (a view-only node).
func TestPairRelayPostsTheCardFromTheMember(t *testing.T) {
	pn := newPairNode(t, true)
	viewOnlyNode(t, pn)
	s, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, relayOpts(pn, true))
	for _, body := range []string{
		relayBodyQueued,
		strings.Replace(strings.Replace(relayBodyQueued, `"state":"queued"`, `"state":"completed"`, 1), "workload:submitted", "workload:completed", 1),
	} {
		if rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, body, relayHeaders("t", "node-v")); rec.Code != http.StatusOK {
			t.Fatalf("relay = %d (%s)", rec.Code, rec.Body.String())
		}
	}
	cards := pn.cards(t)
	if len(cards) != 2 {
		t.Fatalf("states = %v, want queued and completed", cards)
	}
	for state, wi := range cards {
		if want := pairworkloads.RelayJobID("node-v", "led-1-1"); wi["id"] != want {
			t.Errorf("%s: id = %v, want %q", state, wi["id"], want)
		}
		if wi["originatedFrom"] != "node-s-uuid" {
			t.Errorf("%s: originatedFrom = %v, want the member's own", state, wi["originatedFrom"])
		}
		if wi["scheduledOn"] != "node-v-uuid" {
			t.Errorf("%s: scheduledOn = %v, want the view-only node's uuid", state, wi["scheduledOn"])
		}
		if wi["requesterId"] != "offload-harness/fleet:node-v/sess-9" {
			t.Errorf("%s: requesterId = %v", state, wi["requesterId"])
		}
	}
	// The terminal frame removed the in-flight marker.
	if ents, _ := os.ReadDir(pn.open); len(ents) != 0 {
		t.Errorf("register after the terminal frame = %v, want empty", ents)
	}
}

func TestPairRelayRefusesWhatIsNotAFrame(t *testing.T) {
	pn := newPairNode(t, true)
	s, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, relayOpts(pn, true))
	for name, c := range map[string]struct {
		body   string
		asker  string
		status int
	}{
		"no asker header":        {relayBodyQueued, "", http.StatusBadRequest},
		"blank asker header":     {relayBodyQueued, "   ", http.StatusBadRequest},
		"not json":               {`{`, "node-v", http.StatusBadRequest},
		"unknown top-level key":  {strings.Replace(relayBodyQueued, `"node":"node-v"`, `"node":"node-v","x":1`, 1), "node-v", http.StatusBadRequest},
		"unknown workloadInfo":   {strings.Replace(relayBodyQueued, `"error":null`, `"error":null,"prompt":"hi"`, 1), "node-v", http.StatusBadRequest},
		"method outside the set": {strings.Replace(relayBodyQueued, "workload:submitted", "workloads:remove", 1), "node-v", http.StatusBadRequest},
		"body over the cap":      {`{"jsonrpc":"2.0","pad":"` + strings.Repeat("x", pairworkloads.RelayBodyMax) + `"}`, "node-v", http.StatusRequestEntityTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, c.body, relayHeaders("t", c.asker))
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, c.status, rec.Body.String())
			}
		})
	}
	if pn.frameCount() != 0 {
		t.Fatal("a refused relay call posted a card")
	}
}

// One token holder cannot flood PAIR: a per-asker token bucket, a 429 with Retry-After, and the
// refusal comes before the body is read.
func TestPairRelayIsRateLimitedPerAsker(t *testing.T) {
	pn := newPairNode(t, true)
	s, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, relayOpts(pn, true))
	s.relayLimiter = pairworkloads.NewRelayLimiter(0.001, 2, 1000, 1000)
	post := func(asker, body string) *httptest.ResponseRecorder {
		return do(t, s, http.MethodPost, pairworkloads.RelayPath, body, relayHeaders("t", asker))
	}
	for i := 0; i < 2; i++ {
		if rec := post("node-v", relayBodyQueued); rec.Code != http.StatusOK {
			t.Fatalf("call %d = %d", i, rec.Code)
		}
	}
	rec := post("node-v", relayBodyQueued)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third call = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("a 429 without Retry-After")
	}
	if rec := post("node-v", `{junk`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("an over-limit asker's junk body = %d, want 429 before the body is read", rec.Code)
	}
	if rec := post("node-w", relayBodyQueued); rec.Code != http.StatusOK {
		t.Fatalf("another asker was refused (%d): one flood must not starve the rest", rec.Code)
	}
	if got := pn.frameCount(); got != 3 {
		t.Fatalf("frames = %d, want 3 (the limited calls posted nothing)", got)
	}
}

// The call rate is not the only bound: past the cap of cards a token holder leaves open, a NEW card is a
// 429 (the register and PAIR's Jobs list stay bounded), while the terminal frame of an open card still
// goes through and frees its slot.
func TestPairRelayCapsTheCardsLeftOpen(t *testing.T) {
	pn := newPairNode(t, true)
	s, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, relayOpts(pn, true))
	s.relayLimiter = pairworkloads.NewRelayLimiter(1000, 1000, 1e6, 1_000_000)
	s.relayLimiter.SetOpenCaps(2, 100)
	frame := func(id, method, state string) string {
		return `{"jsonrpc":"2.0","method":"` + method + `","params":{"workloadInfo":{"id":"` + id + `","model":"gemma-4-e4b",` +
			`"engine":"llamacpp","runId":"` + id + `","state":"` + state + `","originatedFrom":null,"scheduledOn":null,` +
			`"createdAt":1000,"startedAt":null,"completedAt":null,"error":null,"requesterId":"offload-harness/sess-9"}},"node":"node-v"}`
	}
	post := func(body string) *httptest.ResponseRecorder {
		return do(t, s, http.MethodPost, pairworkloads.RelayPath, body, relayHeaders("t", "node-v"))
	}
	for _, id := range []string{"c1", "c2"} {
		if rec := post(frame(id, "workload:submitted", "queued")); rec.Code != http.StatusOK {
			t.Fatalf("card %s = %d", id, rec.Code)
		}
	}
	rec := post(frame("c3", "workload:submitted", "queued"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("a 3rd open card = %d (Retry-After %q), want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := post(frame("c1", "workload:started", "running")); rec.Code != http.StatusOK {
		t.Fatalf("the next frame of an open card = %d, want 200", rec.Code)
	}
	if rec := post(frame("c1", "workload:completed", "completed")); rec.Code != http.StatusOK {
		t.Fatalf("a terminal frame = %d, want 200", rec.Code)
	}
	if rec := post(frame("c3", "workload:submitted", "queued")); rec.Code != http.StatusOK {
		t.Fatalf("a card after a slot freed = %d, want 200", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, frame("c9", "workload:submitted", "queued"), relayHeaders("t", "node-w")); rec.Code != http.StatusOK {
		t.Fatalf("another asker's first card = %d: the cap is per asker", rec.Code)
	}
}

// Frames of a job are not ordered on the wire, so an in-flight frame can land after its card's terminal
// frame. The member answers it 200 (a refusal would demote a healthy relay) and posts nothing: no
// frame to PAIR, no marker nothing would close.
func TestPairRelayIgnoresAnInFlightFrameThatArrivesAfterTheTerminalFrame(t *testing.T) {
	pn := newPairNode(t, true)
	s, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, relayOpts(pn, true))
	post := func(body string) {
		t.Helper()
		if rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, body, relayHeaders("t", "node-v")); rec.Code != http.StatusOK {
			t.Fatalf("relay = %d (%s)", rec.Code, rec.Body.String())
		}
		pn.e.Wait()
	}
	completed := strings.Replace(strings.Replace(relayBodyQueued, `"state":"queued"`, `"state":"completed"`, 1), "workload:submitted", "workload:completed", 1)
	running := strings.Replace(strings.Replace(relayBodyQueued, `"state":"queued"`, `"state":"running"`, 1), "workload:submitted", "workload:started", 1)
	post(completed)
	before := len(pn.cards(t))
	post(running) // the late frame
	if got := len(pn.cards(t)); got != before {
		t.Fatalf("the late in-flight frame was posted to PAIR (%d card states, was %d)", got, before)
	}
	if ents, _ := os.ReadDir(pn.open); len(ents) != 0 {
		t.Fatalf("register = %v: the late frame left a marker nothing closes", ents)
	}
}

// A relayed in-flight card's marker is a remote producer's: pid 0, flagged remote, so the member's
// sweep never judges it by a pid of this box.
func TestPairRelayInFlightMarkerHasNoLocalPid(t *testing.T) {
	pn := newPairNode(t, true)
	s, _ := newTestServer(t, relayCfg("t"), &fakeRunner{}, relayOpts(pn, true))
	if rec := do(t, s, http.MethodPost, pairworkloads.RelayPath, relayBodyQueued, relayHeaders("t", "node-v")); rec.Code != http.StatusOK {
		t.Fatalf("relay = %d", rec.Code)
	}
	pn.e.Wait()
	ents, _ := os.ReadDir(pn.open)
	if len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "0-") {
		t.Fatalf("register = %v, want one pid-0 marker", ents)
	}
	raw, _ := os.ReadFile(filepath.Join(pn.open, ents[0].Name()))
	var mk struct {
		PID    int  `json:"pid"`
		Remote bool `json:"remote"`
	}
	_ = json.Unmarshal(raw, &mk)
	if mk.PID != 0 || !mk.Remote {
		t.Fatalf("marker = %s", raw)
	}
}

// The whole path over HTTP: a relaying emitter with no PAIR identity posts a job's three frames to a
// real member server, and the member's ingress sees ONE card, in order, on the view-only node, with
// the member's own origin; the register is empty once the job is done.
func TestPairRelayEndToEndFromARelayingEmitter(t *testing.T) {
	pn := newPairNode(t, true)
	viewOnlyNode(t, pn)
	s, _ := newTestServer(t, relayCfg("fleet-token"), &fakeRunner{}, relayOpts(pn, false))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	relaying := pairworkloads.New(pairworkloads.Config{Enabled: true, AppDir: t.TempDir(), OpenDir: t.TempDir(),
		Relay: pairworkloads.RelayConfig{Bases: []string{srv.URL}, Token: "fleet-token"}})
	// This box's own events carry no node: the hint is its own short name, which the test makes a
	// view-only node by naming the view-only entry after the machine running it.
	self := core.SanitizeAsker(strings.ToLower(hostnameShort(t)))
	if err := os.WriteFile(filepath.Join(pn.app, "configs", "view-only-nodes.json"),
		[]byte(`[{"name":"`+self+`","address":"192.0.2.50","port":18811,"nodeUuid":"node-v-uuid"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []pairworkloads.Event{
		{JobID: "lease-1-9", Model: "seatbench.ps1", Engine: "gpu-lease", State: "queued", Requester: "offload-harness", CreatedAt: 1000},
		{JobID: "lease-1-9", Model: "seatbench.ps1", Engine: "gpu-lease", State: "running", Requester: "offload-harness", CreatedAt: 1000, StartedAt: 1100},
		{JobID: "lease-1-9", Model: "seatbench.ps1", Engine: "gpu-lease", State: "completed", Requester: "offload-harness", CreatedAt: 1000, StartedAt: 1100, CompletedAt: 1500},
	} {
		if err := relaying.Send(context.Background(), ev); err != nil {
			t.Fatalf("Send %s: %v", ev.State, err)
		}
	}
	cards := pn.cards(t) // fails unless the frames are one card
	if len(cards) != 3 {
		t.Fatalf("states = %v", cards)
	}
	want := pairworkloads.RelayJobID(self, "lease-1-9")
	for state, wi := range cards {
		if wi["id"] != want || wi["originatedFrom"] != "node-s-uuid" || wi["scheduledOn"] != "node-v-uuid" || wi["engine"] != "gpu-lease" {
			t.Errorf("%s frame: %v", state, wi)
		}
	}
	if ents, _ := os.ReadDir(pn.open); len(ents) != 0 {
		t.Errorf("member register after the terminal frame = %v", ents)
	}
}

func hostnameShort(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil || h == "" {
		t.Skip("no hostname on this machine")
	}
	h, _, _ = strings.Cut(h, ".")
	return h
}
