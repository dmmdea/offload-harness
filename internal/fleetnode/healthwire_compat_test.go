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

// TestLeasesWireIsReadByEveryHealthReader is the wire proof for GPU routing P7 (per-card lease
// truth). One node, one REAL health handler, two live leases on different cards of a three-card
// flagship box (a long media render on card C, a short reservation on card A), read by every
// reader that ships:
//
//   - the raw JSON: leases[] has one entry per lease with its cards, and the SINGULAR lease block
//     and lease_exclusive are the worst across them, which is everything a delegator one
//     release behind reads - so that delegator is never told less than is true;
//   - the delegator's decoder (delegate.FetchNodeView) and the real gate (delegate.Place): a
//     contract whose seats are not all on a leased card stays routable to the node, where the
//     whole-node reading would have refused it;
//   - the fleet deploy's decoder (nodeswap's real ReadHealth), undisturbed by the new keys.
func TestLeasesWireIsReadByEveryHealthReader(t *testing.T) {
	cfg := config.FlagshipFixture()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "agent-pool"
	cfg.AgentCtxTokens = 262144
	cfg.FleetAuthToken = "wire-token"
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })

	devs := []fleetnode.GPUDevice{
		{Index: 0, UUID: "GPU-1111AAAA-2222-3333-4444-555566667777", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 15, UtilPct: 0, UtilKnown: true},
		{Index: 1, UUID: "GPU-2222BBBB-3333-4444-5555-666677778888", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 15, UtilPct: 0, UtilKnown: true},
		{Index: 2, UUID: "GPU-3333CCCC-4444-5555-6666-777788889999", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 2, UtilPct: 97, UtilKnown: true},
	}
	cardA, cardC := "gpu-1111aaaa-2222-3333-4444-555566667777", "gpu-3333cccc-4444-5555-6666-777788889999"
	media := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 7, Epochs: []uint64{7}, PID: 4242,
		ExpiresAt: time.Now().Add(6 * time.Hour), Devices: []string{cardC}}
	text := gpulease.Info{Held: true, Class: gpulease.ClassText, Epoch: 9, Epochs: []uint64{9}, PID: 4243,
		ExpiresAt: time.Now().Add(20 * time.Second), Devices: []string{cardA}, Exclusive: true}
	both := media
	both.Leases = []gpulease.Info{media, text}
	both.Epochs = []uint64{7, 9}

	roster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"agent-pool","object":"model"},{"id":"gemma-4-26b-agent","object":"model"}]}`))
	}))
	defer roster.Close()
	cfg.Endpoint = roster.URL

	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID:  "lease-node",
		Version: "test",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 15, Devices: devs, At: time.Now()}, true
		},
		Lease:            func() gpulease.Info { return both },
		LoopbackListener: true,
		Cfg:              cfg,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// The roster probe is a background refresh: one health read to start it, then wait for it.
	for i := 0; i < 200; i++ {
		resp, err := http.Get(ts.URL + "/fleet/health")
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Resident bool `json:"agent_seat_resident"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&probe)
		resp.Body.Close()
		if probe.Resident {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 1) The raw JSON.
	resp, err := http.Get(ts.URL + "/fleet/health")
	if err != nil {
		t.Fatalf("health GET: %v", err)
	}
	defer resp.Body.Close()
	var raw struct {
		Lease     map[string]any   `json:"lease"`
		Leases    []map[string]any `json:"leases"`
		Exclusive bool             `json:"lease_exclusive"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("health not JSON: %v", err)
	}
	if len(raw.Leases) != 2 || raw.Leases[0]["epoch"] != float64(7) || raw.Leases[1]["epoch"] != float64(9) {
		t.Fatalf("leases = %v, want both live leases, lowest epoch first", raw.Leases)
	}
	if raw.Lease["class"] != "text" || raw.Lease["busy"] != true || raw.Lease["held"] != true || !raw.Exclusive {
		t.Fatalf("singular lease block = %v exclusive=%v: a reader one release behind must see the worst of the two (a text, busy, exclusive hold), not the lowest epoch's short media render", raw.Lease, raw.Exclusive)
	}

	// 2) The delegator's decoder and the real gate.
	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's health decoder rejected the payload: %v", err)
	}
	if len(view.Leases) != 2 || len(view.Leases[0].Devices) != 1 || view.Leases[0].Devices[0] != cardC || !view.Leases[1].Exclusive {
		t.Fatalf("delegator decoded the leases as %+v", view.Leases)
	}
	if !view.LeaseExclusive || !view.LeasedText {
		t.Fatalf("the singular fields still decode (exclusive=%v text=%v)", view.LeaseExclusive, view.LeasedText)
	}
	if len(view.Layers) == 0 {
		t.Fatal("the node published no layer rows: the per-card fence has nothing to read the seats from")
	}
	// The node resolves each seat's pin against its own card table and publishes the lease ids.
	seatIDs := 0
	for _, row := range view.Layers {
		if row.Name != "triple" {
			continue
		}
		for _, s := range row.Seats {
			if s.Role == "agent" {
				seatIDs = len(s.DeviceIDs)
			}
		}
	}
	if seatIDs != 3 {
		t.Fatalf("the three-card agent seat published %d device ids, want its three cards resolved on the node", seatIDs)
	}
	local := delegate.NodeView{NodeID: "local-box", Local: true}
	st := delegate.Subtask{Contract: core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          "summarize the docs",
		OutputSchema:  json.RawMessage(`{"type":"object"}`),
	}, EstTokens: 1000}
	// The default chain is the three-card seat, then the single layer's card-0 seat. The
	// exclusive reservation holds card A, which holds BOTH, so the node is fenced for this
	// contract, exactly as the whole-node reading would have it.
	if got := delegate.Place("seed", st, local, []delegate.NodeView{view}, true); !got.Local {
		t.Fatalf("Place chose %q: an exclusive reservation on card 0 holds every seat of the default chain", got.NodeID)
	}
	// With the reservation gone, the media render on card C holds only the three-card seat; the
	// single layer's card-0 seat is free, so the node stays routable.
	view.Leases = view.Leases[:1]
	view.LeaseExclusive = false
	if got := delegate.Place("seed", st, local, []delegate.NodeView{view}, true); got.NodeID != "lease-node" {
		t.Fatalf("Place chose %q: a long render on card 2 leaves the single layer's card-0 seat free, so the node must stay a target", got.NodeID)
	}
	// And the very same view read the way a node one release behind publishes it (no leases[],
	// only the singular block) is fenced as a whole node: the old reading is untouched.
	view.Leases = nil
	view.LeaseBusy, view.LeasedText = true, false
	if got := delegate.Place("seed", st, local, []delegate.NodeView{view}, true); !got.Local {
		t.Fatalf("Place chose %q: without leases[] a busy non-text lease still fences the whole node", got.NodeID)
	}

	// 3) The deploy's decoder, the real one its wait-idle step calls.
	info, err := nodeswap.DefaultDeps().ReadHealth(context.Background(), ts.URL+"/fleet/health")
	if err != nil {
		t.Fatalf("the deploy's health decoder rejected the payload: %v", err)
	}
	if !info.OK || info.NodeID != "lease-node" {
		t.Fatalf("deploy read %+v: the leases[] keys must not disturb it", info)
	}
}

// TestHealthPayloadOfAnOlderNodeDecodesWithNoLeases is the other direction: a node at 0.160.0 or
// 0.161.0 publishes only the singular lease block. The delegator's decoder must read it with
// Leases nil and keep fencing it as a whole node, and a payload from the future carrying keys
// nobody has seen must still decode.
func TestHealthPayloadOfAnOlderNodeDecodesWithNoLeases(t *testing.T) {
	older := `{"node_id":"old-node","queue_depth":0,"agent_enabled":true,"agent_seat":"offload-e4b","agent_seat_resident":true,"agent_ctx_tokens":8192,` +
		`"lease":{"held":true,"class":"media","pid":99,"until":"2099-01-01T00:00:00Z","remaining_sec":21600,"busy":true},` +
		`"gpu_devices":[{"index":0,"uuid":"GPU-1111AAAA","name":"x","vram_total_gb":16,"vram_free_gb":15,"util_pct":0,"util_known":true}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(older))
	}))
	defer srv.Close()
	view, err := delegate.FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("an older node's health was rejected: %v", err)
	}
	if view.Leases != nil || !view.LeaseBusy {
		t.Fatalf("Leases = %v LeaseBusy = %v, want nil and busy: the singular block is the whole reading", view.Leases, view.LeaseBusy)
	}
	st := delegate.Subtask{Contract: core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          "summarize the docs",
		OutputSchema:  json.RawMessage(`{"type":"object"}`),
	}, EstTokens: 1000}
	local := delegate.NodeView{NodeID: "local-box", Local: true}
	if got := delegate.Place("seed", st, local, []delegate.NodeView{view}, true); !got.Local {
		t.Fatalf("Place chose %q: an older node under a long media lease is fenced as a whole node, as it always was", got.NodeID)
	}

	future := `{"node_id":"future-node","leases":[{"epoch":1,"class":"media","a_key_from_2027":{"x":[1]}}],"another_key":true}`
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(future))
	}))
	defer fs.Close()
	fv, err := delegate.FetchNodeView(context.Background(), fs.URL, "")
	if err != nil {
		t.Fatalf("a lease entry carrying a key nobody has seen broke the decoder: %v", err)
	}
	if len(fv.Leases) != 1 || fv.Leases[0].Epoch != 1 {
		t.Fatalf("future payload decoded as %+v", fv.Leases)
	}
}
