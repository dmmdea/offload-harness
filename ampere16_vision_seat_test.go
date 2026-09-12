package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestAmpere16VisionSeatIsTheMeasuredWinner pins the ampere-16 vision seat to
// the 2026-09-12 bake-off result on the tier's reference box (Lenovo M720q,
// NVIDIA A2 16 GB): Qwen3.8-27B UD-IQ3_S + mmproj-F16 was the only candidate
// that read every serial number, email and low-contrast string (OCR 9/9, VQA
// 5/5) in the harness's own assess_image / ocr / vqa over a labelled set; the
// seat it replaced (qwen3-vl-8b, inherited from blackwell-16) was never run on
// this silicon. Record: Benchmarks and Optimizations/2026-09-12-a2-vision-bakeoff/.
//
// The flags are the measured ones and each is load-bearing: reasoning off (a
// thinking template spends the budget in reasoning_content and returns empty
// content — offload-harness#168), ctx 8192 (12.4 GiB resident leaves ~2.5 GiB
// on a 15,356 MiB card; a larger KV would not fit beside the mmproj compute
// buffers), swappable (the heavy group holds exactly one big seat).
func TestAmpere16VisionSeatIsTheMeasuredWinner(t *testing.T) {
	const tier = "ampere-16"
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			MediaSeats []struct {
				Kind      string   `json:"kind"`
				Name      string   `json:"name"`
				Aliases   []string `json:"aliases"`
				Model     string   `json:"model"`
				Mmproj    string   `json:"mmproj"`
				CtxSize   int      `json:"ctx_size"`
				Reasoning string   `json:"reasoning"`
				Residency string   `json:"residency"`
				Measured  string   `json:"measured"`
			} `json:"media_seats"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	p, ok := doc.Profiles[tier]
	if !ok {
		t.Fatalf("tier %q not found — this gate went blind", tier)
	}
	var vision *struct {
		Kind      string   `json:"kind"`
		Name      string   `json:"name"`
		Aliases   []string `json:"aliases"`
		Model     string   `json:"model"`
		Mmproj    string   `json:"mmproj"`
		CtxSize   int      `json:"ctx_size"`
		Reasoning string   `json:"reasoning"`
		Residency string   `json:"residency"`
		Measured  string   `json:"measured"`
	}
	for i := range p.MediaSeats {
		if p.MediaSeats[i].Kind == "vision" {
			vision = &p.MediaSeats[i]
		}
	}
	if vision == nil {
		t.Fatalf("%s declares no vision seat", tier)
	}
	if vision.Name != "qwen38-27b-vision" || vision.Model != "Qwen3.8-27B-UD-IQ3_S.gguf" || vision.Mmproj != "mmproj-F16.gguf" {
		t.Errorf("%s vision seat is %q (%s + %s); the measured winner is qwen38-27b-vision = Qwen3.8-27B-UD-IQ3_S.gguf + mmproj-F16.gguf "+
			"(2026-09-12 bake-off on the reference A2: OCR 9/9, VQA 5/5, the only candidate that read every code and low-contrast string)",
			tier, vision.Name, vision.Model, vision.Mmproj)
	}
	if vision.Reasoning != "off" {
		t.Errorf("%s vision seat reasoning is %q, want \"off\": a thinking vision template returns empty content (offload-harness#168)", tier, vision.Reasoning)
	}
	if vision.CtxSize != 8192 {
		t.Errorf("%s vision seat ctx_size is %d, want 8192: 12.4 GiB resident on a 15,356 MiB card leaves no room for a larger KV", tier, vision.CtxSize)
	}
	if vision.Residency != "swappable" {
		t.Errorf("%s vision seat residency is %q, want swappable: the heavy group holds exactly one big seat", tier, vision.Residency)
	}
	if vision.Measured == "" {
		t.Errorf("%s vision seat carries no `measured` record; a seat chosen by bake-off must say where the numbers are", tier)
	}
	// ocr rides the same seat on this tier (no ocr_model): the alias must stay.
	hasOCR := false
	for _, a := range vision.Aliases {
		if a == "ocr" {
			hasOCR = true
		}
	}
	if !hasOCR {
		t.Errorf("%s vision seat lost the ocr alias; this tier has no separate OCR seat", tier)
	}
}
