package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vision seat is a QUALITY binding, and blackwell-3x16 shipped the loser of its
// own bake-off.
//
// The 48 GB gates measured Qwen3-VL-32B against the incumbent 8B on the reference box
// and split the roles by which model won which:
//
//	visioncanary MMMU  0.740 vs 0.630  (+11 points, clears the >=8 promotion bar)
//	    -> the 32B takes VISION
//	DocVQA ANLS 0.928 vs 0.965 (-3.7) plus a dense-transcription regression
//	    -> the 8B KEEPS OCR
//
// That split was applied on the box — both live config files bind
// vision_model=qwen3-vl-32b and ocr_model=qwen3-vl-8b — and the tier table was never
// updated, so a fresh install of the tier whose reference box IS that machine got the
// 8B for vision: a measured 11-point MMMU regression, shipped silently.
//
// This gate holds the split and the flags it was measured at. It is deliberately
// specific: the point is not "a vision seat exists" but "the seat is the model that
// won, configured the way it won".
func TestTripleBlackwellVisionSeatIsTheMeasuredWinner(t *testing.T) {
	const tier = "blackwell-3x16"

	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			MediaSeats []struct {
				Kind           string   `json:"kind"`
				Name           string   `json:"name"`
				CtxSize        int      `json:"ctx_size"`
				ImageMinTokens int      `json:"image_min_tokens"`
				SplitMode      string   `json:"split_mode"`
				TensorSplit    string   `json:"tensor_split"`
				Temp           *float64 `json:"temp"`
				TopP           *float64 `json:"top_p"`
				TopK           *int     `json:"top_k"`
				GPUEnv         []string `json:"gpu_env"`
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

	byKind := map[string]int{}
	for i, s := range p.MediaSeats {
		byKind[s.Kind] = i
	}
	vi, hasVision := byKind["vision"]
	oi, hasOCR := byKind["ocr"]
	if !hasVision || !hasOCR {
		t.Fatalf("%s must declare BOTH a vision and an ocr seat — the 48 GB gates split the roles because the 32B "+
			"won MMMU (+11) while the 8B won dense transcription. Collapsing them back to one seat regresses one "+
			"role or the other. Got kinds: %v", tier, kinds(p.MediaSeats))
	}
	v, o := p.MediaSeats[vi], p.MediaSeats[oi]

	if v.Name != "qwen3-vl-32b" {
		t.Errorf("%s vision seat is %q; the measured winner is qwen3-vl-32b (visioncanary MMMU 0.740 vs the 8B's "+
			"0.630) and it is what the reference box binds as vision_model", tier, v.Name)
	}
	if o.Name != "qwen3-vl-8b" {
		t.Errorf("%s ocr seat is %q; the 8B keeps OCR because the 32B REGRESSED there (DocVQA ANLS -3.7 plus a "+
			"dense-transcription regression)", tier, o.Name)
	}

	// The flags the 32B was measured with. Each is load-bearing, not decoration.
	if v.SplitMode != "tensor" {
		t.Errorf("%s vision seat split_mode is %q, want \"tensor\": at the SAME --tensor-split, tensor mode produced "+
			"byte-identical output 5/5 on a fixed image+prompt while raising generation 19.6 -> 34.1 t/s "+
			"(CUDA-X 2026-09-05). Identical output is what makes it free rather than a trade", tier, v.SplitMode)
	}
	if v.TensorSplit == "" {
		t.Errorf("%s vision seat declares split_mode but no tensor_split — llama.cpp would divide it by its own "+
			"default rather than the measured proportions", tier)
	}
	if v.ImageMinTokens <= 0 {
		t.Errorf("%s vision seat has no image_min_tokens; the reference seat pins 1024. Without a floor a large VLM "+
			"answers confidently off a thumbnail, which is a silent quality loss", tier)
	}
	// Qwen3-VL's official sampler: all three together or none. A VLM run off its own
	// recipe is an unmeasured quality change.
	if v.Temp == nil || v.TopP == nil || v.TopK == nil {
		t.Errorf("%s vision seat must pin the vendor sampler (temp/top_p/top_k) together; got temp=%v top_p=%v "+
			"top_k=%v", tier, deref(v.Temp), deref(v.TopP), derefI(v.TopK))
	}
	// A 32B does not fit one 16 GB card: it must name both cards of the pair, and the
	// proportion list is positional over exactly those devices.
	pin := ""
	for _, e := range v.GPUEnv {
		if s, ok := strings.CutPrefix(strings.TrimSpace(e), "CUDA_VISIBLE_DEVICES="); ok {
			pin = s
		}
	}
	devs := strings.Split(pin, ",")
	if pin == "" || len(devs) != 2 {
		t.Errorf("%s vision seat must pin exactly the two 5060 Ti cards (a 32B does not fit one 16 GB card); got %q",
			tier, pin)
	}
	if v.TensorSplit != "" && len(strings.Split(v.TensorSplit, ",")) != len(devs) {
		t.Errorf("%s vision seat tensor_split %q does not match its %d visible device(s) — the list is positional",
			tier, v.TensorSplit, len(devs))
	}
}

func kinds(seats []struct {
	Kind           string   `json:"kind"`
	Name           string   `json:"name"`
	CtxSize        int      `json:"ctx_size"`
	ImageMinTokens int      `json:"image_min_tokens"`
	SplitMode      string   `json:"split_mode"`
	TensorSplit    string   `json:"tensor_split"`
	Temp           *float64 `json:"temp"`
	TopP           *float64 `json:"top_p"`
	TopK           *int     `json:"top_k"`
	GPUEnv         []string `json:"gpu_env"`
}) []string {
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		out = append(out, s.Kind+":"+s.Name)
	}
	return out
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func derefI(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}
