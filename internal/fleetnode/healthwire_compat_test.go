// This is an EXTERNAL test package (fleetnode_test, not fleetnode) for one
// reason: it must import internal/delegate to run the delegator's REAL health
// decoder against this node's REAL health handler. Nothing in internal/delegate
// is modified or exercised for its own sake here — the subject under test is
// the fleet node's wire, and delegate.FetchNodeView is the production reader
// that has to keep decoding it.
//
// Re-declaring healthWire's fields inside this package would be the exact
// "seam test that bypasses the logic it certifies" failure: a copy of the
// struct proves a copy still decodes. Only the shipped decoder can prove the
// shipped decoder still decodes.
package fleetnode_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/nodeswap"
)

// nopRunner satisfies fleetnode.Runner without doing anything: this file never
// dispatches, it only reads health.
type nopRunner struct{}

func (nopRunner) Run(ctx context.Context, req core.Request) core.Result {
	return core.Result{OK: true, Data: json.RawMessage(`{}`)}
}

// TestHealthWireStaysDecodableByTheDelegator is the wire-compatibility proof
// for the 0.100.0 capacity fields. internal/delegate/nodeview.go decodes health
// into a FIXED struct listing only the six fields placement consumes; the four
// fields added here are not among them. Plain encoding/json ignores unknown
// keys (no DisallowUnknownFields on that path), but "ignores unknown keys" is a
// property of how the decoder is CONFIGURED, and configuration changes — so it
// is pinned here rather than assumed.
//
// The assertion that matters is queue_depth: the delegator's placement
// tie-break (gate.go, `r.QueueDepth < best.QueueDepth`) reads it, and it must
// still arrive with its ORIGINAL meaning — accepted + running — not the running
// count and not the backlog count.
func TestHealthWireStaysDecodableByTheDelegator(t *testing.T) {
	cfg := config.Config{
		ImageGenScript:         "C:/x/comfy-generate.mjs",
		FleetMaxQueueDepth:     7,
		FleetMaxConcurrentJobs: 2,
	}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })

	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID:  "wire-node",
		Version: "test",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Cfg: cfg,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Put the node in a state where the three counters are all different, so a
	// decoder reading the wrong key cannot accidentally look right: with two
	// execution slots, five admitted jobs are 2 running + 3 queued = depth 5.
	release := make(chan struct{})
	defer close(release)
	block := func(ctx context.Context) (json.RawMessage, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`1`), nil
	}
	for _, id := range []string{"w1", "w2", "w3", "w4", "w5"} {
		if !jobs.Accept(id, block) {
			t.Fatalf("Accept(%s) must admit", id)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if q, r := jobs.Counts(); q == 3 && r == 2 {
			break
		}
		if time.Now().After(deadline) {
			q, r := jobs.Counts()
			t.Fatalf("node never settled at 3 queued / 2 running (got %d/%d)", q, r)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 1) The production decoder still decodes, and queue_depth still means
	//    accepted+running.
	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's health decoder rejected a 0.100.0 payload: %v", err)
	}
	if view.NodeID != "wire-node" {
		t.Fatalf("node_id decoded as %q", view.NodeID)
	}
	if view.QueueDepth != 5 {
		t.Fatalf("QueueDepth decoded as %d, want 5 (2 running + 3 queued) — queue_depth changed meaning on the wire", view.QueueDepth)
	}
	// The capacity triad decodes too (offload_status publishes it, and an
	// operator sent here by a `queue deadline` failure needs it to be right).
	// All three values differ, so a decoder wired to the wrong key is caught.
	if view.JobsRunning != 2 || view.JobsQueued != 3 || view.MaxConcurrentJobs != 2 {
		t.Fatalf("capacity decoded as running=%d queued=%d max=%d, want 2/3/2",
			view.JobsRunning, view.JobsQueued, view.MaxConcurrentJobs)
	}

	// 2) The new fields really are ON that payload (otherwise (1) proves
	//    nothing about tolerating them).
	resp, err := http.Get(ts.URL + "/fleet/health")
	if err != nil {
		t.Fatalf("health GET: %v", err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("health not JSON: %v", err)
	}
	for field, want := range map[string]float64{
		"queue_depth":         5,
		"jobs_running":        2,
		"jobs_queued":         3,
		"max_concurrent_jobs": 2,
		"max_queue_depth":     7,
	} {
		if raw[field] != want {
			t.Fatalf("health %s = %v, want %v", field, raw[field], want)
		}
	}

	// 3) Belt and braces for a FUTURE additive field: a payload carrying a key
	//    no released decoder has ever seen must still decode. This is the
	//    property the four fields above rely on, asserted directly.
	future := `{"node_id":"future-node","queue_depth":9,"a_field_from_2027":{"nested":[1,2,3]}}`
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(future))
	}))
	defer fs.Close()
	fv, err := delegate.FetchNodeView(context.Background(), fs.URL, "")
	if err != nil {
		t.Fatalf("an unknown health field broke the delegator's decode: %v", err)
	}
	if fv.NodeID != "future-node" || fv.QueueDepth != 9 {
		t.Fatalf("future payload decoded as %+v", fv)
	}
	// A pre-0.100.0 node publishes no capacity fields at all; they must decode
	// to zero rather than fail, and a consumer reads 0 as "no usable number".
	if fv.JobsRunning != 0 || fv.JobsQueued != 0 || fv.MaxConcurrentJobs != 0 {
		t.Fatalf("absent capacity fields decoded as %d/%d/%d, want zeros",
			fv.JobsRunning, fv.JobsQueued, fv.MaxConcurrentJobs)
	}
	if !strings.Contains(future, "a_field_from_2027") {
		t.Fatal("test fixture lost its unknown field")
	}
}

// TestJobWaitCapStaysBelowTheDelegatorsPollRequestTimeout is the ORDERING
// constraint the long poll lives or dies by (register S-19).
//
// `GET /fleet/jobs/{id}?wait=` holds a connection open server-side. The
// delegator bounds ONE poll exchange client-side with pollRequestTimeout, so a
// server-side cap at or above it means every long poll is cancelled by the
// caller a moment before the node answers — the node would do the work of
// waiting and the delegator would file the result as a transport failure. The
// cap must therefore stay strictly below it, with room for the answer to be
// written.
//
// The delegator's constant is UNEXPORTED and internal/delegate cannot be
// imported for it from the package under test either way (delegate imports
// fleetnode). Re-declaring the number here would pin a copy against a copy —
// the exact "seam test that bypasses the logic it certifies" failure — so this
// reads the shipped source and fails the moment someone lowers the real one.
func TestJobWaitCapStaysBelowTheDelegatorsPollRequestTimeout(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "delegate", "run.go"))
	if err != nil {
		t.Fatalf("reading the delegator's poll bound: %v", err)
	}
	m := regexp.MustCompile(`pollRequestTimeout\s*=\s*(\d+)\s*\*\s*time\.Second`).FindSubmatch(src)
	if m == nil {
		t.Fatal("pollRequestTimeout is no longer declared as `N * time.Second` in internal/delegate/run.go — re-derive this bound by hand before changing the cap")
	}
	sec, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("pollRequestTimeout value %q: %v", m[1], err)
	}
	poll := time.Duration(sec) * time.Second
	held := fleetnode.MaxJobWaitSec*time.Second + 2*time.Second // the cap plus the handler's write slack
	if held >= poll {
		t.Fatalf("a capped long poll holds the connection for up to %s while the delegator abandons one exchange after %s: every wait= poll would be cancelled client-side (MaxJobWaitSec = %d)",
			held, poll, fleetnode.MaxJobWaitSec)
	}
}

// TestOverdueLeaseWireIsReadByEveryHealthReader is the wire proof for GPU
// routing P1 (the overdue lease verdict and the per-card rows the delegator now
// decodes). One node, one REAL health handler, a held lease past its declared
// window and a 3-card snapshot, read by every reader that ships:
//
//   - the raw JSON, field by field: lease.busy is true (the clamp to zero no
//     longer reads it free), lease.overdue is true, remaining_sec is absent
//     (omitempty), gpu_devices[] has all three rows;
//   - the delegator's decoder (delegate.FetchNodeView): the overdue lease is
//     LeaseOverdue and NOT LeaseBusy, so the gate ranks the node last instead of
//     hard-excluding it, and the cards arrive as Devices;
//   - the fleet deploy's decoder (nodeswap's real ReadHealth, the one its
//     idle-wait runs): it still reads node id and the running/queued counts
//     exactly, and the new lease and device fields do not disturb it.
//
// The deploy does not read lease.busy at all: its idle-wait takes job counts
// from health and the lease from the lease directory (gpulease Inspect), which
// the last assertion pins at the source. So the Busy change cannot move a
// deploy; if that ever changes, the new reader has to decide what an overdue
// lease means for it, and this test is where it learns that.
func TestOverdueLeaseWireIsReadByEveryHealthReader(t *testing.T) {
	cfg := config.Config{FleetMaxConcurrentJobs: 1, FleetMaxQueueDepth: 4}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })

	devs := []fleetnode.GPUDevice{
		{Index: 0, UUID: "GPU-1111aaaa-2222-3333-4444-555566667777", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 2, UtilPct: 96, UtilKnown: true},
		{Index: 1, UUID: "GPU-2222bbbb-3333-4444-5555-666677778888", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 15, UtilPct: 1, UtilKnown: true},
		{Index: 2, UUID: "GPU-3333cccc-4444-5555-6666-777788889999", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 15, UtilPct: 0, UtilKnown: true, DisplayActive: true},
	}
	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID:  "overdue-node",
		Version: "test",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 15, Devices: devs, At: time.Now()}, true
		},
		Lease: func() gpulease.Info {
			return gpulease.Info{Held: true, Class: gpulease.ClassMedia, PID: 4242, ExpiresAt: time.Now().Add(-2 * time.Hour)}
		},
		Cfg: cfg,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// One running, one queued: counts the deploy reads and the lease must not move.
	release := make(chan struct{})
	defer close(release)
	block := func(ctx context.Context) (json.RawMessage, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`1`), nil
	}
	for _, id := range []string{"o1", "o2"} {
		if !jobs.Accept(id, block) {
			t.Fatalf("Accept(%s) must admit", id)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if q, r := jobs.Counts(); q == 1 && r == 1 {
			break
		}
		if time.Now().After(deadline) {
			q, r := jobs.Counts()
			t.Fatalf("node never settled at 1 queued / 1 running (got %d/%d)", q, r)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 1) The raw JSON.
	resp, err := http.Get(ts.URL + "/fleet/health")
	if err != nil {
		t.Fatalf("health GET: %v", err)
	}
	defer resp.Body.Close()
	var raw struct {
		Lease      map[string]any `json:"lease"`
		GpuDevices []any          `json:"gpu_devices"`
		Running    int            `json:"jobs_running"`
		Queued     int            `json:"jobs_queued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("health not JSON: %v", err)
	}
	if raw.Lease["held"] != true || raw.Lease["busy"] != true || raw.Lease["overdue"] != true || raw.Lease["class"] != "media" {
		t.Fatalf("lease block = %v, want held media, busy and overdue", raw.Lease)
	}
	if _, present := raw.Lease["remaining_sec"]; present {
		t.Fatalf("remaining_sec present on an overdue lease: %v", raw.Lease)
	}
	if len(raw.GpuDevices) != 3 {
		t.Fatalf("gpu_devices = %v, want all three rows", raw.GpuDevices)
	}

	// 2) The delegator.
	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's health decoder rejected the payload: %v", err)
	}
	if !view.LeaseOverdue || view.LeaseBusy {
		t.Fatalf("delegator read the overdue lease as overdue=%v busy=%v, want overdue and NOT busy (busy would hard-exclude a media-lease node)", view.LeaseOverdue, view.LeaseBusy)
	}
	if len(view.Devices) != 3 || view.Devices[1].UUID != "GPU-2222bbbb-3333-4444-5555-666677778888" || !view.Devices[2].DisplayActive {
		t.Fatalf("delegator decoded the cards as %+v", view.Devices)
	}
	if free, total, known := view.FreeCards(); !known || free != 1 || total != 3 {
		t.Fatalf("FreeCards() = %d of %d (known %v), want 1 of 3: one busy, one display", free, total, known)
	}
	// An overdue lease is ranked last, never refused: the node's refusing flag
	// does not read the overdue busy (a media lease does not turn dispatch away),
	// so saturation.high stays false and the capacity wait still asks the node.
	// The only thing keeping idle_slot false here is the job this test queued
	// (1 running of 1). The delegator must still place on it when it is the only
	// remote. This runs the real wire through the real gate.
	if !view.SaturationKnown || view.SaturationHigh || view.IdleSlot {
		t.Fatalf("saturation decoded as known=%v high=%v idle_slot=%v, want known, not high (the overdue busy is not a refusal) and no idle slot (the node is running its one job)", view.SaturationKnown, view.SaturationHigh, view.IdleSlot)
	}
	view.AgentEnabled, view.AgentResident, view.AgentSeat, view.AgentCtxTokens = true, true, "offload-e4b", 8192
	st := delegate.Subtask{Contract: core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          "summarize the docs",
		OutputSchema:  json.RawMessage(`{"type":"object"}`),
	}, EstTokens: 1000}
	local := delegate.NodeView{NodeID: "local-box", Local: true}
	if got := delegate.Place("seed", st, local, []delegate.NodeView{view}, true); got.NodeID != "overdue-node" {
		t.Fatalf("Place chose %q with the overdue node as the only remote and the local box busy: it must take the work (the node queues it), not fall back to local", got.NodeID)
	}

	// 3) The deploy's decoder, the real one its wait-idle step calls.
	info, err := nodeswap.DefaultDeps().ReadHealth(context.Background(), ts.URL+"/fleet/health")
	if err != nil {
		t.Fatalf("the deploy's health decoder rejected the payload: %v", err)
	}
	if !info.OK || info.NodeID != "overdue-node" || info.RunningJobs != 1 || info.QueuedJobs != 1 {
		t.Fatalf("deploy read %+v, want ok, node overdue-node, 1 running, 1 queued: the lease and device fields must not disturb it", info)
	}

	// 4) The deploy takes its lease from the lease directory, never from health.
	src, err := os.ReadFile(filepath.Join("..", "nodeswap", "deps.go"))
	if err != nil {
		t.Fatalf("reading the deploy's health decoder: %v", err)
	}
	decl := regexp.MustCompile(`(?s)type fleetHealthResponse struct \{.*?\r?\n\}`).Find(src)
	if decl == nil {
		t.Fatal("fleetHealthResponse is no longer declared in internal/nodeswap/deps.go - re-derive what the deploy reads from health before changing the lease verdict")
	}
	if strings.Contains(string(decl), "lease") || strings.Contains(string(decl), "busy") {
		t.Fatalf("the deploy now decodes a lease or busy field from health:\n%s\nit must decide what an OVERDUE lease means for the idle-wait (lease.overdue, GPU routing P1)", decl)
	}
	if !strings.Contains(string(src), "m.Inspect()") {
		t.Fatal("the deploy's lease read is no longer gpulease Inspect() on the lease directory; re-check what it does with an overdue lease")
	}
}
