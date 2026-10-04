package pairworkloads

import (
	"encoding/json"
	"io"
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

// The identity fallback (L4): a harness that runs as another OS user than PAIR cannot read
// node-id.json, so it asks PAIR's own loopback node-info for this node's UUID. Every server below is
// an httptest listener; nothing here dials a live PAIR.

const fbTestUUID = "00000000-0000-4000-8000-00000000d5a1"

const goodNodeInfo = `{"hostUuid":"` + fbTestUUID + `","clusterUuid":"","gpus":[]}`

// nodeInfoServer answers GET /v1/node-info with body (HTTP 200 unless status is set) and counts its
// requests.
type nodeInfoServer struct {
	*httptest.Server
	mu     sync.Mutex
	hits   int
	status int
	body   string
}

func newNodeInfoServer(t *testing.T, body string) *nodeInfoServer {
	t.Helper()
	n := &nodeInfoServer{body: body}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.hits++
		status, body := n.status, n.body
		n.mu.Unlock()
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(n.Close)
	return n
}

func (n *nodeInfoServer) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hits
}

func (n *nodeInfoServer) setStatus(code int) {
	n.mu.Lock()
	n.status = code
	n.mu.Unlock()
}

func (n *nodeInfoServer) url() string { return n.URL + "/v1/node-info" }

// ingressServer is the PAIR ingress: it tells a probe (a body that is not a frame) from a frame,
// counts both, answers the probe with probeAns and a frame with 200, and keeps the frames.
type ingressServer struct {
	*httptest.Server
	mu       sync.Mutex
	probes   int
	frames   []map[string]any
	probeCT  string
	probeRaw string
}

func newIngressServer(t *testing.T, probeAns int) *ingressServer {
	t.Helper()
	s := &ingressServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		var f map[string]any
		if json.Unmarshal(raw, &f) == nil && f["method"] != nil {
			s.frames = append(s.frames, f)
			w.WriteHeader(http.StatusOK)
			return
		}
		s.probes++
		s.probeCT, s.probeRaw = r.Header.Get("Content-Type"), string(raw)
		w.WriteHeader(probeAns)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ingressServer) probeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes
}

func (s *ingressServer) frameInfos() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, f := range s.frames {
		out = append(out, f["params"].(map[string]any)["workloadInfo"].(map[string]any))
	}
	return out
}

// fbEmitter builds an emitter whose app dir holds no node-id.json (a harness user that cannot see
// PAIR's files), against the given node-info and ingress.
func fbEmitter(t *testing.T, nodeInfoURL, endpoint string) *Emitter {
	t.Helper()
	return New(Config{Enabled: true, Endpoint: endpoint, AppDir: t.TempDir(), OpenDir: t.TempDir(), NodeInfoURL: nodeInfoURL})
}

// A missing node-id.json with PAIR's node-info reporting a UUID and the ingress answering: the
// emitter is enabled under that UUID and its frames carry it as originatedFrom and scheduledOn.
func TestFallbackIdentityFromNodeInfoEnablesTheEmitter(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := fbEmitter(t, ni.url(), ing.URL)
	if !e.Enabled() {
		t.Fatal("a node whose node-id.json is missing must take its identity from node-info while the ingress answers")
	}
	if self, _ := e.identity(); self != fbTestUUID {
		t.Fatalf("self = %q, want node-info's hostUuid %q", self, fbTestUUID)
	}
	e.Emit(Event{JobID: "fb-1", Model: "m", Engine: "llamacpp", State: "queued", CreatedAt: 1})
	e.Wait()
	frames := ing.frameInfos()
	if len(frames) != 1 {
		t.Fatalf("ingress got %d frames, want 1", len(frames))
	}
	if wi := frames[0]; wi["originatedFrom"] != fbTestUUID || wi["scheduledOn"] != fbTestUUID {
		t.Fatalf("frame identity = originatedFrom %v scheduledOn %v, want %s", wi["originatedFrom"], wi["scheduledOn"], fbTestUUID)
	}
}

// The probe is a POST of an empty JSON object with a JSON content type, and it is not a frame: the
// ingress counts it as a probe, never as a card.
func TestFallbackIngressProbeIsAnEmptyJSONPost(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := fbEmitter(t, ni.url(), ing.URL)
	if !e.Enabled() {
		t.Fatal("not enabled")
	}
	if ing.probeCount() != 1 || len(ing.frameInfos()) != 0 {
		t.Fatalf("probes = %d frames = %d, want one probe and no card", ing.probeCount(), len(ing.frameInfos()))
	}
	ing.mu.Lock()
	defer ing.mu.Unlock()
	if ing.probeCT != "application/json" || ing.probeRaw != "{}" {
		t.Fatalf("probe = Content-Type %q body %q, want application/json and {}", ing.probeCT, ing.probeRaw)
	}
}

// Any HTTP answer proves a listener: a 404, a 405 or a 5xx from the ingress route still counts.
func TestFallbackAcceptsAnyHTTPAnswerFromTheIngress(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusInternalServerError} {
		ni := newNodeInfoServer(t, goodNodeInfo)
		ing := newIngressServer(t, code)
		if e := fbEmitter(t, ni.url(), ing.URL); !e.Enabled() {
			t.Errorf("an ingress answering %d is up; the fallback identity must be accepted", code)
		}
	}
}

// A box with node-info but NO ingress (a view-only node) stays disabled.
func TestFallbackStaysOffWithoutAnIngress(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	gone := ing.URL
	ing.Close() // nothing listens there any more
	e := fbEmitter(t, ni.url(), gone)
	if e.Enabled() {
		t.Fatal("node-info alone must not enable the emitter: a card posted to a missing ingress goes nowhere")
	}
	if ni.count() != 1 {
		t.Fatalf("node-info probed %d times", ni.count())
	}
}

// Anything other than a 200 JSON answer carrying a canonical UUID is no identity, and the ingress is
// not even probed.
func TestFallbackRejectsAnUnusableNodeInfoAnswer(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"not found":      {http.StatusNotFound, goodNodeInfo},
		"forbidden":      {http.StatusForbidden, goodNodeInfo},
		"not json":       {0, "<html>hi</html>"},
		"no hostUuid":    {0, `{"clusterUuid":""}`},
		"empty hostUuid": {0, `{"hostUuid":""}`},
		"not a uuid":     {0, `{"hostUuid":"node-a"}`},
		"short uuid":     {0, `{"hostUuid":"00000000-0000-4000-8000-00000000d5a"}`},
		"braced uuid":    {0, `{"hostUuid":"{` + fbTestUUID + `}"}`},
		"path in uuid":   {0, `{"hostUuid":"../` + fbTestUUID + `"}`},
		"number":         {0, `{"hostUuid":12345}`},
		"array":          {0, `{"hostUuid":["` + fbTestUUID + `"]}`},
	}
	for name, c := range cases {
		ni := newNodeInfoServer(t, c.body)
		ni.setStatus(c.status)
		ing := newIngressServer(t, http.StatusBadRequest)
		e := fbEmitter(t, ni.url(), ing.URL)
		if e.Enabled() {
			t.Errorf("%s: an unusable node-info answer must leave the emitter disabled", name)
		}
		if ing.probeCount() != 0 {
			t.Errorf("%s: the ingress was probed %d times without an identity to vouch for", name, ing.probeCount())
		}
	}
}

func TestFallbackAcceptsAnUppercaseUUID(t *testing.T) {
	up := strings.ToUpper(fbTestUUID)
	ni := newNodeInfoServer(t, `{"hostUuid":"`+up+`"}`)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := fbEmitter(t, ni.url(), ing.URL)
	if self, _ := e.identity(); self != up {
		t.Fatalf("self = %q, want the canonical UUID verbatim %q", self, up)
	}
}

// A node-id.json that exists but cannot be read (here a directory stands where the file should be,
// which fails the read the way a permission denial does) takes the fallback too.
func TestFallbackWhenNodeIDIsUnreadable(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	app := t.TempDir()
	if err := os.Mkdir(filepath.Join(app, "node-id.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := New(Config{Enabled: true, Endpoint: ing.URL, AppDir: app, OpenDir: t.TempDir(), NodeInfoURL: ni.url()})
	if self, _ := e.identity(); self != fbTestUUID {
		t.Fatalf("self = %q, want the fallback identity", self)
	}
}

// The primary path is untouched: a readable node-id.json never asks node-info or the ingress, and the
// identity is the file's whatever node-info says.
func TestReadableNodeIDNeverUsesTheFallback(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := New(Config{Enabled: true, Endpoint: ing.URL, AppDir: writePairAppDir(t), OpenDir: t.TempDir(), NodeInfoURL: ni.url()})
	self, members := e.identity()
	if self != "self-uuid" || members["node-b"] != "node-b-uuid" {
		t.Fatalf("identity = %q %v, want the file's", self, members)
	}
	if n := ni.count(); n != 0 {
		t.Fatalf("node-info asked %d times with a readable node-id.json", n)
	}
	if ing.probeCount() != 0 {
		t.Fatalf("ingress probed %d times with a readable node-id.json", ing.probeCount())
	}
}

// A node-id.json that is readable but names nothing is a PAIR that is installed and broken, not a
// permission problem: it stays disabled as before, without asking node-info.
func TestMalformedNodeIDDoesNotUseTheFallback(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, "node-id.json"), []byte(`{"node_uuid":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(Config{Enabled: true, Endpoint: ing.URL, AppDir: app, OpenDir: t.TempDir(), NodeInfoURL: ni.url()})
	if e.Enabled() || ni.count() != 0 {
		t.Fatalf("enabled=%v node-info hits=%d, want a disabled emitter that asked nobody", e.Enabled(), ni.count())
	}
}

// Without a node-info URL (a bare Config: every test that builds an emitter by hand) the fallback is
// off, so no emitter can reach a live PAIR it did not name.
func TestNoNodeInfoURLMeansNoFallback(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := fbEmitter(t, "", ing.URL)
	if e.Enabled() || ni.count() != 0 {
		t.Fatalf("enabled=%v node-info hits=%d", e.Enabled(), ni.count())
	}
	if ing.probeCount() != 0 {
		t.Fatalf("the ingress was probed %d times with the fallback off", ing.probeCount())
	}
}

// Only a loopback node-info is ever dialled. The addresses below are documentation space: if the guard
// failed the probe would sit there until its timeout and the refusal would not be instant.
func TestFallbackRefusesANonLoopbackNodeInfoURL(t *testing.T) {
	ing := newIngressServer(t, http.StatusBadRequest)
	for _, u := range []string{
		"http://192.0.2.1:14318/v1/node-info",
		"http://node-info.example.test/v1/node-info",
		"http://[2001:db8::1]:14318/v1/node-info",
		"ftp://127.0.0.1/v1/node-info",
		"127.0.0.1:14318/v1/node-info",
		"http://127.0.0.1.example.test/v1/node-info",
	} {
		e := fbEmitter(t, u, ing.URL)
		start := time.Now()
		if e.Enabled() {
			t.Errorf("%s: a non-loopback node-info must be refused", u)
		}
		if d := time.Since(start); d > nodeInfoTimeout/2 {
			t.Errorf("%s: refusal took %v: it was dialled", u, d)
		}
	}
	if ing.probeCount() != 0 {
		t.Fatalf("ingress probed %d times behind a refused node-info URL", ing.probeCount())
	}
}

func TestLoopbackHTTPURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://127.0.0.1:14318/v1/node-info": true,
		"http://localhost:14318/v1/node-info": true,
		"http://[::1]:14318/v1/node-info":     true,
		"https://127.0.0.1:14319/x":           true,
		"http://127.0.0.2/x":                  true,
		"http://192.0.2.1/x":                  false,
		"http://0.0.0.0/x":                    false,
		"":                                    false,
		"http://":                             false,
		"gopher://127.0.0.1/x":                false,
	} {
		if got := loopbackHTTPURL(raw); got != want {
			t.Errorf("loopbackHTTPURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// Nothing is probed per call: many calls inside the TTL ask node-info and the ingress once each, an
// identity reload inside fallbackTTL asks neither again, and a success older than fallbackTTL is asked
// again.
func TestFallbackProbesAreCached(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := fbEmitter(t, ni.url(), ing.URL)
	for i := 0; i < 50; i++ {
		if !e.Enabled() {
			t.Fatal("not enabled")
		}
		e.Emit(Event{JobID: "c", Model: "m", Engine: "llamacpp", State: "queued", CreatedAt: 1})
	}
	e.Wait()
	if n := ni.count(); n != 1 {
		t.Fatalf("node-info probed %d times across 50 calls, want 1", n)
	}
	if ing.probeCount() != 1 {
		t.Fatalf("ingress probed %d times across 50 calls, want 1", ing.probeCount())
	}

	e.idMu.Lock()
	e.idAt = time.Now().Add(-2 * identityTTL)
	e.idMu.Unlock()
	if !e.Enabled() {
		t.Fatal("not enabled after an identity reload")
	}
	if n := ni.count(); n != 1 {
		t.Fatalf("node-info probed %d times after an identity reload inside the TTL, want still 1", n)
	}
	if ing.probeCount() != 1 {
		t.Fatalf("ingress probed %d times after an identity reload inside the TTL, want still 1", ing.probeCount())
	}

	e.idMu.Lock()
	e.idAt = time.Now().Add(-2 * identityTTL)
	e.fbUUIDAt = time.Now().Add(-2 * fallbackTTL)
	e.fbIngressAt = time.Now().Add(-2 * fallbackTTL)
	e.idMu.Unlock()
	if !e.Enabled() {
		t.Fatal("not enabled after the TTL")
	}
	if n := ni.count(); n != 2 {
		t.Fatalf("node-info probed %d times after the TTL, want 2", n)
	}
	if ing.probeCount() != 2 {
		t.Fatalf("ingress probed %d times after the TTL, want 2", ing.probeCount())
	}
}

// A failed fallback is retried on the identity cadence, not on every call: a box with no usable
// node-info costs one failed request a minute, not one per frame.
func TestFailedFallbackIsNotReprobedPerCall(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ni.setStatus(http.StatusInternalServerError)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := fbEmitter(t, ni.url(), ing.URL)
	for i := 0; i < 20; i++ {
		if e.Enabled() {
			t.Fatal("enabled on a failing node-info")
		}
	}
	if n := ni.count(); n != 1 {
		t.Fatalf("node-info probed %d times across 20 calls, want 1", n)
	}
	ni.setStatus(0) // node-info recovers; the next identity reload picks it up
	e.idMu.Lock()
	e.idAt = time.Now().Add(-2 * identityTTL)
	e.idMu.Unlock()
	if !e.Enabled() {
		t.Fatal("a recovered node-info must enable the emitter on the next identity reload")
	}
}

// members.json unreadable: only this node resolves. The node itself keeps its card on this box; any
// other node's card carries no scheduledOn, as for any node PAIR does not know.
func TestFallbackWithoutMembersResolvesOnlySelf(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := fbEmitter(t, ni.url(), ing.URL)
	if u, ok := e.resolveNode(Event{JobID: "j"}); !ok || u != fbTestUUID {
		t.Fatalf("this box = %q, %v, want the fallback identity", u, ok)
	}
	if u, ok := e.resolveNode(Event{JobID: "j", Node: "node-b"}); ok || u != "" {
		t.Fatalf("an unknown node resolved to %q, %v, want none (scheduledOn null)", u, ok)
	}
}

// members.json readable beside an unreadable node-id.json (PAIR's cluster dir can be world-readable):
// the members resolve as they do on the primary path.
func TestFallbackStillReadsReadableMembers(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	app := t.TempDir()
	if err := os.MkdirAll(filepath.Join(app, "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	members := `[{"nodeUuid":"node-b-uuid","name":"node-b"},{"nodeUuid":"` + fbTestUUID + `","name":"node-d"}]`
	if err := os.WriteFile(filepath.Join(app, "cluster", "members.json"), []byte(members), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(Config{Enabled: true, Endpoint: ing.URL, AppDir: app, OpenDir: t.TempDir(), NodeInfoURL: ni.url()})
	if u, ok := e.resolveNode(Event{JobID: "j", Node: "node-b"}); !ok || u != "node-b-uuid" {
		t.Fatalf("node-b = %q, %v", u, ok)
	}
	if u, ok := e.resolveNode(Event{JobID: "j", Node: "node-d"}); !ok || u != fbTestUUID {
		t.Fatalf("node-d (this node by name) = %q, %v", u, ok)
	}
}

// node-info does not follow a redirect off the loopback service.
func TestFallbackDoesNotFollowARedirect(t *testing.T) {
	target := newNodeInfoServer(t, goodNodeInfo)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.url(), http.StatusFound)
	}))
	t.Cleanup(redir.Close)
	ing := newIngressServer(t, http.StatusBadRequest)
	e := fbEmitter(t, redir.URL+"/v1/node-info", ing.URL)
	if e.Enabled() || target.count() != 0 {
		t.Fatalf("enabled=%v redirect target hits=%d, want a refusal that followed nothing", e.Enabled(), target.count())
	}
}

func TestFromConfigNodeInfoURL(t *testing.T) {
	t.Setenv("OFFLOAD_PAIR_APPDIR", "")
	if got := FromConfig(config.Config{}).NodeInfoURL; got != DefaultNodeInfoURL {
		t.Fatalf("default = %q, want %q", got, DefaultNodeInfoURL)
	}
	if got := FromConfig(config.Config{PairNodeInfoURL: " http://127.0.0.1:9999/v1/node-info "}).NodeInfoURL; got != "http://127.0.0.1:9999/v1/node-info" {
		t.Fatalf("configured = %q", got)
	}
	// A box that names PAIR's data dir on purpose does not get the default port: a missing node-id.json
	// there is "PAIR is not installed" (and a test fixture can never reach a live PAIR).
	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
	if got := FromConfig(config.Config{}).NodeInfoURL; got != "" {
		t.Fatalf("with OFFLOAD_PAIR_APPDIR set the default must not apply, got %q", got)
	}
	if got := FromConfig(config.Config{PairNodeInfoURL: "http://127.0.0.1:9999/v1/node-info"}).NodeInfoURL; got == "" {
		t.Fatal("an explicit pair_node_info_url applies whatever OFFLOAD_PAIR_APPDIR says")
	}
}

// The asker's wire headers ask the serving node to card the job only when this box's own emitter is
// not enabled. With the fallback identity this box IS enabled, so it must not ask (two cards); the
// wire emitter has to carry the same node-info URL as the emitter that cards.
func TestWireHeadersAgreeWithTheFallbackIdentity(t *testing.T) {
	ni := newNodeInfoServer(t, goodNodeInfo)
	ing := newIngressServer(t, http.StatusBadRequest)
	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
	cfg := config.Config{PairWorkloadsEnabled: true, PairWorkloadsEndpoint: ing.URL, PairNodeInfoURL: ni.url()}
	h := http.Header{}
	WireHeadersFor(cfg, h)
	if got := h.Get(core.PairCardHeader); got != "" {
		t.Fatalf("an enabled (fallback) asker asked the node to card the job: %q", got)
	}
	// Without the URL the same box reads as disabled and asks the node to card the job.
	h = http.Header{}
	WireHeadersFor(config.Config{PairWorkloadsEnabled: true, PairWorkloadsEndpoint: ing.URL}, h)
	if got := h.Get(core.PairCardHeader); got != core.PairCardNode {
		t.Fatalf("a disabled asker must ask the node to card the job, header = %q", got)
	}
}
