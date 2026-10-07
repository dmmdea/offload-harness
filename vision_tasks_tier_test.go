package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// The FLEET vision lane advertises vqa, ocr and assess_image on any node whose seed binds a vision
// seat without declaring tasks, so a Q4 seat that a tier page itself calls inadequate for OCR was
// handed remote OCR (fleet-first R2, finding R3-F14). A tier narrows the lane in data: the seat's
// `tasks`, written by mediaseat.Bindings as vision_tasks.
//
// This table is every shipped tier that binds a vision seat, with the lane it declares and WHY. A
// tier added to profiles.json without a row fails here, so the choice is made on purpose; a row is
// changed only with a measurement (an ocrprobe run) or a changed tier-page statement.
//
// What the shipped pages say, which is all this table acts on:
//   - ampere-8: "the vision seat carries NO ocr alias BY DESIGN pending ocrprobe measurement --
//     blackwell-16 records Q4 as the measured OCR-fidelity cliff, so the omission is a quality
//     statement". Declared: vqa and assess_image, no ocr.
//   - ampere-6, amd-gcn, amd-rdna3, amd-rdna3-dgpu, cpu, dual-gpu: Q4 vision seats whose pages make
//     NO statement about OCR quality (the fleet-first review calls them "unmeasured on Q4"). The
//     page being silent is not a verdict, so they keep the lane unrestricted until a measurement or
//     a page says otherwise.
//   - rockchip-rk3588: the runtime cannot constrain sampling, and assess_image always sends a
//     grammar, so it serves vqa and ocr only (TestRK3588VisionLaneIsLimitedToWhatTheRuntimeCanDo).
func TestShippedTiersDeclareTheVisionLaneTheirPagesSupport(t *testing.T) {
	var (
		unrestricted []string // nil: the node serves all three, as every node did before tiers could narrow it
		noOCR        = []string{"vqa", "assess_image"}
	)
	want := map[string][]string{
		"ampere-8":        noOCR,
		"rockchip-rk3588": {"vqa", "ocr"},

		"ampere-6":       unrestricted,
		"amd-gcn":        unrestricted,
		"amd-rdna3":      unrestricted,
		"amd-rdna3-dgpu": unrestricted,
		"cpu":            unrestricted,
		"dual-gpu":       unrestricted,

		// Q8 or larger vision seats, and tiers with a dedicated OCR model bound as ocr_model.
		"blackwell-8":    unrestricted,
		"blackwell-16":   unrestricted,
		"blackwell-2x16": unrestricted,
		"blackwell-32":   unrestricted,
		"blackwell-3x16": unrestricted,
		"blackwell-48":   unrestricted,
		"blackwell-72":   unrestricted,
		"ampere-16":      unrestricted,
		"volta-16":       unrestricted,
	}
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cfg := effectiveConfig(t, profiles[id], id, "linux")
		if cfg.VisionModel == "" {
			continue // no vision seat bound: nothing to declare
		}
		w, ok := want[id]
		if !ok {
			t.Errorf("tier %s binds the vision seat %q and has no row in this table: decide its fleet vision lane (declare `tasks` on the seat, or add the row as unrestricted with the page evidence)", id, cfg.VisionModel)
			continue
		}
		if !reflect.DeepEqual(cfg.VisionTasks, w) && !(len(cfg.VisionTasks) == 0 && len(w) == 0) {
			t.Errorf("tier %s vision_tasks = %v, want %v", id, cfg.VisionTasks, w)
		}
	}
	for id := range want {
		if _, exists := profiles[id]; !exists {
			t.Errorf("the table names tier %q, which profiles.json no longer has", id)
		}
	}
}

// The ampere-8 declaration rests on its own page's statement, so the two must stay together: if the
// quality statement is rewritten (an ocrprobe measurement clears the seat), the declaration is the
// thing to revisit, and this fails to say so.
func TestAmpere8OCRWithholdingRestsOnItsPageStatement(t *testing.T) {
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["ampere-8"]
	page, err := os.ReadFile(filepath.Join("docs", "tiers", "ampere-8.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "NO ocr alias BY DESIGN") {
		t.Fatalf("ampere-8's page no longer says its vision seat carries no ocr alias by design; its seat declares tasks without ocr on that statement. Revisit the declaration (and drop it if a measurement clears the seat).")
	}
	var found bool
	for _, s := range p.MediaSeats {
		if s.Kind == "vision" && !s.Extra {
			found = true
			if strings.Contains(strings.Join(s.Tasks, ","), "ocr") {
				t.Errorf("ampere-8's vision seat declares ocr (%v) while its page withholds the ocr alias as a quality statement", s.Tasks)
			}
		}
	}
	if !found {
		t.Fatal("ampere-8 has no bound vision seat")
	}
}
