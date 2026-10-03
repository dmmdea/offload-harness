package pairworkloads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The card relay's tests (D26). Fixtures name boxes by node letter and a per-test label; the
// view-only box is node-v, at an RFC 5737 address.

const viewOnlyJSON = `[{"name":"node-v","address":"192.0.2.50","port":18811,"nodeUuid":"node-v-uuid"}]`

// writeViewOnly adds PAIR's view-only list (configs/view-only-nodes.json) to an app dir.
func writeViewOnly(t *testing.T, appDir string) {
	t.Helper()
	p := filepath.Join(appDir, viewOnlyNodesFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(viewOnlyJSON), 0o644); err != nil {
		t.Fatal(err)
	}
}

// relayInfo is a relaying box's workloadInfo, as buildRelay writes it.
func relayFrame(t *testing.T, method string, info map[string]any, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{"jsonrpc": "2.0", "method": method, "params": map[string]any{"workloadInfo": info}}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func queuedInfo() map[string]any {
	return map[string]any{
		"id": "led-1-1", "model": "gemma-4-e4b", "engine": "llamacpp", "runId": "led-1-1", "state": "queued",
		"originatedFrom": nil, "scheduledOn": nil, "createdAt": 1000, "startedAt": nil, "completedAt": nil,
		"error": nil, "requesterId": "offload-harness/sess-9",
	}
}

// member builds the serving side: an enabled emitter with a PAIR identity, its members file, the
// view-only list, a stand-in ingress, and its own register directory.
type relayMember struct {
	pair  *capture
	e     *Emitter
	open  string
	appDr string
}

func newRelayMember(t *testing.T) *relayMember {
	t.Helper()
	m := &relayMember{pair: &capture{}, appDr: writePairAppDir(t), open: t.TempDir()}
	writeViewOnly(t, m.appDr)
	srv := httptest.NewServer(http.HandlerFunc(m.pair.handler))
	t.Cleanup(srv.Close)
	m.e = New(Config{Enabled: true, Endpoint: srv.URL, AppDir: m.appDr, OpenDir: m.open})
	return m
}

// relayOne parses a relayed body from asker and emits it through the member's emitter, as the
// fleet-node handler does, and returns the posted workloadInfo.
func (m *relayMember) relayOne(t *testing.T, body []byte, asker string) map[string]any {
	t.Helper()
	ev, err := ParseRelay(body, asker)
	if err != nil {
		t.Fatalf("ParseRelay: %v", err)
	}
	before := m.pair.count()
	m.e.Emit(ev)
	m.e.Wait()
	if m.pair.count() != before+1 {
		t.Fatalf("frames = %d, want %d", m.pair.count(), before+1)
	}
	return m.pair.info(before)
}

// The relayed id is namespaced per asker and bounded; two different (asker, id) pairs never share a
// card, even when the plain concatenation is the same string.
func TestRelayJobIDIsNamespacedBoundedAndInjective(t *testing.T) {
	got := RelayJobID("node-q", "led-1-1")
	if !strings.HasPrefix(got, "relay-node-q-led-1-1-") || len(got) != len("relay-node-q-led-1-1-")+8 {
		t.Fatalf("RelayJobID = %q", got)
	}
	if RelayJobID("node-q", "led-1-1") != got {
		t.Fatal("the id is not deterministic: a terminal frame would open a second card")
	}
	if a, b := RelayJobID("n", "led-1-1"), RelayJobID("n-led", "1-1"); a == b {
		t.Fatalf("two different (asker, id) pairs share the card %q", a)
	}
	if a, b := RelayJobID("node-q", "led-1-1"), RelayJobID("node-r", "led-1-1"); a == b {
		t.Fatalf("two askers' same job id share the card %q", a)
	}
	long := RelayJobID(strings.Repeat("a", 64), strings.Repeat("x", 128))
	if len(long) > relayIDMax {
		t.Fatalf("id length %d over the bound %d", len(long), relayIDMax)
	}
	if long == RelayJobID(strings.Repeat("a", 64), strings.Repeat("x", 127)+"y") {
		t.Fatal("ids that differ past the bound share a card")
	}
}

// The requester names the relaying box and keeps the original suffix, bounded.
func TestRelayRequesterNamesTheAskerAndKeepsTheSession(t *testing.T) {
	cases := []struct{ asker, in, want string }{
		{"node-q", "", "offload-harness/fleet:node-q"},
		{"node-q", "offload-harness", "offload-harness/fleet:node-q"},
		{"node-q", "offload-harness/sess-9", "offload-harness/fleet:node-q/sess-9"},
		{"node-q", "somebody-else", "offload-harness/fleet:node-q/somebody-else"},
		{"node-q", "offload-harness/" + strings.Repeat("s", 200), "offload-harness/fleet:node-q/" + strings.Repeat("s", core.AskerMaxLen)},
	}
	for _, c := range cases {
		if got := RelayRequester(c.asker, c.in); got != c.want {
			t.Errorf("RelayRequester(%q,%q) = %q, want %q", c.asker, c.in, got, c.want)
		}
	}
}

// ParseRelay refuses everything but the frame the relaying box builds, and an asker-less call.
func TestParseRelayIsStrict(t *testing.T) {
	good := relayFrame(t, "workload:submitted", queuedInfo(), nil)
	if _, err := ParseRelay(good, "node-q"); err != nil {
		t.Fatalf("a well-formed frame was refused: %v", err)
	}
	with := func(mut func(info map[string]any)) []byte {
		info := queuedInfo()
		mut(info)
		return relayFrame(t, "workload:submitted", info, nil)
	}
	cases := []struct {
		name  string
		body  []byte
		asker string
	}{
		{"no asker", good, ""},
		{"blank asker", good, " \t"},
		{"not json", []byte(`{`), "node-q"},
		{"unknown top-level key", relayFrame(t, "workload:submitted", queuedInfo(), map[string]any{"extra": 1}), "node-q"},
		{"unknown info key", with(func(i map[string]any) { i["prompt"] = "secret" }), "node-q"},
		{"wrong jsonrpc", relayFrame(t, "workload:submitted", queuedInfo(), map[string]any{"jsonrpc": "1.0"}), "node-q"},
		{"method outside the lifecycle", relayFrame(t, "workloads:remove", queuedInfo(), nil), "node-q"},
		{"method disagrees with state", relayFrame(t, "workload:completed", queuedInfo(), nil), "node-q"},
		{"unknown state", with(func(i map[string]any) { i["state"] = "paused" }), "node-q"},
		{"no id", with(func(i map[string]any) { delete(i, "id") }), "node-q"},
		{"runId differs from id", with(func(i map[string]any) { i["runId"] = "other" }), "node-q"},
		{"no model", with(func(i map[string]any) { i["model"] = "" }), "node-q"},
		{"engine is not a name", with(func(i map[string]any) { i["engine"] = "Llama CPP!" }), "node-q"},
		{"control character in the model", with(func(i map[string]any) { i["model"] = "a\x07b" }), "node-q"},
		{"negative timestamp", with(func(i map[string]any) { i["createdAt"] = -5 }), "node-q"},
		{"timestamp as a string", with(func(i map[string]any) { i["startedAt"] = "yesterday" }), "node-q"},
		{"originatedFrom as an object", with(func(i map[string]any) { i["originatedFrom"] = map[string]any{"a": 1} }), "node-q"},
		{"too many aliases", relayFrame(t, "workload:submitted", queuedInfo(), map[string]any{"node_aliases": []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}}), "node-q"},
		{"node as a number", relayFrame(t, "workload:submitted", queuedInfo(), map[string]any{"node": 7}), "node-q"},
		{"body over the cap", append([]byte(nil), []byte(strings.Repeat(" ", RelayBodyMax+1))...), "node-q"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseRelay(c.body, c.asker); err == nil {
				t.Fatal("refusal expected")
			} else if _, ok := err.(*RelayError); !ok {
				t.Fatalf("error %T, want *RelayError", err)
			}
		})
	}
}

// The posted card is the member's: namespaced id, the member's own originatedFrom whatever the body
// said, the requester rewritten, and scheduledOn resolved by the MEMBER from the hint, view-only
// nodes included, and never the member itself for a job that did not run there.
func TestRelayCardIsResolvedOnTheMember(t *testing.T) {
	cases := []struct {
		name    string
		extra   map[string]any
		asker   string
		wantOn  any // scheduledOn
		wantHit string
	}{
		{"a member by hint", map[string]any{"node": "node-b"}, "node-q", "node-b-uuid", ""},
		{"a view-only node by hint", map[string]any{"node": "node-v"}, "node-q", "node-v-uuid", ""},
		{"a view-only node by alias", map[string]any{"node": "node-x", "node_aliases": []string{"node-v"}}, "node-q", "node-v-uuid", ""},
		{"a view-only node by address alias", map[string]any{"node": "node-x", "node_aliases": []string{"192.0.2.50"}}, "node-q", "node-v-uuid", ""},
		{"no hint, the asker is a view-only name", nil, "node-v", "node-v-uuid", ""},
		{"no hint, the asker resolves nowhere", nil, "node-q", nil, ""},
		{"a hint that resolves nowhere", map[string]any{"node": "node-x"}, "node-v", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newRelayMember(t)
			info := queuedInfo()
			info["originatedFrom"] = "evil-origin-uuid"
			info["scheduledOn"] = "evil-schedule-uuid"
			got := m.relayOne(t, relayFrame(t, "workload:submitted", info, c.extra), c.asker)
			if got["scheduledOn"] != c.wantOn {
				t.Errorf("scheduledOn = %v, want %v", got["scheduledOn"], c.wantOn)
			}
			if got["scheduledOn"] == "self-uuid" {
				t.Error("a relayed job that did not run on the member was stamped on the member")
			}
			if got["originatedFrom"] != "self-uuid" {
				t.Errorf("originatedFrom = %v: the member's emitter stamps its own identity, never the body's", got["originatedFrom"])
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw), "evil-") {
				t.Errorf("a value the relay supplied for a field the member owns reached PAIR: %s", raw)
			}
			if want := RelayJobID(c.asker, "led-1-1"); got["id"] != want || got["runId"] != want {
				t.Errorf("id = %v runId = %v, want %q", got["id"], got["runId"], want)
			}
			if want := "offload-harness/fleet:" + c.asker + "/sess-9"; got["requesterId"] != want {
				t.Errorf("requesterId = %v, want %q", got["requesterId"], want)
			}
		})
	}
}

// A relayed in-flight card is a REMOTE producer's: its marker carries no pid, and the member's sweep
// leaves it alone while its producer (on another box) lives, however dead every pid of this box looks.
func TestRelayedMarkerIsNotJudgedByTheLocalPidTable(t *testing.T) {
	m := newRelayMember(t)
	m.relayOne(t, relayFrame(t, "workload:submitted", queuedInfo(), map[string]any{"node": "node-v"}), "node-q")
	ents, _ := os.ReadDir(m.open)
	if len(ents) != 1 {
		t.Fatalf("register = %v, want one marker", ents)
	}
	if !strings.HasPrefix(ents[0].Name(), "0-relay-node-q-led-1-1-") {
		t.Errorf("marker name %q: a relayed marker is pid 0 so the terminal frame finds it by name", ents[0].Name())
	}
	raw, _ := os.ReadFile(filepath.Join(m.open, ents[0].Name()))
	var mk openMarker
	if err := json.Unmarshal(raw, &mk); err != nil {
		t.Fatal(err)
	}
	if !mk.Remote || mk.PID != 0 || mk.ProcStart != 0 {
		t.Fatalf("marker = %+v, want remote with no pid", mk)
	}

	// A sweeper on the member, every pid dead: the relayed card stays.
	sw := New(Config{Enabled: true, Endpoint: m.e.cfg.Endpoint, AppDir: m.appDr, OpenDir: m.open})
	sw.alive = dead
	sw.procStart = func(int) (int64, bool) { return 0, false }
	if n := sw.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("the sweep closed %d relayed card(s) whose producer is on another box", n)
	}
	if got := m.pair.count(); got != 1 {
		t.Fatalf("frames = %d, want only the queued frame", got)
	}

	// Past the age cap it closes failed, once.
	sw.now = func() time.Time { return time.Now().Add(RelayOpenMaxAge + time.Hour) }
	if n := sw.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("sweep past the age cap closed %d, want 1", n)
	}
	m.e.Wait()
	if got := m.pair.method(1); got != "workload:errored" {
		t.Fatalf("the age-cap close is %v, want workload:errored", got)
	}
}

// The terminal relayed frame closes the card and removes its marker by name, even when the member
// restarted since the in-flight frame (a new emitter has none of the old one's memory).
func TestRelayedTerminalRemovesTheMarkerAfterAMemberRestart(t *testing.T) {
	m := newRelayMember(t)
	m.relayOne(t, relayFrame(t, "workload:submitted", queuedInfo(), map[string]any{"node": "node-v"}), "node-q")

	restarted := New(Config{Enabled: true, Endpoint: m.e.cfg.Endpoint, AppDir: m.appDr, OpenDir: m.open})
	done := queuedInfo()
	done["state"], done["completedAt"], done["startedAt"] = "completed", 2000, 1100
	ev, err := ParseRelay(relayFrame(t, "workload:completed", done, map[string]any{"node": "node-v"}), "node-q")
	if err != nil {
		t.Fatal(err)
	}
	restarted.Emit(ev)
	restarted.Wait()
	if ents, _ := os.ReadDir(m.open); len(ents) != 0 {
		t.Fatalf("register after the terminal frame = %v, want empty: the card would be failed by the age cap after it completed", ents)
	}
	if got := m.pair.method(m.pair.count() - 1); got != "workload:completed" {
		t.Fatalf("last frame = %v", got)
	}
}

// A terminal frame PAIR could not take is kept as a pending frame of the same remote kind, and the
// sweep delivers it whatever any pid says.
func TestRelayedPendingTerminalSurvivesAsRemote(t *testing.T) {
	m := newRelayMember(t)
	m.relayOne(t, relayFrame(t, "workload:submitted", queuedInfo(), map[string]any{"node": "node-v"}), "node-q")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer down.Close()
	broken := New(Config{Enabled: true, Endpoint: down.URL, AppDir: m.appDr, OpenDir: m.open})
	done := queuedInfo()
	done["state"], done["completedAt"] = "completed", 2000
	ev, _ := ParseRelay(relayFrame(t, "workload:completed", done, nil), "node-q")
	broken.Emit(ev)
	broken.Wait()
	ents, _ := os.ReadDir(m.open)
	if len(ents) != 1 {
		t.Fatalf("register = %v, want one pending marker", ents)
	}
	raw, _ := os.ReadFile(filepath.Join(m.open, ents[0].Name()))
	var mk openMarker
	_ = json.Unmarshal(raw, &mk)
	if !mk.Pending || !mk.Remote || mk.PID != 0 {
		t.Fatalf("pending marker = %+v, want pending, remote, no pid", mk)
	}
}

func TestRelayLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewRelayLimiter(2, 3, 100, 100)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("node-q"); !ok {
			t.Fatalf("burst call %d refused", i)
		}
	}
	if ok, wait := l.Allow("node-q"); ok || wait <= 0 {
		t.Fatalf("a call past the burst: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("node-r"); !ok {
		t.Fatal("one asker's flood starved another")
	}
	now = now.Add(time.Second) // 2 tokens refill
	if ok, _ := l.Allow("node-q"); !ok {
		t.Fatal("no refill after a second")
	}
	if ok, _ := l.Allow("node-q"); !ok {
		t.Fatal("second refilled token refused")
	}
	if ok, _ := l.Allow("node-q"); ok {
		t.Fatal("more than the refill allowed")
	}
}

// The asker name is a header the caller chooses: a fresh name per call must not be a fresh bucket.
func TestRelayLimiterGlobalBucketStopsAskerRotation(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewRelayLimiter(10, 10, 1, 5)
	l.now = func() time.Time { return now }
	allowed := 0
	for i := 0; i < 50; i++ {
		if ok, _ := l.Allow(strings.Repeat("a", i+1)); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("%d calls got through with a rotating asker name, want the global burst of 5", allowed)
	}
}

func TestRelayLimiterBoundsItsBuckets(t *testing.T) {
	l := NewRelayLimiter(1, 1, 1e9, 1_000_000)
	for i := 0; i < RelayBuckets*3; i++ {
		l.Allow("asker-" + string(rune('a'+i%26)) + strings.Repeat("x", i))
	}
	l.mu.Lock()
	n := len(l.b)
	l.mu.Unlock()
	if n > RelayBuckets {
		t.Fatalf("%d buckets tracked, bound %d", n, RelayBuckets)
	}
}

// --- the relaying side ----------------------------------------------------------------------------

// relayHit is one request a stand-in relay member received.
type relayHit struct {
	header http.Header
	body   map[string]any
}

type fakeRelay struct {
	mu     sync.Mutex
	hits   []relayHit
	status int
	srv    *httptest.Server
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.hits = append(f.hits, relayHit{header: r.Header.Clone(), body: b})
		st := f.status
		f.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelay) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *fakeRelay) hit(i int) relayHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[i]
}

func (f *fakeRelay) info(i int) map[string]any {
	return f.hit(i).body["params"].(map[string]any)["workloadInfo"].(map[string]any)
}

// noPairAppDir is an app dir with no PAIR in it: no node-id.json, so the emitter has no identity.
func noPairAppDir(t *testing.T) string { return t.TempDir() }

func relayEmitter(t *testing.T, relays ...*fakeRelay) (*Emitter, string) {
	t.Helper()
	open := t.TempDir()
	var bases []string
	for _, r := range relays {
		bases = append(bases, r.srv.URL)
	}
	e := New(Config{Enabled: true, Endpoint: "http://127.0.0.1:1/unused", AppDir: noPairAppDir(t), OpenDir: open,
		Relay: RelayConfig{Bases: bases, Token: "fleet-token"}})
	return e, open
}

// With no identity of its own the emitter's frames go to the relay, with the bearer and its own name,
// as the relay body (no originatedFrom, no scheduledOn, the node hint beside the frame).
func TestRelayModeSendsFramesToTheRelayWithBearerAndAsker(t *testing.T) {
	r := newFakeRelay(t)
	e, _ := relayEmitter(t, r)
	if !e.Enabled() {
		t.Fatal("an emitter with a relay and no identity is not enabled")
	}
	e.Emit(Event{JobID: "led-5-1", Model: "gemma-4-e4b", Engine: "llamacpp", State: "queued", Requester: "offload-harness/sess-9", CreatedAt: 5000,
		Node: "node-c", NodeAliases: []string{"node-c-fleet"}})
	e.Wait()
	if r.count() != 1 {
		t.Fatalf("relay hits = %d, want 1", r.count())
	}
	h := r.hit(0)
	if got := h.header.Get("Authorization"); got != "Bearer fleet-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.header.Get(core.AskerHeader); got != relaySelfName() || got == "" {
		t.Errorf("%s = %q, want this box's short name %q", core.AskerHeader, got, relaySelfName())
	}
	if got := h.header.Get(core.PairCardHeader); got != "" {
		t.Errorf("a relayed frame carries %s=%q", core.PairCardHeader, got)
	}
	info := r.info(0)
	if info["originatedFrom"] != nil || info["scheduledOn"] != nil {
		t.Errorf("originatedFrom/scheduledOn = %v/%v: the relaying box has no identity to claim", info["originatedFrom"], info["scheduledOn"])
	}
	if info["id"] != "led-5-1" || info["state"] != "queued" || info["requesterId"] != "offload-harness/sess-9" {
		t.Errorf("info = %v", info)
	}
	if h.body["node"] != "node-c" {
		t.Errorf("node hint = %v", h.body["node"])
	}
	if al, _ := h.body["node_aliases"].([]any); len(al) != 1 || al[0] != "node-c-fleet" {
		t.Errorf("node_aliases = %v", h.body["node_aliases"])
	}
	if h.body["method"] != "workload:submitted" {
		t.Errorf("method = %v", h.body["method"])
	}
	// The body the relaying box builds is exactly what the member's strict decode takes.
	raw, _ := json.Marshal(h.body)
	if _, err := ParseRelay(raw, "node-q"); err != nil {
		t.Errorf("the relaying box's frame does not pass the member's decode: %v", err)
	}
}

// An event of this box's own (empty Node) carries this box's own name as the hint, so the member
// can resolve it (a view-only name).
func TestRelayModeNamesThisBoxWhenTheEventHasNoNode(t *testing.T) {
	r := newFakeRelay(t)
	e, _ := relayEmitter(t, r)
	e.Emit(Event{JobID: "led-5-2", Model: "m", Engine: "llamacpp", State: "completed", CreatedAt: 5000, CompletedAt: 6000})
	e.Wait()
	if got := r.hit(0).body["node"]; got != relaySelfName() || got == "" {
		t.Fatalf("node hint = %v, want %q", got, relaySelfName())
	}
}

// The relay is used ONLY when there is no local identity: a readable node-id.json keeps the loopback
// ingress and the relay never hears of the frame.
func TestRelayIsUsedOnlyWithoutALocalIdentity(t *testing.T) {
	r := newFakeRelay(t)
	pair := &capture{}
	ing := httptest.NewServer(http.HandlerFunc(pair.handler))
	defer ing.Close()
	e := New(Config{Enabled: true, Endpoint: ing.URL, AppDir: writePairAppDir(t), OpenDir: t.TempDir(),
		Relay: RelayConfig{Bases: []string{r.srv.URL}, Token: "t"}})
	e.Emit(Event{JobID: "led-6-1", Model: "m", Engine: "llamacpp", State: "queued", CreatedAt: 1})
	e.Wait()
	if pair.count() != 1 || r.count() != 0 {
		t.Fatalf("ingress frames = %d, relay frames = %d: with an identity the frame goes to PAIR's ingress only", pair.count(), r.count())
	}
}

// A relay's marker endpoint is the relay's route URL, so the relaying box's sweeper closes only the
// cards it opened through a relay, and closes them through it with the node hint kept.
func TestRelayMarkerEndpointIsTheRelayURL(t *testing.T) {
	r := newFakeRelay(t)
	e, open := relayEmitter(t, r)
	e.Emit(Event{JobID: "led-7-1", Model: "m", Engine: "llamacpp", State: "running", CreatedAt: 1, StartedAt: 2, Node: "node-c"})
	e.Wait()
	ents, _ := os.ReadDir(open)
	if len(ents) != 1 {
		t.Fatalf("register = %v", ents)
	}
	raw, _ := os.ReadFile(filepath.Join(open, ents[0].Name()))
	var mk openMarker
	_ = json.Unmarshal(raw, &mk)
	if mk.Endpoint != r.srv.URL+RelayPath {
		t.Fatalf("marker endpoint = %q, want the relay route %q", mk.Endpoint, r.srv.URL+RelayPath)
	}
	if mk.Relay == nil || mk.Relay.Node != "node-c" || mk.Remote {
		t.Fatalf("marker = %+v, want the relay hint kept and not a remote-producer marker", mk)
	}

	// A different relay's sweeper leaves it alone.
	other := newFakeRelay(t)
	sw := New(Config{Enabled: true, Endpoint: "http://127.0.0.1:1/unused", AppDir: noPairAppDir(t), OpenDir: open,
		Relay: RelayConfig{Bases: []string{other.srv.URL}, Token: "t"}})
	sw.alive = dead
	if n := sw.SweepOrphans(context.Background()); n != 0 || other.count() != 0 || r.count() != 1 {
		t.Fatalf("a sweeper of another relay touched the marker: closed %d, other hits %d, relay hits %d", n, other.count(), r.count())
	}

	// The same relay's sweeper (the producer's pid is dead) closes it THROUGH the relay.
	sw2 := New(Config{Enabled: true, Endpoint: "http://127.0.0.1:1/unused", AppDir: noPairAppDir(t), OpenDir: open,
		Relay: RelayConfig{Bases: []string{r.srv.URL}, Token: "t"}})
	sw2.alive = dead
	sw2.procStart = func(int) (int64, bool) { return 0, false }
	if n := sw2.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("sweep closed %d, want 1", n)
	}
	if r.count() != 2 {
		t.Fatalf("relay hits = %d, want the close", r.count())
	}
	closeHit := r.hit(1)
	if closeHit.body["method"] != "workload:errored" || closeHit.body["node"] != "node-c" {
		t.Errorf("close = %v: it must be an errored relay frame carrying the node hint", closeHit.body)
	}
	if got := r.info(1); got["id"] != "led-7-1" || got["state"] != "failed" {
		t.Errorf("close info = %v", got)
	}
	if ents, _ := os.ReadDir(open); len(ents) != 0 {
		t.Errorf("register after the close = %v", ents)
	}
}

// A job's terminal frame goes to the relay its in-flight frames opened the card on, even when the
// first healthy relay changed in between.
func TestRelayPinsATerminalFrameToTheRelayItOpened(t *testing.T) {
	a, b := newFakeRelay(t), newFakeRelay(t)
	e, _ := relayEmitter(t, a, b)
	e.Emit(Event{JobID: "led-8-1", Model: "m", Engine: "llamacpp", State: "queued", CreatedAt: 1})
	e.Wait()
	if a.count() != 1 || b.count() != 0 {
		t.Fatalf("queued went to a=%d b=%d, want the first relay", a.count(), b.count())
	}
	e.demoteRelay(relayURL(a.srv.URL)) // the first relay just failed a post: the next job goes to b
	e.Emit(Event{JobID: "led-8-2", Model: "m", Engine: "llamacpp", State: "queued", CreatedAt: 1})
	e.Wait()
	if b.count() != 1 {
		t.Fatalf("a fresh job did not move to the healthy relay (b hits %d)", b.count())
	}
	e.Emit(Event{JobID: "led-8-1", Model: "m", Engine: "llamacpp", State: "completed", CreatedAt: 1, CompletedAt: 9})
	e.Wait()
	if a.count() != 2 || b.count() != 1 {
		t.Fatalf("terminal went to a=%d b=%d: a card is closed where it was opened", a.count(), b.count())
	}
}

// An unreachable relay leaves the terminal frame pending (kept for the sweep), like an unreachable
// PAIR, and never fails the caller.
func TestRelayModeKeepsATerminalFrameTheRelayCouldNotTake(t *testing.T) {
	r := newFakeRelay(t)
	e, open := relayEmitter(t, r)
	e.Emit(Event{JobID: "led-9-1", Model: "m", Engine: "llamacpp", State: "running", CreatedAt: 1, StartedAt: 2})
	e.Wait()
	r.mu.Lock()
	r.status = http.StatusServiceUnavailable
	r.mu.Unlock()
	e.Emit(Event{JobID: "led-9-1", Model: "m", Engine: "llamacpp", State: "completed", CreatedAt: 1, StartedAt: 2, CompletedAt: 3})
	e.Wait()
	ents, _ := os.ReadDir(open)
	if len(ents) != 1 {
		t.Fatalf("register = %v, want the pending terminal marker", ents)
	}
	raw, _ := os.ReadFile(filepath.Join(open, ents[0].Name()))
	var mk openMarker
	_ = json.Unmarshal(raw, &mk)
	if !mk.Pending || mk.Relay == nil || mk.Endpoint != r.srv.URL+RelayPath {
		t.Fatalf("marker = %+v, want a pending terminal frame kept for the same relay", mk)
	}
}

// The wire headers follow relay mode: the asker cards through its relay, so it must not ask the
// serving node to card the job too.
func TestRelayModeSuppressesThePairCardHeader(t *testing.T) {
	r := newFakeRelay(t)
	t.Setenv("OFFLOAD_PAIR_APPDIR", noPairAppDir(t))
	cfg := config.Default()
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsRelay = []string{r.srv.URL}
	cfg.FleetAuthToken = "fleet-token"
	h := http.Header{}
	WireHeadersFor(cfg, h)
	if h.Get(core.AskerHeader) == "" {
		t.Error("an asker in relay mode still names itself")
	}
	if got := h.Get(core.PairCardHeader); got != "" {
		t.Errorf("%s = %q: the asker cards through its relay, the node must not card the job too", core.PairCardHeader, got)
	}

	// Control: with no relay at all the emitter is not enabled and the node is asked to card the job.
	cfg2 := config.Default()
	cfg2.PairWorkloadsEnabled = true
	cfg2.PairWorkloadsRelay = []string{"off"}
	h2 := http.Header{}
	WireHeadersFor(cfg2, h2)
	if got := h2.Get(core.PairCardHeader); got != core.PairCardNode {
		t.Errorf("with the relay off the header = %q, want %q", got, core.PairCardNode)
	}
}

// auto = every delegate_remotes base whose health advertises pair_relay.
func TestRelayAutoUsesTheHealthCapability(t *testing.T) {
	e := New(Config{Enabled: true, AppDir: noPairAppDir(t), OpenDir: t.TempDir(),
		Relay: RelayConfig{Auto: true, Remotes: []string{"http://192.0.2.1:18811", "http://192.0.2.2:18811", "http://192.0.2.3:18811"}, Token: "t"}})
	var mu sync.Mutex
	probed := map[string]int{}
	e.relay.probe = func(ctx context.Context, base, token string) (bool, error) {
		mu.Lock()
		probed[base]++
		mu.Unlock()
		switch base {
		case "http://192.0.2.1:18811":
			return false, nil
		case "http://192.0.2.2:18811":
			return true, nil
		}
		return false, context.DeadlineExceeded
	}
	got := e.relayCandidates()
	if len(got) != 1 || got[0] != "http://192.0.2.2:18811"+RelayPath {
		t.Fatalf("candidates = %v, want only the member that advertises pair_relay", got)
	}
	if !e.Enabled() {
		t.Fatal("one advertising member: the emitter is enabled")
	}
	e.relayCandidates()
	for b, n := range probed {
		if n != 1 {
			t.Errorf("%s probed %d times inside the verdict TTL, want 1", b, n)
		}
	}
	if m := e.Mode(); m.Mode != ModeRelay || m.Relay != "http://192.0.2.2:18811"+RelayPath {
		t.Errorf("Mode = %+v", m)
	}

	none := New(Config{Enabled: true, AppDir: noPairAppDir(t), OpenDir: t.TempDir(),
		Relay: RelayConfig{Auto: true, Remotes: []string{"http://192.0.2.1:18811"}}})
	none.relay.probe = func(context.Context, string, string) (bool, error) { return false, nil }
	if none.Enabled() {
		t.Error("no member advertises pair_relay and there is no identity: the emitter must stay off")
	}
	if m := none.Mode(); m.Mode != ModeOff || m.Reason == "" {
		t.Errorf("Mode = %+v, want off with a reason", m)
	}
}

// The health probe reads the member's pair_relay flag over HTTP with the bearer.
func TestProbeRelayHealthReadsThePairRelayFlag(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/fleet/health" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"node_id":"x","pair_relay":true}`))
	}))
	defer srv.Close()
	e := New(Config{Enabled: true, AppDir: noPairAppDir(t)})
	ok, err := e.probeRelayHealth(context.Background(), srv.URL, "tok")
	if err != nil || !ok || gotAuth != "Bearer tok" {
		t.Fatalf("probe = %v, %v (auth %q)", ok, err, gotAuth)
	}
	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"node_id":"x"}`)) }))
	defer off.Close()
	if ok, _ := e.probeRelayHealth(context.Background(), off.URL, ""); ok {
		t.Fatal("a member that does not advertise pair_relay was read as one that does")
	}
}

func TestRelayFromConfig(t *testing.T) {
	mk := func(relay []string, remotes ...string) config.Config {
		c := config.Default()
		c.PairWorkloadsRelay = relay
		c.DelegateRemotes = remotes
		c.FleetAuthToken = "tok"
		return c
	}
	if r := RelayFromConfig(mk(nil, "http://192.0.2.1:1")); !r.Auto || len(r.Remotes) != 1 || len(r.Bases) != 0 || r.Token != "tok" {
		t.Errorf("absent = %+v, want auto over the remotes", r)
	}
	if r := RelayFromConfig(mk([]string{"auto"}, "http://192.0.2.1:1")); !r.Auto {
		t.Errorf("auto = %+v", r)
	}
	if r := RelayFromConfig(mk([]string{"off"}, "http://192.0.2.1:1")); r.configured() {
		t.Errorf("off = %+v, want no relay", r)
	}
	if r := RelayFromConfig(mk([]string{"http://192.0.2.9:18811"})); r.Auto || len(r.Bases) != 1 || !r.configured() {
		t.Errorf("explicit = %+v, want the list and no auto", r)
	}
	if r := RelayFromConfig(mk(nil)); r.configured() {
		t.Errorf("auto with no remotes = %+v, want nothing to relay to", r)
	}
	if r := RelayFromConfig(mk([]string{"http://192.0.2.9:18811", "off"})); r.configured() {
		t.Errorf("off must win over a listed base: %+v", r)
	}
}

func TestEmitterModeNames(t *testing.T) {
	file := New(Config{Enabled: true, AppDir: writePairAppDir(t), OpenDir: t.TempDir()})
	if m := file.Mode(); m.Mode != ModeLocal {
		t.Errorf("node-id.json: %+v", m)
	}
	if m := New(Config{Enabled: false}).Mode(); m.Mode != ModeOff {
		t.Errorf("disabled: %+v", m)
	}
	if m := New(Config{Enabled: true, AppDir: noPairAppDir(t)}).Mode(); m.Mode != ModeOff {
		t.Errorf("no PAIR, no relay: %+v", m)
	}
	if file.LocalIdentity() != true || New(Config{Enabled: true, AppDir: noPairAppDir(t)}).LocalIdentity() {
		t.Error("LocalIdentity must be true exactly when the emitter has an identity of its own")
	}
	r := newFakeRelay(t)
	relay := New(Config{Enabled: true, AppDir: noPairAppDir(t), Relay: RelayConfig{Bases: []string{r.srv.URL}}})
	if relay.LocalIdentity() {
		t.Error("a relaying emitter must not serve as a relay member")
	}
	if m := relay.Mode(); m.Mode != ModeRelay || m.Relay != r.srv.URL+RelayPath {
		t.Errorf("relay: %+v", m)
	}
}

// The node-info fallback is a local identity too: a box that has it reports to its own ingress and
// never relays, and says so as its mode.
func TestNodeInfoFallbackBeatsTheRelay(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	r := newFakeRelay(t)
	e := New(Config{Enabled: true, Endpoint: ing.URL, AppDir: t.TempDir(), OpenDir: t.TempDir(), NodeInfoURL: ni.url(),
		Relay: RelayConfig{Bases: []string{r.srv.URL}}})
	if m := e.Mode(); m.Mode != ModeNodeInfo {
		t.Fatalf("Mode = %+v, want %q", m, ModeNodeInfo)
	}
	e.Emit(Event{JobID: "fb-r-1", Model: "m", Engine: "llamacpp", State: "queued", CreatedAt: 1})
	e.Wait()
	if len(ing.frameInfos()) != 1 || r.count() != 0 {
		t.Fatalf("ingress frames %d, relay frames %d: the fallback identity keeps the local ingress", len(ing.frameInfos()), r.count())
	}
}

// After a member has a verdict, no call waits on it again: a stale verdict is answered at once and
// refreshed in the background, so a member that goes offline never stalls the harness's own calls.
func TestRelayStaleVerdictIsServedWhileItRefreshesInTheBackground(t *testing.T) {
	const base = "http://192.0.2.7:18811"
	e := New(Config{Enabled: true, AppDir: noPairAppDir(t), OpenDir: t.TempDir(),
		Relay: RelayConfig{Auto: true, Remotes: []string{base}, Token: "t"}})
	var mu sync.Mutex
	probes := 0
	release := make(chan struct{})
	e.relay.probe = func(ctx context.Context, b, token string) (bool, error) {
		mu.Lock()
		probes++
		n := probes
		mu.Unlock()
		if n > 1 {
			<-release // the refresh hangs, as a probe of an offline member does
		}
		return true, nil
	}
	if got := e.relayCandidates(); len(got) != 1 {
		t.Fatalf("cold candidates = %v", got)
	}
	// Age the verdict past its TTL.
	e.relay.mu.Lock()
	v := e.relay.verdicts[base]
	v.at = time.Now().Add(-2 * relayProbeTTL)
	e.relay.verdicts[base] = v
	e.relay.mu.Unlock()

	done := make(chan []string, 1)
	go func() { done <- e.relayCandidates() }()
	select {
	case got := <-done:
		if len(got) != 1 {
			t.Fatalf("stale candidates = %v, want the previous verdict", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a call waited on the refresh of a stale verdict")
	}
	// The refresh starts in the background (exactly one), and another call during it starts no second.
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return probes
	}
	for deadline := time.Now().Add(2 * time.Second); count() < 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	e.relayCandidates()
	time.Sleep(50 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("probes = %d, want the cold probe and ONE background refresh", n)
	}
	close(release)
}
