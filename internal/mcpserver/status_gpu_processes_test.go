package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// offload_status {section:"gpu_lease"} listed ~30 Windows desktop processes of the display card,
// every one used_known=false (WDDM sizes no process), and they buried the single process that
// mattered, a python on a work card (F18, 2026-10-09). The lease block now carries the desktop as
// one count per display card; every other process is listed as before. This runs the real
// block (Snapshot, Map, the MCP transport) over a fake 3-card box and its process table.

const (
	procWork0   = "GPU-1111aaaa-2222-3333-4444-555566667777"
	procDisplay = "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000"
	procWork2   = "GPU-3333cccc-0000-0000-0000-000000000000"
)

func fakeThreeCardSampler(displayActive bool) func(context.Context) ([]gpuactivity.GPU, error) {
	return func(context.Context) ([]gpuactivity.GPU, error) {
		return []gpuactivity.GPU{
			{Index: 0, UUID: procWork0, Name: "RTX A", UtilPct: 97, UtilKnown: true, MemUsedMiB: 15000, MemTotalMiB: 16311},
			{Index: 1, UUID: procDisplay, Name: "RTX B", UtilPct: 5, UtilKnown: true, MemUsedMiB: 4000, MemTotalMiB: 16303, DisplayActive: displayActive},
			{Index: 2, UUID: procWork2, Name: "RTX C", UtilPct: 0, UtilKnown: true, MemUsedMiB: 300, MemTotalMiB: 16311},
		}, nil
	}
}

// fakeProcessTable is what nvidia-smi lists on the reference box: 31 unsized desktop rows on the
// display card, the unsized python that holds work card 0, and one sized process on card 2.
func fakeProcessTable(context.Context) ([]gpuactivity.GPUProcess, error) {
	apps := []string{"explorer.exe", "chrome.exe", "msedgewebview2.exe", "slack.exe"}
	var rows []gpuactivity.GPUProcess
	for i := 0; i < 31; i++ {
		rows = append(rows, gpuactivity.GPUProcess{PID: 20000 + i, Name: `C:\Program Files\desk\` + apps[i%len(apps)], GPUUUID: procDisplay})
	}
	rows = append(rows,
		gpuactivity.GPUProcess{PID: 900, Name: `C:\Python\python.exe`, GPUUUID: procWork0},
		gpuactivity.GPUProcess{PID: 901, Name: "/opt/llama/llama-server", UsedMiB: 9000, UsedKnown: true, GPUUUID: procWork2})
	return rows, nil
}

func TestOffloadStatusLeaseBlockFoldsADisplayCardsDesktop(t *testing.T) {
	s, cfg, _ := statusFixture(t)
	statusSamplesGPU = true // statusFixture's cleanup puts it back
	statusGPUProcSampler = fakeProcessTable
	t.Cleanup(func() { statusGPUSampler, statusGPUProcSampler = nil, nil })

	m, err := gpulease.OpenAt("", cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "kv bench", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	lease := func(section string) (map[string]any, string) {
		raw := statusCall(t, s, json.RawMessage(`{"section":"`+section+`"}`))
		block, _ := decodeObject(t, raw)["gpu_lease"].(map[string]any)
		act, _ := block["activity"].(map[string]any)
		if act == nil {
			t.Fatalf("section %s: no gpu_lease.activity in %.300s", section, raw)
		}
		return act, raw
	}

	// The display card is lit: its 31 rows fold, in the focused section and in the full answer
	// (which builds the same block), and the two real processes stay.
	statusGPUSampler = fakeThreeCardSampler(true)
	for _, section := range []string{"gpu_lease", "all"} {
		act, raw := lease(section)
		rows, _ := act["gpu_processes"].([]any)
		var pids []string
		for _, r := range rows {
			pids = append(pids, fmt.Sprint(r.(map[string]any)["pid"]))
		}
		if got := strings.Join(pids, ","); got != "900,901" {
			t.Errorf("section %s: listed processes = [%s], want [900,901] (the holder on card 0 and the sized process on card 2)", section, got)
		}
		sum, _ := act["display_card_processes_unknown"].([]any)
		if len(sum) != 1 {
			t.Fatalf("section %s: want one summary for the display card, got %v", section, act["display_card_processes_unknown"])
		}
		if row := sum[0].(map[string]any); row["index"] != float64(1) || row["count"] != float64(31) || row["gpu_uuid"] != procDisplay || row["name"] != "RTX B" {
			t.Errorf("section %s: summary = %v", section, row)
		}
		for _, app := range []string{"explorer.exe", "chrome.exe", "slack.exe"} {
			if strings.Contains(raw, app) {
				t.Errorf("section %s: the folded desktop (%s) is still in the answer", section, app)
			}
		}
	}

	// No card drives a screen: nothing is known to be the desktop, so nothing folds.
	statusGPUSampler = fakeThreeCardSampler(false)
	act, _ := lease("gpu_lease")
	if rows, _ := act["gpu_processes"].([]any); len(rows) != 33 {
		t.Errorf("with no display card flagged all 33 rows are listed, got %d", len(rows))
	}
	if _, ok := act["display_card_processes_unknown"]; ok {
		t.Errorf("no display card, no summary: %v", act["display_card_processes_unknown"])
	}
}
