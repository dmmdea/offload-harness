package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpucards"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// `gpu status` and `gpu cards` carry the display card's cap verdict on its row of the card table: the VRAM in use
// now (the desktop and every process counted) against the share gpu_display_cap_pct allows, by the allocator's own
// arithmetic. The cap is read from the config file as it is when the verb runs. Fixtures are synthetic.

// capStatusCards is a three-card box whose card 1 is the display card with displayFree GiB free of 16.0.
func capStatusCards(displayFree float64, attachedUnknown bool) []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "Test Card A", TotalGiB: 16, FreeGiB: 15, UtilKnown: true, AttachedUnknown: attachedUnknown},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "Test Card B", TotalGiB: 16, FreeGiB: displayFree, UtilKnown: true, DisplayActive: true, AttachedUnknown: attachedUnknown},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "Test Card A", TotalGiB: 16, FreeGiB: 16, UtilKnown: true, AttachedUnknown: attachedUnknown},
	}
	cards, _ := gpuprobe.BuildCards(devs, "")
	return cards
}

// writeCapConfig rewrites the verb fixture's config file with a display cap (a negative cap leaves the key out).
func writeCapConfig(t *testing.T, cfgPath string, capPct int) {
	t.Helper()
	body := `{"state_dir": ` + strconv.Quote(filepath.Dir(cfgPath)) + `, "gpu_card_scoped_leases": true`
	if capPct >= 0 {
		body += `, "gpu_display_cap_pct": ` + strconv.Itoa(capPct)
	}
	body += `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statusRows(t *testing.T, out string) (rows []gpucards.Row, raw []map[string]any) {
	t.Helper()
	var doc struct {
		Cards []gpucards.Row `json:"cards"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	var rawDoc struct {
		Cards []map[string]any `json:"cards"`
	}
	_ = json.Unmarshal([]byte(out), &rawDoc)
	if len(doc.Cards) != 3 {
		t.Fatalf("one row per card: %s", out)
	}
	return doc.Cards, rawDoc.Cards
}

func TestGPUStatusJSONCarriesTheDisplayCapOnTheDisplayCard(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	writeCapConfig(t, cfg, 60)
	useQuietStatus(t, capStatusCards(3.0, false), nil)

	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	rows, raw := statusRows(t, out)
	d := rows[1].DisplayCap
	if d == nil {
		t.Fatalf("the display card's row must carry display_cap:\n%s", out)
	}
	if !d.OverCap || d.CapPct != 60 || d.BudgetGiB < 9.59 || d.BudgetGiB > 9.61 || d.UsedGiB < 12.99 || d.UsedGiB > 13.01 || d.OverByGiB < 3.39 || d.OverByGiB > 3.41 {
		t.Errorf("13.0 of 16.0 GiB in use against a cap of 60 (budget 9.6, over by 3.4): %+v", *d)
	}
	for _, i := range []int{0, 2} {
		if _, has := raw[i]["display_cap"]; has {
			t.Errorf("card %d is not the display card and carries no display_cap: %v", i, raw[i])
		}
	}

	// The cap is the file's at the time of the call: a looser cap written since is within; a 0 is closed, never over.
	writeCapConfig(t, cfg, 95)
	out = captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	rows, _ = statusRows(t, out)
	if d := rows[1].DisplayCap; d == nil || d.OverCap || d.CapPct != 95 || d.Closed {
		t.Errorf("a cap of 95: 13.0 of 16.0 GiB is within it: %+v", d)
	}
	writeCapConfig(t, cfg, 0)
	out = captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	rows, _ = statusRows(t, out)
	if d := rows[1].DisplayCap; d == nil || !d.Closed || d.OverCap || d.CapPct != 0 {
		t.Errorf("a cap of 0 is closed and not over: %+v", d)
	}
	// An unset key is the default, 60.
	writeCapConfig(t, cfg, -1)
	out = captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	rows, _ = statusRows(t, out)
	if d := rows[1].DisplayCap; d == nil || d.CapPct != 60 || !d.OverCap {
		t.Errorf("the key unset reads as the default 60: %+v", d)
	}
}

func TestGPUStatusTextSaysWhenTheDisplayCardIsOverItsCap(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	writeCapConfig(t, cfg, 60)
	useQuietStatus(t, capStatusCards(3.0, false), nil)

	text := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"card table:", "display cap:", "OVER by 3.4 GiB", "gpu_display_cap_pct 60", "desktop and every process"} {
		if !strings.Contains(text, want) {
			t.Errorf("`gpu status` must contain %q:\n%s", want, text)
		}
	}
	// The positive control: within the cap the line says within.
	writeCapConfig(t, cfg, 95)
	text = captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(text, "display cap:") || !strings.Contains(text, "within") || strings.Contains(text, "OVER") {
		t.Errorf("a card within its cap says within:\n%s", text)
	}
}

func TestGPUCardsVerbCarriesTheDisplayCapInJSONAndText(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	writeCapConfig(t, cfg, 60)
	useQuietStatus(t, capStatusCards(3.0, false), nil)

	out := captureStdout(t, func() {
		if err := runGPUCards([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	rows, raw := statusRows(t, out)
	if d := rows[1].DisplayCap; d == nil || !d.OverCap || d.CapPct != 60 {
		t.Errorf("`gpu cards --json`: the display card's row carries the verdict: %+v", d)
	}
	if _, has := raw[0]["display_cap"]; has {
		t.Errorf("only the display card does: %v", raw[0])
	}
	text := captureStdout(t, func() {
		if err := runGPUCards([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(text, "display cap:") || !strings.Contains(text, "OVER by 3.4 GiB") {
		t.Errorf("`gpu cards` prints the display cap line:\n%s", text)
	}
	// A reading that could not say which card the monitor is on is not judged, on either surface.
	useQuietStatus(t, capStatusCards(3.0, true), nil)
	text = captureStdout(t, func() {
		if err := runGPUCards([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(text, "OVER") || !strings.Contains(text, "could not say which card the monitor is on") {
		t.Errorf("an unknown reading is said, not judged:\n%s", text)
	}
}
