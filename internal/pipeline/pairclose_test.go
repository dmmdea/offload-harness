package pipeline

// A media call's PAIR card, read the moment Run returns (2026-10-09).
//
// A session's runner opened a fresh stdio door per attempt, called offload_generate_image, got a clean
// "gpu queued" deferral and killed the door right after the reply. The call's card was still open in
// PAIR's register, and the orphan sweep later closed it "Failed: harness process exited before the job
// finished". These tests run the real Run behind a real lease queue with a real pairworkloads emitter
// and a slow stand-in for PAIR's ingress, and look at what is on the wire and in the register when Run
// returns, which is when the door answers: nothing here waits for a background goroutine.

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
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// pairRig is PAIR as the harness sees it: an ingress that records every frame (late, as a PAIR on a
// loaded box answers), the app dir that names this box, and the open-card register beside them.
type pairRig struct {
	t      *testing.T
	em     *pairworkloads.Emitter
	url    string
	appDir string
	regDir string

	mu     sync.Mutex
	frames []pairFrame
}

type pairFrame struct {
	method string
	info   map[string]any
}

func newPairRig(t *testing.T) *pairRig {
	t.Helper()
	r := &pairRig{t: t, appDir: t.TempDir(), regDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(r.appDir, "node-id.json"), []byte(`{"node_uuid":"self-uuid","created_at":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(100 * time.Millisecond) // a frame is never on the wire the instant it is sent
		var f struct {
			Method string `json:"method"`
			Params struct {
				WorkloadInfo map[string]any `json:"workloadInfo"`
			} `json:"params"`
		}
		_ = json.NewDecoder(req.Body).Decode(&f)
		r.mu.Lock()
		r.frames = append(r.frames, pairFrame{f.Method, f.Params.WorkloadInfo})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	r.em = pairworkloads.New(pairworkloads.Config{Enabled: true, Endpoint: srv.URL, AppDir: r.appDir, OpenDir: r.regDir})
	t.Cleanup(r.em.Wait) // the in-flight frames land before the server closes
	return r
}

// seen is a copy of the frames on the wire right now.
func (r *pairRig) seen() []pairFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pairFrame(nil), r.frames...)
}

// byMethod returns the workloadInfo of the one frame with method, failing when there is not exactly
// one or more than one card is in play (one call is one card).
func (r *pairRig) byMethod(method string) map[string]any {
	r.t.Helper()
	ids := map[any]bool{}
	var got []map[string]any
	for _, f := range r.seen() {
		ids[f.info["id"]] = true
		if f.method == method {
			got = append(got, f.info)
		}
	}
	if len(ids) != 1 {
		r.t.Fatalf("one call must be one card, saw ids %v", ids)
	}
	if len(got) != 1 {
		r.t.Fatalf("want exactly one %s frame on the wire when Run returns, got %d (frames: %v)", method, len(got), r.methods())
	}
	return got[0]
}

func (r *pairRig) methods() []string {
	var out []string
	for _, f := range r.seen() {
		out = append(out, f.method)
	}
	return out
}

// quiet fails when the register still holds a card: the orphan sweep reads nothing else, so a card
// in it is a card the sweep closes "harness process exited before the job finished".
func (r *pairRig) quiet() {
	r.t.Helper()
	ents, _ := os.ReadDir(r.regDir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if len(names) != 0 {
		r.t.Fatalf("the register still holds %v when Run returns: the sweep would close the card as an orphan", names)
	}
	if f := r.byMethod("workload:completed"); f["id"] == nil {
		r.t.Fatalf("no closing frame")
	}
}

// ledgerOn gives the fixture's pipeline a ledger the emitter observes, as the MCP server has.
func (r *pairRig) ledgerOn(f *admitFixture) {
	r.t.Helper()
	l, err := ledger.Open(filepath.Join(r.t.TempDir(), "ledger.jsonl"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { l.Close() })
	f.p.led = l
	r.em.AttachLedger(l)
}

// queuedFixture is a host whose only card for the call is held by another job: the call waits its
// window and answers with a place in line (or, from a door that cannot resume, a busy card).
func queuedFixture(t *testing.T, rig *pairRig, withLedger bool) *admitFixture {
	t.Helper()
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	f.p.SetCallTracker(rig.em)
	if withLedger {
		rig.ledgerOn(f)
	}
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })
	return f
}

// The PAIR card keys on core's held classes, and the pipeline spells them with its own literals: the
// two must stay one vocabulary, or a held call would close red again with every other test green.
func TestThePipelineProducesTheClassesTheCardKeysOn(t *testing.T) {
	if errClassGPUBusy != core.ErrClassGPUBusy {
		t.Fatalf("pipeline errClassGPUBusy %q != core.ErrClassGPUBusy %q", errClassGPUBusy, core.ErrClassGPUBusy)
	}
	queued := (&Pipeline{}).deferForLease(&errGPUQueued{Token: "tk-1", Position: 1, Why: "x"}, core.TaskGenerateImage, core.Meta{}, 0, time.Now())
	busy := (&Pipeline{}).deferForLease(&errGPUBusy{detail: "x"}, core.TaskGenerateImage, core.Meta{}, 0, time.Now())
	if !core.CardHeld(queued.Meta.ErrClass) || !core.CardHeld(busy.Meta.ErrClass) {
		t.Fatalf("a place in line (%q) and a busy card (%q) must both be held classes", queued.Meta.ErrClass, busy.Meta.ErrClass)
	}
	unusable := (&Pipeline{}).deferForLease(os.ErrPermission, core.TaskGenerateImage, core.Meta{}, 0, time.Now())
	if core.CardHeld(unusable.Meta.ErrClass) {
		t.Fatalf("an unusable lease location is a configuration fault, not a held card: %q", unusable.Meta.ErrClass)
	}
}

// The incident: a call that waited its window and was handed a place in line is closed quiet, with the
// reason, before its door can answer. Both ways a card closes: by the call's own ledger row, and by
// Run's close when the ledger is held by another process and the call writes no row.
func TestAQueuedMediaCallClosesItsPairCardQuietBeforeRunReturns(t *testing.T) {
	for _, withLedger := range []bool{false, true} {
		name := "no ledger row"
		if withLedger {
			name = "ledger row"
		}
		t.Run(name, func(t *testing.T) {
			rig := newPairRig(t)
			f := queuedFixture(t, rig, withLedger)
			res := f.await(f.image(nil))
			if res.Meta.ErrClass != "gpu_queued" || !res.Deferred {
				t.Fatalf("want the queued answer, got class %q: %s", res.Meta.ErrClass, res.Reason)
			}
			// RUN HAS RETURNED: THE DOOR ANSWERS NOW, AND ITS CLIENT KILLS IT.
			opened, closed := rig.byMethod("workload:submitted"), rig.byMethod("workload:completed")
			if got, _ := closed["error"].(string); !strings.HasPrefix(got, "gpu queued: ") {
				t.Fatalf("the close must carry the deferral reason, got %q", got)
			}
			if closed["startedAt"] != nil || closed["engine"] != "comfyui" || closed["id"] != opened["id"] {
				t.Fatalf("a call that never held the card closes unstarted, on the card it opened: open %v closed %v", opened, closed)
			}
			for _, m := range rig.methods() {
				if m == "workload:errored" {
					t.Fatalf("a place in line was closed as a failure: %v", rig.methods())
				}
			}
			rig.quiet()
		})
	}
}

// The same for a door that cannot resume: the busy-card answer is the other held class.
func TestABusyMediaCallClosesItsPairCardQuietBeforeRunReturns(t *testing.T) {
	rig := newPairRig(t)
	f := queuedFixture(t, rig, true)
	res := f.await(f.plainImage())
	busyNotQueued(t, res)
	closed := rig.byMethod("workload:completed")
	if got, _ := closed["error"].(string); !strings.HasPrefix(got, "gpu busy: ") || closed["startedAt"] != nil {
		t.Fatalf("a busy card closes quiet and unstarted with its reason: %v", closed)
	}
	rig.quiet()
}

// A render that ran is a completed card with its start and no error.
func TestASuccessfulMediaCallClosesItsPairCardCompleted(t *testing.T) {
	for _, withLedger := range []bool{false, true} {
		rig := newPairRig(t)
		f := newAdmitFixture(t, nil)
		f.p.SetCallTracker(rig.em)
		if withLedger {
			rig.ledgerOn(f)
		}
		f.letRunnersGo()
		if res := f.await(f.image(nil)); !res.OK {
			t.Fatalf("the stub render failed: %s", res.Reason)
		}
		closed := rig.byMethod("workload:completed")
		if closed["error"] != nil || closed["startedAt"] == nil {
			t.Fatalf("(ledger=%v) a render that ran closes completed, started, with no error: %v", withLedger, closed)
		}
		rig.quiet()
	}
}

// A render that broke is a failed card with the real error, not a quiet one.
func TestAFailedMediaCallClosesItsPairCardFailedWithTheError(t *testing.T) {
	for _, withLedger := range []bool{false, true} {
		rig := newPairRig(t)
		f := newAdmitFixture(t, nil)
		f.p.SetCallTracker(rig.em)
		if withLedger {
			rig.ledgerOn(f)
		}
		fail := filepath.Join(f.dir, "fail.mjs")
		if err := os.WriteFile(fail, []byte("console.error('boom: the render crashed'); process.exit(3);\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.p.cfg.ImageGenScript = fail
		res := f.await(f.image(nil))
		if res.OK || !strings.HasPrefix(res.Reason, "image generation failed") {
			t.Fatalf("want a failed render, got ok=%v: %s", res.OK, res.Reason)
		}
		closed := rig.byMethod("workload:errored")
		if got, _ := closed["error"].(string); !strings.HasPrefix(got, "image generation failed") {
			t.Fatalf("(ledger=%v) the failure must carry the error: %v", withLedger, closed)
		}
		for _, m := range rig.methods() {
			if m == "workload:completed" {
				t.Fatalf("a render that broke closed completed: %v", rig.methods())
			}
		}
		ents, _ := os.ReadDir(rig.regDir)
		if len(ents) != 0 {
			t.Fatalf("(ledger=%v) the register still holds %d card(s) when Run returns", withLedger, len(ents))
		}
	}
}

// An early return (a request refused before any lane work) closes its card too, failed, with its reason.
func TestAnEarlyReturnClosesItsPairCardFailed(t *testing.T) {
	rig := newPairRig(t)
	f := newAdmitFixture(t, nil)
	f.p.SetCallTracker(rig.em)
	res := f.p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "  ", Params: map[string]any{"out": filepath.Join(f.dir, "o.png")}})
	if !res.Deferred || res.Reason != "empty image prompt" {
		t.Fatalf("want the early refusal, got %+v", res)
	}
	closed := rig.byMethod("workload:errored")
	if closed["error"] != "empty image prompt" {
		t.Fatalf("an early return closes failed with its reason: %v", closed)
	}
	if ents, _ := os.ReadDir(rig.regDir); len(ents) != 0 {
		t.Fatalf("the register still holds %d card(s) when Run returns", len(ents))
	}
}

// A panic in a lane closes the card failed, delivered, before the panic goes on: the door dies of it,
// and a close left to a goroutine would die with the process.
func TestAPanickingMediaCallClosesItsPairCardBeforeThePanicGoesOn(t *testing.T) {
	rig := newPairRig(t)
	f := newAdmitFixture(t, nil)
	f.p.SetCallTracker(rig.em)
	f.p.alloc.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { panic("card table exploded") }
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		f.p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a calm ocean", Params: map[string]any{"out": filepath.Join(f.dir, "o.png")}})
	}()
	if recovered == nil {
		t.Fatal("the lane's panic must go on past the close")
	}
	closed := rig.byMethod("workload:errored")
	if got, _ := closed["error"].(string); !strings.HasPrefix(got, "panic: card table exploded") {
		t.Fatalf("a panic closes the card failed with the panic: %v", closed)
	}
	if ents, _ := os.ReadDir(rig.regDir); len(ents) != 0 {
		t.Fatalf("the register still holds %d card(s) when the panic reaches the top", len(ents))
	}
}
