package fleetnode

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

const (
	wuCard0 = "GPU-3ee161b5-c188-495b-eaeb-291e6e6e1d97"
	wuCard1 = "GPU-2a44210f-6739-2d89-0e21-44cd5143faf7" // the display card
	wuCard2 = "GPU-0c3843d3-5721-d9f7-47fe-89fdb8373e24"
)

// qubeDevices is the Qube while its operator plays a game: 33% on the display
// card, 0% on both cards the harness can use.
func qubeDevices() []GPUDevice {
	return []GPUDevice{
		{Index: 0, UUID: wuCard0, Name: "RTX 5060 Ti", TotalGiB: 15.9, FreeGiB: 15.4, UtilPct: 0, UtilKnown: true},
		{Index: 1, UUID: wuCard1, Name: "RTX 5070 Ti", TotalGiB: 15.9, FreeGiB: 2.2, UtilPct: 33, UtilKnown: true},
		{Index: 2, UUID: wuCard2, Name: "RTX 5060 Ti", TotalGiB: 15.9, FreeGiB: 15.9, UtilPct: 0, UtilKnown: true},
	}
}

// qubeDesktop is the Qube's real --query-compute-apps shape: graphics rows,
// all on the display card, with no memory figure.
func qubeDesktop() []gpuprobe.ComputeApp {
	return []gpuprobe.ComputeApp{{GPUUUID: wuCard1}, {GPUUUID: wuCard1}, {GPUUUID: wuCard1}}
}

// TestSamplerRefreshesTheDisplaySetSlowlyAndCarriesItForward: which card drives
// the desktop is hardware plus a login session, so the probe runs on tick 0 and
// every displayEvery ticks after — not on every 2 s health tick — and a failed
// probe keeps the previous set rather than suddenly re-counting the desktop.
func TestSamplerRefreshesTheDisplaySetSlowlyAndCarriesItForward(t *testing.T) {
	calls := 0
	fail := false
	s := &Sampler{displayProbe: func() ([]gpuprobe.ComputeApp, error) {
		calls++
		if fail {
			return nil, errors.New("nvidia-smi hiccup")
		}
		return qubeDesktop(), nil
	}}
	probe := func() ([]GPUDevice, error) { return qubeDevices(), nil }

	for i := 0; i < displayEvery+1; i++ {
		s.sampleDevices(probe)
	}
	if calls != 2 {
		t.Fatalf("display probe ran %d times over %d ticks, want 2 (tick 0 and tick %d)", calls, displayEvery+1, displayEvery)
	}
	snap, ok := s.Load()
	if !ok || !snap.DisplayUUIDs[wuCard1] || snap.DisplayUUIDs[wuCard0] || snap.DisplayUUIDs[wuCard2] {
		t.Fatalf("snapshot display set = %v, want only card 1", snap.DisplayUUIDs)
	}

	fail = true
	for i := 0; i < displayEvery; i++ {
		s.sampleDevices(probe)
	}
	snap, _ = s.Load()
	if !snap.DisplayUUIDs[wuCard1] {
		t.Fatal("a failed display probe must keep the previous set, not re-count the desktop as harness work")
	}
}

// TestHealthWorkUtilSkipsTheDisplayCardAndGpuUtilKeepsItsContract: gpu_util_pct
// stays the busiest card on the box (its documented contract — the dashboard
// and the PAIR rule want exactly that), and work_util_pct is the busiest card
// the harness can run a seat on.
func TestHealthWorkUtilSkipsTheDisplayCardAndGpuUtilKeepsItsContract(t *testing.T) {
	display := gpuprobe.DisplayCardUUIDs([]string{wuCard0, wuCard1, wuCard2}, qubeDesktop())
	opts := &Options{
		NodeID: "qube",
		Snapshot: func() (Snapshot, bool) {
			return Snapshot{TotalGiB: 47.7, FreeGiB: 33.5, Devices: qubeDevices(), DisplayUUIDs: display, At: time.Now()}, true
		},
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var h struct {
		GpuUtilPct    int  `json:"gpu_util_pct"`
		GpuUtilKnown  bool `json:"gpu_util_known"`
		WorkUtilPct   int  `json:"work_util_pct"`
		WorkUtilKnown bool `json:"work_util_known"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if !h.GpuUtilKnown || h.GpuUtilPct != 33 {
		t.Fatalf("gpu_util_pct = %d (known %v), want 33 — its contract is the busiest card on the box", h.GpuUtilPct, h.GpuUtilKnown)
	}
	if !h.WorkUtilKnown || h.WorkUtilPct != 0 {
		t.Fatalf("work_util_pct = %d (known %v), want 0 — the game is on the display card, and every harness card is idle", h.WorkUtilPct, h.WorkUtilKnown)
	}
}

// TestHealthWorkUtilOnASingleGPUBoxCountsItsOnlyCard: a laptop runs its seats
// on its display card by necessity, so nothing is excluded and the two figures
// agree.
func TestHealthWorkUtilOnASingleGPUBoxCountsItsOnlyCard(t *testing.T) {
	only := []GPUDevice{{Index: 0, UUID: wuCard1, Name: "RTX 3070 Laptop", TotalGiB: 8, FreeGiB: 2, UtilPct: 90, UtilKnown: true}}
	display := gpuprobe.DisplayCardUUIDs([]string{wuCard1}, qubeDesktop())
	opts := &Options{NodeID: "aorus", Snapshot: func() (Snapshot, bool) {
		return Snapshot{TotalGiB: 8, FreeGiB: 2, Devices: only, DisplayUUIDs: display, At: time.Now()}, true
	}}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	// Assert the status first: a snapshot the handler judges stale answers 503,
	// and a test that skips this reads zeros and blames the code under test.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var h struct {
		GpuUtilPct  int  `json:"gpu_util_pct"`
		WorkUtilPct int  `json:"work_util_pct"`
		WorkKnown   bool `json:"work_util_known"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	if !h.WorkKnown || h.WorkUtilPct != 90 || h.GpuUtilPct != 90 {
		t.Fatalf("single-GPU box: gpu %d / work %d (known %v), want 90 / 90", h.GpuUtilPct, h.WorkUtilPct, h.WorkKnown)
	}
}
