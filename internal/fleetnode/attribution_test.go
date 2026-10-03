package fleetnode

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

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// pairNode is the serving node's PAIR side: a stand-in ingress recording every frame, and an emitter
// enabled against a PAIR app dir (this node is a member named node-s).
type pairNode struct {
	mu     sync.Mutex
	frames []map[string]any
	e      *pairworkloads.Emitter
	open   string // the emitter's open-card register directory
}

func newPairNode(t *testing.T, enabled bool) *pairNode {
	t.Helper()
	pn := &pairNode{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f map[string]any
		_ = json.NewDecoder(r.Body).Decode(&f)
		pn.mu.Lock()
		pn.frames = append(pn.frames, f)
		pn.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	app := t.TempDir()
	for name, body := range map[string]string{
		"node-id.json":                           `{"node_uuid":"node-s-uuid"}`,
		filepath.Join("cluster", "members.json"): `[{"nodeUuid":"node-s-uuid","name":"node-s"}]`,
	} {
		p := filepath.Join(app, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pn.open = t.TempDir()
	pn.e = pairworkloads.New(pairworkloads.Config{Enabled: enabled, Endpoint: srv.URL, AppDir: app, OpenDir: pn.open})
	return pn
}

// cards waits for the emitter and returns the frames by state, failing unless they are one card.
func (pn *pairNode) cards(t *testing.T) map[string]map[string]any {
	t.Helper()
	pn.e.Wait()
	pn.mu.Lock()
	defer pn.mu.Unlock()
	out := map[string]map[string]any{}
	ids := map[any]bool{}
	for _, f := range pn.frames {
		wi := f["params"].(map[string]any)["workloadInfo"].(map[string]any)
		ids[wi["id"]] = true
		out[wi["state"].(string)] = wi
	}
	if len(ids) > 1 {
		t.Fatalf("one job opened %d cards: %v", len(ids), ids)
	}
	return out
}

func (pn *pairNode) frameCount() int {
	pn.e.Wait()
	pn.mu.Lock()
	defer pn.mu.Unlock()
	return len(pn.frames)
}

func pairOpts(pn *pairNode) *Options {
	return &Options{
		NodeID: "testnode", Snapshot: goodSnapshot, Footprints: func() []FootprintEntry { return nil },
		GpuVendor: "nvidia", GpuArch: "ampere", LoopbackListener: true, Pair: pn.e,
	}
}

var askerHeaders = map[string]string{core.AskerHeader: "node-q", core.PairCardHeader: core.PairCardNode}

// D7: a job whose asker signalled that it will not card it gets ONE card from this node: queued at
// admit, running at start, completed at finish; the id is the fleet job id, the node is this box, the
// requester names the asker, and the engine is the task's own.
func TestNodeCardsAJobOnTheAskersSignal(t *testing.T) {
	pn := newPairNode(t, true)
	started := make(chan struct{})
	release := make(chan struct{})
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		close(started)
		<-release
		return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`), Meta: core.Meta{Model: "sdxl-lightning"}}
	}}
	s, _ := newTestServer(t, imageCfg(), fr, pairOpts(pn))
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"media-77","task_type":"image-gen","payload":{"prompt":"hi"}}`, askerHeaders); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d (body %s)", rec.Code, rec.Body.String())
	}
	<-started
	mid := pn.cards(t)
	if mid["queued"] == nil || mid["running"] == nil || mid["completed"] != nil {
		t.Fatalf("while the job runs the card is queued and running, never terminal: %v", mid)
	}
	close(release)
	pollJob(t, s, "media-77", JobDone)

	cards := pn.cards(t)
	if len(cards) != 3 {
		t.Fatalf("states = %v, want queued, running, completed", cards)
	}
	for state, wi := range cards {
		if wi["id"] != "media-77" || wi["originatedFrom"] != "node-s-uuid" || wi["scheduledOn"] != "node-s-uuid" ||
			wi["engine"] != "comfyui" || wi["requesterId"] != "offload-harness/fleet:node-q" {
			t.Errorf("%s frame: %v", state, wi)
		}
	}
	if cards["queued"]["startedAt"] != nil || cards["running"]["startedAt"] == nil || cards["completed"]["completedAt"] == nil || cards["completed"]["error"] != nil {
		t.Errorf("lifecycle timestamps: %v", cards)
	}
	if cards["completed"]["model"] != "sdxl-lightning" {
		t.Errorf("the terminal frame names the model the job ran: %v", cards["completed"])
	}
	if pn.frameCount() != 3 {
		t.Errorf("frames = %d, want exactly 3", pn.frameCount())
	}
}

// Without the signal the node cards nothing (the asker does, or an older asker would double it), but
// it still records who asked on the request its runner sees (D11).
func TestNodeDoesNotCardWithoutTheSignalButRecordsTheAsker(t *testing.T) {
	pn := newPairNode(t, true)
	fr := &fakeRunner{}
	s, _ := newTestServer(t, imageCfg(), fr, pairOpts(pn))
	for id, h := range map[string]map[string]string{
		"nosignal-1": {core.AskerHeader: "node-q"},
		"nosignal-2": nil,
		"nosignal-3": {core.PairCardHeader: "yes please"}, // only the one legal value counts
	} {
		if rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"`+id+`","task_type":"image-gen","payload":{"prompt":"hi"}}`, h); rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch = %d", rec.Code)
		}
		pollJob(t, s, id, JobDone)
	}
	if n := pn.frameCount(); n != 0 {
		t.Fatalf("frames = %d, want none: no asker signalled that the node cards the job", n)
	}
	got := map[string]string{}
	for _, r := range fr.requests() {
		got[r.FleetJobID] = r.Requester
	}
	if got["nosignal-1"] != "node-q" || got["nosignal-2"] != "" || got["nosignal-3"] != "" {
		t.Fatalf("requester seen by the runner = %v, want the asker's name where the header was present", got)
	}
}

// The signal means nothing on a node whose own emitter is not enabled (key off, PAIR absent).
func TestNodeWithADisabledEmitterCardsNothing(t *testing.T) {
	pn := newPairNode(t, false)
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, pairOpts(pn))
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"off-1","task_type":"image-gen","payload":{"prompt":"hi"}}`, askerHeaders); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d", rec.Code)
	}
	pollJob(t, s, "off-1", JobDone)
	if n := pn.frameCount(); n != 0 {
		t.Fatalf("frames = %d from a node with no enabled emitter", n)
	}
	// No Options.Pair at all is the same.
	s2, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	if rec := do(t, s2, http.MethodPost, "/fleet/dispatch", `{"job_id":"off-2","task_type":"image-gen","payload":{"prompt":"hi"}}`, askerHeaders); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d", rec.Code)
	}
	pollJob(t, s2, "off-2", JobDone)
}

// A job that deferred closes its card failed with the reason; a card is never completed over a
// refusal.
func TestNodeCardFailsWithTheReasonOfADeferredJob(t *testing.T) {
	pn := newPairNode(t, true)
	fr := &fakeRunner{fn: func(context.Context, core.Request) core.Result {
		return core.Deferf("compose_video: LINT_ERRORS: 2 errors", "", core.Meta{})
	}}
	s, _ := newTestServer(t, imageCfg(), fr, pairOpts(pn))
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"bad-1","task_type":"image-gen","payload":{"prompt":"hi"}}`, askerHeaders); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d", rec.Code)
	}
	pollJob(t, s, "bad-1", JobError)
	cards := pn.cards(t)
	if cards["failed"] == nil || cards["completed"] != nil || cards["failed"]["error"] != "compose_video: LINT_ERRORS: 2 errors" {
		t.Fatalf("cards = %v, want one failed card carrying the reason", cards)
	}
}

// The asker value is untrusted: it is cut to printable text of at most 64 characters before it is
// recorded or shown.
func TestNodeSanitizesTheAskerName(t *testing.T) {
	pn := newPairNode(t, true)
	fr := &fakeRunner{}
	s, _ := newTestServer(t, imageCfg(), fr, pairOpts(pn))
	evil := "node-q\x00\x1b[31m  red\r\nteam" + strings.Repeat("x", 200)
	h := map[string]string{core.AskerHeader: evil, core.PairCardHeader: core.PairCardNode}
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"evil-1","task_type":"image-gen","payload":{"prompt":"hi"}}`, h); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d", rec.Code)
	}
	pollJob(t, s, "evil-1", JobDone)
	req := fr.requests()[0]
	if len([]rune(req.Requester)) > 64 || strings.ContainsAny(req.Requester, "\x00\x1b\r\n") || !strings.HasPrefix(req.Requester, "node-q") {
		t.Fatalf("requester = %q", req.Requester)
	}
	cards := pn.cards(t)
	rid, _ := cards["queued"]["requesterId"].(string)
	if rid != "offload-harness/fleet:"+req.Requester || strings.ContainsAny(rid, "\x00\x1b\r\n") {
		t.Fatalf("requesterId = %q, want the sanitized name", rid)
	}
}

func TestSanitizeAsker(t *testing.T) {
	long := strings.Repeat("a", 100)
	for in, want := range map[string]string{
		"":                      "",
		"  node-q  ":            "node-q",
		"node\x00-q":            "node-q",
		"a \t\r\n b":            "a b",
		"\x1b[31m":              "[31m",
		long:                    long[:64],
		strings.Repeat("é", 70): strings.Repeat("é", 64),
	} {
		if got := core.SanitizeAsker(in); got != want {
			t.Errorf("SanitizeAsker(%q) = %q, want %q", in, got, want)
		}
	}
}

// A job dropped before it ever started (withdrawn, or the node drained) never reaches the run
// closure, so its queued card would sit open: the OnDropped hook closes it failed.
func TestNodeCardOfAJobDroppedBeforeItStartedClosesFailed(t *testing.T) {
	pn := newPairNode(t, true)
	cfg := agentLaneCfg(t, withdrawToken)
	cfg.FleetMaxConcurrentJobs = 1
	rel := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(rel) }) }
	s, jobs := newTestServer(t, cfg, &fakeRunner{fn: blockingAgentRun(rel)}, pairOpts(pn))
	t.Cleanup(release)
	h := map[string]string{"Authorization": "Bearer " + withdrawToken, core.AskerHeader: "node-q", core.PairCardHeader: core.PairCardNode}
	for _, id := range []string{"running-1", "queued-1"} {
		if rec := do(t, s, http.MethodPost, "/fleet/dispatch", agentLaneBody(id), h); rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s = %d (body %s)", id, rec.Code, rec.Body.String())
		}
	}
	waitJobState(t, jobs, "running-1", JobRunning)
	if v, _ := jobs.Get("queued-1"); v.State != JobAccepted {
		t.Fatalf("queued-1 is %v, want accepted behind the held slot", v.State)
	}
	if rec := do(t, s, http.MethodDelete, "/fleet/jobs/queued-1", "", withdrawAuth); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent && rec.Code != http.StatusAccepted {
		t.Fatalf("withdraw = %d (body %s)", rec.Code, rec.Body.String())
	}
	pn.e.Wait()
	pn.mu.Lock()
	byID := map[string]map[string]bool{}
	for _, f := range pn.frames {
		wi := f["params"].(map[string]any)["workloadInfo"].(map[string]any)
		id := wi["id"].(string)
		if byID[id] == nil {
			byID[id] = map[string]bool{}
		}
		byID[id][wi["state"].(string)] = true
	}
	pn.mu.Unlock()
	if !byID["queued-1"]["queued"] || !byID["queued-1"]["failed"] || byID["queued-1"]["running"] {
		t.Fatalf("the withdrawn job's card = %v, want queued then failed, never running", byID["queued-1"])
	}
	if !byID["running-1"]["running"] || byID["running-1"]["failed"] || byID["running-1"]["completed"] {
		t.Fatalf("the running job's card = %v, want it still open", byID["running-1"])
	}
}

// D9: a PULLED job behaves exactly like a pushed one: the door is "fleet" (so the node's own pipeline
// does not card it as its own work and the observer skips its row), the asker's name rides the queued
// job to the runner, and a job whose submitter signalled that it will not card it gets its card from
// this node.
func TestPulledJobIsAttributedLikeAPushedOne(t *testing.T) {
	pn := newPairNode(t, true)
	fr := &fakeRunner{}
	cfg := imageCfg()
	cfg.Home = t.TempDir()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "agent-seat"
	cfg.FleetAuthToken = "tok"
	s, jobs := newTestServer(t, cfg, fr, pairOpts(pn))
	served := false
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fleet/queue/claim" {
			if served {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			served = true
			_ = json.NewEncoder(w).Encode(fleetqueue.Job{
				ID: "pulled-3", TaskType: "agent", Asker: "node-q\x00", PairCard: core.PairCardNode,
				Payload: json.RawMessage(`{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `}`),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != "pulled-3" {
		t.Fatalf("claimOne = %q, %v", id, ok)
	}
	waitJobState(t, jobs, "pulled-3", JobDone)

	reqs := fr.requests()
	if len(reqs) != 1 || reqs[0].Door != pairworkloads.FleetDoor || reqs[0].Requester != "node-q" || reqs[0].FleetJobID != "pulled-3" {
		t.Fatalf("runner request = %+v, want door fleet, requester node-q, fleet job id pulled-3", reqs)
	}
	cards := pn.cards(t)
	if len(cards) != 3 || cards["queued"]["id"] != "pulled-3" || cards["completed"]["requesterId"] != "offload-harness/fleet:node-q" {
		t.Fatalf("cards = %v, want one queued/running/completed card for the pulled job", cards)
	}
}

// A pulled job without the signal gets the door and the asker's name but no card.
func TestPulledJobWithoutTheSignalIsNotCarded(t *testing.T) {
	pn := newPairNode(t, true)
	fr := &fakeRunner{}
	cfg := imageCfg()
	cfg.Home = t.TempDir()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "agent-seat"
	cfg.FleetAuthToken = "tok"
	s, jobs := newTestServer(t, cfg, fr, pairOpts(pn))
	served := false
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fleet/queue/claim" {
			if served {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			served = true
			_ = json.NewEncoder(w).Encode(fleetqueue.Job{
				ID: "pulled-4", TaskType: "agent", Asker: "node-q",
				Payload: json.RawMessage(`{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `}`),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	if _, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok {
		t.Fatal("claimOne reported no claim")
	}
	waitJobState(t, jobs, "pulled-4", JobDone)
	if reqs := fr.requests(); len(reqs) != 1 || reqs[0].Door != pairworkloads.FleetDoor || reqs[0].Requester != "node-q" {
		t.Fatalf("runner request = %+v", reqs)
	}
	if n := pn.frameCount(); n != 0 {
		t.Fatalf("frames = %d, want none", n)
	}
}

// claimServing returns a holder that serves job once per entry of ids (the same job id may repeat: a
// lease-expiry re-claim hands a node the job it already holds) and then answers 204.
func claimServing(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	i := 0
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/queue/claim" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if i >= len(ids) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		id := ids[i]
		i++
		_ = json.NewEncoder(w).Encode(fleetqueue.Job{
			ID: id, TaskType: "agent", Asker: "node-q", PairCard: core.PairCardNode,
			Payload: json.RawMessage(`{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `}`),
		})
	}))
	t.Cleanup(h.Close)
	return h
}

func openMarkers(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

// A lease-expiry re-claim of a job this node already holds is a duplicate admission: it must not
// emit a second queued frame, which would reopen a card its terminal frame closed (and leave an
// open-card marker nothing closes while this process lives) or regress a running card to queued.
func TestPulledReclaimOfAFinishedJobEmitsNoSecondCard(t *testing.T) {
	pn := newPairNode(t, true)
	fr := &fakeRunner{}
	cfg := imageCfg()
	cfg.Home = t.TempDir()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "agent-seat"
	cfg.FleetAuthToken = "tok"
	s, jobs := newTestServer(t, cfg, fr, pairOpts(pn))
	holder := claimServing(t, "pulled-5", "pulled-5")
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != "pulled-5" {
		t.Fatalf("first claimOne = %q, %v", id, ok)
	}
	waitJobState(t, jobs, "pulled-5", JobDone)
	pn.e.Wait()
	if n := pn.frameCount(); n != 3 {
		t.Fatalf("frames after the first run = %d, want queued, running, completed", n)
	}
	if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != "pulled-5" {
		t.Fatalf("re-claim = %q, %v", id, ok)
	}
	if n := pn.frameCount(); n != 3 {
		t.Fatalf("frames after the re-claim = %d, want still 3: a duplicate admission opened a card", n)
	}
	if n := openMarkers(t, pn.open); n != 0 {
		t.Fatalf("%d open-card markers left: a duplicate claim reopened a closed card", n)
	}
	if got := len(fr.requests()); got != 1 {
		t.Fatalf("runner ran %d times, want 1", got)
	}
}

// A draining node refuses the claim: the card it would have opened closes failed rather than
// staying queued with no terminal frame (the lease requeues the job elsewhere).
func TestPulledClaimOnADrainingNodeClosesItsCard(t *testing.T) {
	pn := newPairNode(t, true)
	fr := &fakeRunner{}
	cfg := imageCfg()
	cfg.Home = t.TempDir()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "agent-seat"
	cfg.FleetAuthToken = "tok"
	s, jobs := newTestServer(t, cfg, fr, pairOpts(pn))
	jobs.DrainAndStop(time.Second)
	holder := claimServing(t, "pulled-6")
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	if _, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok {
		t.Fatal("claimOne reported no claim")
	}
	cards := pn.cards(t)
	if cards["failed"] == nil || cards["failed"]["error"] != "node draining" {
		t.Fatalf("cards = %v, want the card closed failed with %q", cards, "node draining")
	}
	if n := openMarkers(t, pn.open); n != 0 {
		t.Fatalf("%d open-card markers left on a refused claim", n)
	}
	if got := len(fr.requests()); got != 0 {
		t.Fatalf("a draining node ran %d jobs", got)
	}
}
