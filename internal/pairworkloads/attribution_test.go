package pairworkloads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// writeAttributionAppDir builds a PAIR app dir with RFC 5737 addresses: this box is node-a, node-b is
// a cluster member, and (optionally) node-v is a view-only node that is not a member.
func writeAttributionAppDir(t *testing.T, viewOnly string) string {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "node-id.json"), []byte(`{"node_uuid":"self-uuid","created_at":1}`), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "cluster"), 0o755))
	members := `[{"id":"node-b","nodeUuid":"node-b-uuid","name":"node-b","ipAddress":"192.0.2.2"},{"id":"node-a","nodeUuid":"self-uuid","name":"Node-A","ipAddress":"127.0.0.1"}]`
	must(os.WriteFile(filepath.Join(dir, "cluster", "members.json"), []byte(members), 0o644))
	if viewOnly != "" {
		must(os.MkdirAll(filepath.Join(dir, "configs"), 0o755))
		must(os.WriteFile(filepath.Join(dir, "configs", "view-only-nodes.json"), []byte(viewOnly), 0o644))
	}
	return dir
}

const viewOnlyFixture = `[{"name":"Node-V","address":"192.0.2.50","port":14321,"nodeUuid":"view-uuid"},
{"name":"node-b","address":"192.0.2.99","port":14321,"nodeUuid":"view-shadow-uuid"}]`

func scheduledOn(t *testing.T, e *Emitter, ev Event) any {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e.cfg.Endpoint = srv.URL
	e.client = srv.Client()
	if err := e.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	return c.info(c.count() - 1)["scheduledOn"]
}

// D10: a job that ran on a VIEW-ONLY PAIR node (one the desktop lists in configs/view-only-nodes.json
// and PAIR's members.json does not) carried scheduledOn null for 256 cards. The node resolves from the
// view-only list by name or address, after the members, case-insensitively.
func TestViewOnlyNodesResolveByNameAddressAndAlias(t *testing.T) {
	e := New(Config{Enabled: true, OpenDir: t.TempDir(), AppDir: writeAttributionAppDir(t, viewOnlyFixture)})
	for _, c := range []struct {
		name string
		ev   Event
		want any
	}{
		{"name, any case", Event{JobID: "v1", Node: "NODE-v", State: "running", CreatedAt: 1, StartedAt: 1}, "view-uuid"},
		{"address", Event{JobID: "v2", Node: "192.0.2.50", State: "running", CreatedAt: 1, StartedAt: 1}, "view-uuid"},
		{"alias after an unresolvable dispatch host", Event{JobID: "v3", Node: "node-v.example.invalid", NodeAliases: []string{"node-v"}, State: "running", CreatedAt: 1, StartedAt: 1}, "view-uuid"},
		{"members win over a view-only entry of the same name", Event{JobID: "v4", Node: "node-b", State: "running", CreatedAt: 1, StartedAt: 1}, "node-b-uuid"},
		{"members win by address too", Event{JobID: "v5", Node: "192.0.2.2", State: "running", CreatedAt: 1, StartedAt: 1}, "node-b-uuid"},
		{"a view-only address is not a member name", Event{JobID: "v6", Node: "192.0.2.99", State: "running", CreatedAt: 1, StartedAt: 1}, "view-shadow-uuid"},
		{"nothing resolves", Event{JobID: "v7", Node: "node-z", State: "running", CreatedAt: 1, StartedAt: 1}, nil},
	} {
		if got := scheduledOn(t, e, c.ev); got != c.want {
			t.Errorf("%s: scheduledOn = %v, want %v", c.name, got, c.want)
		}
	}
}

// The view-only list shares the members' cache: edited on disk it is re-read after identityTTL, and a
// missing, empty or malformed file only means no view-only nodes.
func TestViewOnlyNodesReloadAndTolerateBadFiles(t *testing.T) {
	dir := writeAttributionAppDir(t, "")
	e := New(Config{Enabled: true, OpenDir: t.TempDir(), AppDir: dir})
	ev := Event{JobID: "r1", Node: "node-v", State: "running", CreatedAt: 1, StartedAt: 1}
	if got := scheduledOn(t, e, ev); got != nil {
		t.Fatalf("no view-only file: scheduledOn = %v, want null", got)
	}
	path := filepath.Join(dir, "configs", "view-only-nodes.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		e.idMu.Lock()
		e.idAt = time.Time{} // past identityTTL
		e.idMu.Unlock()
	}
	write(`[{"name":"node-v","address":"192.0.2.50","port":1,"nodeUuid":"view-uuid"}]`)
	if got := scheduledOn(t, e, Event{JobID: "r2", Node: "node-v", State: "running", CreatedAt: 1, StartedAt: 1}); got != "view-uuid" {
		t.Fatalf("after the file appeared: scheduledOn = %v", got)
	}
	// Inside the TTL a rewrite is not re-read (the same caching as members.json).
	if err := os.WriteFile(path, []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scheduledOn(t, e, Event{JobID: "r3", Node: "node-v", State: "running", CreatedAt: 1, StartedAt: 1}); got != "view-uuid" {
		t.Fatalf("inside the TTL the cached list must still answer: scheduledOn = %v", got)
	}
	for _, bad := range []string{``, `not json`, `{"name":"node-v"}`, `[{"name":"","address":"","nodeUuid":"x"},{"name":"node-w","nodeUuid":""}]`} {
		write(bad)
		if got := scheduledOn(t, e, Event{JobID: "r4", Node: "node-w", State: "running", CreatedAt: 1, StartedAt: 1}); got != nil {
			t.Fatalf("file %q: scheduledOn = %v, want null", bad, got)
		}
		// Members still resolve whatever the view-only file holds.
		if got := scheduledOn(t, e, Event{JobID: "r5", Node: "node-b", State: "running", CreatedAt: 1, StartedAt: 1}); got != "node-b-uuid" {
			t.Fatalf("file %q broke member resolution: %v", bad, got)
		}
	}
}

func TestNodeNameUsesTheDispatchHost(t *testing.T) {
	cases := map[[2]string]string{
		{"http://NODE-B:18811", "node-b-fleet16"}:    "node-b",
		{"http://node-b:18811/", "node-b-fleet16"}:   "node-b",
		{"http://192.0.2.7:18811", "node-b-fleet16"}: "192.0.2.7",
		{"", "node-b-fleet16"}:                       "node-b-fleet16",
		{"::not a url::", "node-b-fleet16"}:          "node-b-fleet16",
		{"http://node-b.tail.example:18811", "x"}:    "node-b.tail.example",
	}
	for in, want := range cases {
		if got := NodeName(in[0], in[1]); got != want {
			t.Errorf("NodeName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// The asker name a request carries: the PAIR member name of this box when the emitter is enabled and
// the box is a member, else the short lowercase hostname.
func TestAskerNameIsTheMemberNameElseTheShortHostname(t *testing.T) {
	enabled := New(Config{Enabled: true, OpenDir: t.TempDir(), AppDir: writeAttributionAppDir(t, "")})
	if got := enabled.AskerName(); got != "node-a" {
		t.Fatalf("enabled member: AskerName = %q, want the lowercased member name node-a", got)
	}
	disabled := New(Config{Enabled: false, AppDir: writeAttributionAppDir(t, "")})
	host, _ := os.Hostname()
	host = strings.ToLower(strings.SplitN(host, ".", 2)[0])
	if got := disabled.AskerName(); got != core.SanitizeAsker(host) {
		t.Fatalf("disabled: AskerName = %q, want the short hostname %q", got, host)
	}
	// Enabled but this box's UUID is in no member entry: the hostname again.
	dir := writeAttributionAppDir(t, "")
	if err := os.WriteFile(filepath.Join(dir, "cluster", "members.json"), []byte(`[{"nodeUuid":"other","name":"node-b"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	nonMember := New(Config{Enabled: true, OpenDir: t.TempDir(), AppDir: dir})
	if got := nonMember.AskerName(); got != core.SanitizeAsker(host) {
		t.Fatalf("non-member: AskerName = %q, want the short hostname %q", got, host)
	}
}

// D7: an asker that will NOT card a job (its emitter is not enabled) sends X-Offload-Pair-Card: node so
// the serving node cards it; an asker whose emitter is enabled sends only its name. Every asker sends
// its name.
func TestWireHeadersSignalNodeCardingOnlyWhenTheAskerWillNotCard(t *testing.T) {
	enabled := New(Config{Enabled: true, OpenDir: t.TempDir(), AppDir: writeAttributionAppDir(t, "")})
	h := http.Header{}
	enabled.SetWireHeaders(h)
	if h.Get(core.AskerHeader) != "node-a" {
		t.Fatalf("enabled asker header = %q", h.Get(core.AskerHeader))
	}
	if v, present := h[http.CanonicalHeaderKey(core.PairCardHeader)]; present {
		t.Fatalf("an asker that cards the job itself must not send %s (sent %v)", core.PairCardHeader, v)
	}

	for name, e := range map[string]*Emitter{
		"config off":        New(Config{Enabled: false, AppDir: writeAttributionAppDir(t, "")}),
		"PAIR not on a box": New(Config{Enabled: true, AppDir: t.TempDir()}),
		"nil emitter":       nil,
	} {
		h := http.Header{}
		e.SetWireHeaders(h)
		if h.Get(core.PairCardHeader) != core.PairCardNode {
			t.Errorf("%s: %s = %q, want %q", name, core.PairCardHeader, h.Get(core.PairCardHeader), core.PairCardNode)
		}
		if h.Get(core.AskerHeader) == "" {
			t.Errorf("%s: every asker names itself", name)
		}
	}
}

// WireHeadersFor builds the same headers from a config alone (the accelerator lane has no runner).
func TestWireHeadersForConfig(t *testing.T) {
	dir := writeAttributionAppDir(t, "")
	t.Setenv("OFFLOAD_PAIR_APPDIR", dir)
	h := http.Header{}
	WireHeadersFor(config.Config{PairWorkloadsEnabled: true}, h)
	if h.Get(core.AskerHeader) != "node-a" || h.Get(core.PairCardHeader) != "" {
		t.Fatalf("enabled config: %v", h)
	}
	h = http.Header{}
	WireHeadersFor(config.Config{}, h)
	if h.Get(core.PairCardHeader) != core.PairCardNode || h.Get(core.AskerHeader) == "" {
		t.Fatalf("default config: %v", h)
	}
}

// A row whose card its writer already decided is not carded again by the observer.
func TestLedgerObserverSkipsCallerCardedRows(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, OpenDir: t.TempDir(), AppDir: writeAttributionAppDir(t, "")})
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e.AttachLedger(l)
	for _, r := range []ledger.Entry{
		{TS: 100, Task: "vqa", ModelTier: "qwen3-vl-8b", LatencyMs: 10, CardByCaller: true, Node: "node-b"},
		{TS: 101, Task: "summarize", ModelTier: "gemma-4-e4b", LatencyMs: 10},
	} {
		if err := l.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	e.Wait()
	if c.count() != 1 || c.info(0)["model"] != "gemma-4-e4b" {
		t.Fatalf("frames = %d; only the row nobody carded may be carded", c.count())
	}
}

func TestEngineForRemoteLaneTasks(t *testing.T) {
	for task, want := range map[string]string{
		"compose-video": "hyperframes", "compose_video": "hyperframes", "compose-project": "hyperframes",
		"run-graph": "comfyui", "stt": "whispercpp", "vision": "llamacpp", "text": "llamacpp", "agent_run": "llamacpp",
	} {
		if got := EngineFor(task, ""); got != want {
			t.Errorf("EngineFor(%q) = %q, want %q", task, got, want)
		}
	}
}
