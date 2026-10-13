package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// offload_status {section:"brief"} carries the display card's cap verdict on its row of gpu_cards, the same
// rows `gpu status` prints. The server is long-lived and its Config is read once, so the cap must be read from the
// config file as it is when the call is made (config.LiveGPUDisplayCap), exactly as the card allocator's input does:
// the operator's "I need the card" switch (0) written while the server runs has to show.

// capSampler is a three-card sample whose card 1 is the display card with 13281 of 16311 MiB in use (81 percent).
func capSampler(attachedUnknown bool) func(context.Context) ([]gpuactivity.GPU, error) {
	return func(context.Context) ([]gpuactivity.GPU, error) {
		return []gpuactivity.GPU{
			{Index: 0, UUID: "GPU-aaaa0000-x", Name: "Test Card", MemTotalMiB: 16311, MemUsedMiB: 1024, UtilKnown: true, AttachedUnknown: attachedUnknown},
			{Index: 1, UUID: "GPU-bbbb0000-x", Name: "Test Card", MemTotalMiB: 16311, MemUsedMiB: 13281, UtilKnown: true, DisplayActive: true, AttachedUnknown: attachedUnknown},
			{Index: 2, UUID: "GPU-cccc0000-x", Name: "Test Card", MemTotalMiB: 16311, MemUsedMiB: 0, UtilKnown: true, AttachedUnknown: attachedUnknown},
		}, nil
	}
}

func useCapSampler(t *testing.T, attachedUnknown bool) {
	t.Helper()
	statusSamplesGPU = true
	statusGPUSampler = capSampler(attachedUnknown)
	statusGPUProcSampler = func(context.Context) ([]gpuactivity.GPUProcess, error) { return nil, nil }
	t.Cleanup(func() { statusGPUSampler, statusGPUProcSampler = nil, nil })
}

// briefCards runs the brief and returns its gpu_cards block as generic JSON.
func briefCards(t *testing.T, s *Server) (brief map[string]any, rows []map[string]any) {
	t.Helper()
	brief = decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"brief"}`)))
	block, _ := brief["gpu_cards"].(map[string]any)
	list, _ := block["cards"].([]any)
	if len(list) != 3 {
		t.Fatalf("one row per card: %v", block)
	}
	for _, r := range list {
		rows = append(rows, r.(map[string]any))
	}
	return brief, rows
}

func TestBriefGPUCardsCarryTheDisplayCap(t *testing.T) {
	s, _, _ := statusFixture(t)
	useCapSampler(t, false)

	brief, rows := briefCards(t, s)
	dc, _ := rows[1]["display_cap"].(map[string]any)
	if dc == nil {
		t.Fatalf("the display card's row must carry display_cap: %v", rows[1])
	}
	if dc["over_cap"] != true || dc["cap_pct"] != float64(60) {
		t.Errorf("13281 of 16311 MiB in use against the default cap of 60 is over: %v", dc)
	}
	if used, _ := dc["used_pct"].(float64); used < 81 || used > 82 {
		t.Errorf("used_pct is about 81: %v", dc)
	}
	for _, i := range []int{0, 2} {
		if _, has := rows[i]["display_cap"]; has {
			t.Errorf("card %v is not the display card and carries no display_cap: %v", rows[i]["index"], rows[i])
		}
	}
	// The brief's own key set is what it was: the verdict rides a row, it adds no top-level key.
	want := []string{"fleet", "gpu_cards", "gpu_lease_verdict", "local_verdict", "process"}
	if got := sortedKeys(brief); !reflect.DeepEqual(got, want) {
		t.Errorf("brief keys = %v, want %v", got, want)
	}
}

// The mutation this kills is "read the cap at load": the server's Config holds the cap it was started with, and
// only the file says what the operator wrote since.
func TestBriefReadsTheLiveDisplayCapNotTheOneAtLoad(t *testing.T) {
	fixture, cfg, _ := statusFixture(t)
	_ = fixture
	useCapSampler(t, false)

	path := filepath.Join(t.TempDir(), "config.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"gpu_display_cap_pct": 60}`)
	atLoad := 60
	cfg.GPUDisplayCapPct = &atLoad
	cfg = cfg.WithSource(path)
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.startup = fixture.startup

	// Control: the cap in the file at the time of the call is the cap judged.
	_, rows := briefCards(t, s)
	if dc, _ := rows[1]["display_cap"].(map[string]any); dc == nil || dc["cap_pct"] != float64(60) || dc["over_cap"] != true {
		t.Fatalf("control: cap 60 in the file, 81 percent in use: over %v", rows[1])
	}

	// The operator writes 0 while the server runs: the card is closed, and the row says so, not "over".
	write(`{"gpu_display_cap_pct": 0}`)
	_, rows = briefCards(t, s)
	dc, _ := rows[1]["display_cap"].(map[string]any)
	if dc == nil || dc["closed"] != true || dc["cap_pct"] != float64(0) || dc["over_cap"] != false {
		t.Fatalf("the switch written after the server started must show: closed, cap_pct 0, not over; got %v", rows[1]["display_cap"])
	}

	// And a looser cap written later reads as within.
	write(`{"gpu_display_cap_pct": 95}`)
	_, rows = briefCards(t, s)
	dc, _ = rows[1]["display_cap"].(map[string]any)
	if dc == nil || dc["cap_pct"] != float64(95) || dc["over_cap"] != false || dc["closed"] == true {
		t.Fatalf("a cap of 95 written later: 81 percent is within it; got %v", rows[1]["display_cap"])
	}
}

// The brief builds its table from a sample that discarded whether display_attached was read. A reading that
// could not say which card the monitor is on is carried through (gpuactivity.GPU.AttachedUnknown), so the brief says
// "not judged" instead of showing no display card at all.
func TestBriefSaysWhenTheReadingCouldNotSayWhichCardTheMonitorIsOn(t *testing.T) {
	s, _, _ := statusFixture(t)
	useCapSampler(t, true)

	_, rows := briefCards(t, s)
	for _, r := range rows {
		if r["display_unknown"] != true {
			t.Errorf("card %v: the row must carry display_unknown: %v", r["index"], r)
		}
		dc, _ := r["display_cap"].(map[string]any)
		if dc == nil || !strings.Contains(dc["unknown"].(string), "could not say which card the monitor is on") {
			t.Errorf("card %v: the cap is not judged on such a reading: %v", r["index"], r["display_cap"])
		}
		if dc != nil && dc["over_cap"] == true {
			t.Errorf("card %v: no verdict on an unknown reading: %v", r["index"], dc)
		}
	}
}
