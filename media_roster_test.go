package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A-15. The 48 GB bake-off (2026-08-31/09-01, `2026-08-31-48gb-bakeoff-renders/`) ran
// the whole media lane on the 3-card reference box, and the tier shipped a media block
// copied byte-for-byte from `blackwell-2x16` — which is why the 3-card box measured no
// media difference from the 2-card one. PR #264 corrected the PLACEMENTS (imagegen
// pool cuda:1/cuda:2, vision and STT onto card 2) and #266 the vision MODEL. Nothing
// ever gated what the bake-off actually decided about the models and their precision,
// so this roster could drift back into a copy without failing anything.
//
// The audit, slot by slot, against the record — and where the record names no winner
// the slot is LEFT ALONE and said so, rather than being changed to look decided:
//
//	vision   qwen3-vl-32b, pair (0,2)  · MMMU 0.740 vs the 8B's 0.630 (+11)
//	ocr      qwen3-vl-8b, card 2       · the 32B REGRESSED here (DocVQA -3.7 + dense
//	                                     transcription), so the roles stay split
//	stt      ggml-large-v3-turbo, card 2 · turbo wins or ties every group (es-long WER
//	                                     0.0652 vs large-v3's 0.0838) at ~2x the speed,
//	                                     so the proposed `stt_hq` lane FAILED and stays
//	                                     empty
//	imagegen krea2 Turbo bf16, 8 steps / cfg 1, pool cuda:1 + donor cuda:2 · Turbo bf16
//	                                     pristine on all 4 prompts; Krea 2 RAW is BROKEN
//	                                     on ComfyUI 0.34.0 under every recipe tested
//	                                     (int8 AND bf16, euler+er_sde x simple/normal,
//	                                     raw_dynamic shift, AuraFlow 3.1) — composition
//	                                     correct, buried in terminal noise, same-seed
//	                                     Turbo clean — so RAW is a fine-tuning base, not
//	                                     a servable lane
//	videogen LTX-2.5 int8-convrot + the CONV video VAE, compute cuda:0 + donor cuda:1 ·
//	                                     round 2 of the decoder A/B measured conv ahead
//	                                     on every one of 5 pairs (+15-25% Laplacian
//	                                     energy, +9-11% high-frequency) with start-frame
//	                                     fidelity a wash (0.02-0.26 dB to diffusion) AND
//	                                     12-15% less wall time. cuda:0 is the documented
//	                                     ComfyUI-MultiGPU #220 exception, not a choice
//	tts      NO WINNER NAMED. Chatterbox v3 and both Spanish packs transcribe back
//	         verbatim (intelligibility proven) but naturalness was left as an operator
//	         A/B and adoption needs a venv bump. The slot is untouched and ungated here.
func TestTripleBlackwellMediaRosterIsTheMeasuredOne(t *testing.T) {
	const tier = "blackwell-3x16"
	p := readProfile(t, tier)

	// --- seats: which model holds which role, and on which card ---
	wantSeat := map[string]struct{ name, pin string }{
		"vision": {"qwen3-vl-32b", "0,2"},
		"ocr":    {"qwen3-vl-8b", "2"},
		"stt":    {"whisper-stt", "2"},
	}
	got := map[string]bool{}
	for _, s := range p.MediaSeats {
		w, ok := wantSeat[s.Kind]
		if !ok {
			continue
		}
		got[s.Kind] = true
		if s.Name != w.name {
			t.Errorf("%s %s seat is %q, want %q — that is the model that won this role in the 48 GB bake-off",
				tier, s.Kind, s.Name, w.name)
		}
		if pin := strings.Join(devicePin(s.GPUEnv), ","); pin != w.pin {
			t.Errorf("%s %s seat is pinned to %q, want %q — the 2026-08-31 G1 rebalance moved whisper and the "+
				"vl-8b/OCR seat onto the second 5060 Ti while the cascade and memory-stack residents kept card 0",
				tier, s.Kind, pin, w.pin)
		}
	}
	for kind := range wantSeat {
		if !got[kind] {
			t.Errorf("%s declares no %s seat — the bake-off measured one for every role", tier, kind)
		}
	}
	if m := seatOfKind(p.MediaSeats, "stt").Model; !strings.Contains(m, "large-v3-turbo") {
		t.Errorf("%s STT model is %q, want the large-v3-TURBO weights: turbo won or tied every group (es-long WER "+
			"0.0652 vs large-v3's 0.0838) at about twice the speed, which is why the proposed stt_hq lane failed",
			tier, m)
	}

	// --- imagegen: the winner, and the arm that is not servable ---
	seed := p.ConfigSeed
	if seed["imagegen_family"] != "krea2" || !strings.Contains(seed["imagegen_ckpt"], "krea2_turbo_bf16") {
		t.Errorf("%s imagegen is %q / %q, want the krea2 Turbo bf16 checkpoint — the arm that came back pristine "+
			"on all four bake-off prompts", tier, seed["imagegen_family"], seed["imagegen_ckpt"])
	}
	if strings.Contains(strings.ToLower(seed["imagegen_ckpt"]), "raw") {
		t.Errorf("%s seeds a Krea 2 RAW checkpoint (%q). RAW is BROKEN on ComfyUI 0.34.0 under every recipe the "+
			"bake-off tried — int8 and bf16, euler+er_sde over simple/normal, raw_dynamic shift, AuraFlow 3.1 — "+
			"composition correct and buried in terminal noise, while the same-seed Turbo render is clean. It is a "+
			"fine-tuning base, not a servable lane", tier, seed["imagegen_ckpt"])
	}
	if seed["imagegen_steps"] != "8" || seed["imagegen_cfg"] != "1" {
		t.Errorf("%s imagegen recipe is %s steps / cfg %s, want 8 / 1 — the production recipe the bake-off rendered "+
			"at", tier, seed["imagegen_steps"], seed["imagegen_cfg"])
	}

	// --- videogen: the decoder the instrument picked ---
	vae := seed["videogen_video_vae"]
	if !strings.Contains(vae, "conv") {
		t.Errorf("%s video VAE is %q, want the CONV decoder. Round 2 of the A/B held seed, prompt, sampler, "+
			"resolution and placement constant and changed only the VAE: conv resolved more detail on every one of "+
			"5 pairs (+15-25%% Laplacian energy, +9-11%% high-frequency), start-frame fidelity was a wash "+
			"(0.02-0.26 dB), and conv was 12-15%% FASTER. It wins on both axes that were measurable", tier, vae)
	}
	if strings.Contains(vae, "diff") {
		t.Errorf("%s video VAE is the diffusion decoder (%q): softer on every measured pair and 12-15%% more wall "+
			"time", tier, vae)
	}
	if !strings.Contains(seed["videogen_transformer"], "int8-convrot") {
		t.Errorf("%s videogen transformer is %q, want the int8-convrot LTX-2.5 build — the bf16 transformer OOMs on "+
			"the COMPUTE device's activations at 1920x1088, which a donor card cannot fix",
			tier, seed["videogen_transformer"])
	}

	// --- absence: the 3-card pool keys are this tier's, and only this tier's ---
	two := readProfile(t, "blackwell-2x16")
	if two.ConfigSeed["imagegen_pool_compute"] != "cuda:0" || two.ConfigSeed["imagegen_pool_donor"] != "cuda:1" {
		t.Errorf("blackwell-2x16 imagegen pool is %q/%q, want cuda:0/cuda:1 — a two-card box has no third card, "+
			"and copying this tier's cuda:1/cuda:2 back onto it is the same unadapted copy in the other direction",
			two.ConfigSeed["imagegen_pool_compute"], two.ConfigSeed["imagegen_pool_donor"])
	}
}

type rosterSeat struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Model  string   `json:"model"`
	GPUEnv []string `json:"gpu_env"`
}

type profileRow struct {
	RawSeed    map[string]any `json:"config_seed"`
	MediaSeats []rosterSeat   `json:"media_seats"`
	// ConfigSeed is RawSeed flattened to strings, so a JSON number and a JSON string
	// compare the same way.
	ConfigSeed map[string]string `json:"-"`
}

// readProfile reads one committed tier row. A fixture would pass while the shipped
// roster was a copy again, which is the whole failure this file exists for.
func readProfile(t *testing.T, tier string) profileRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]profileRow `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	p, ok := doc.Profiles[tier]
	if !ok {
		t.Fatalf("tier %q not found — this gate went blind", tier)
	}
	if len(p.MediaSeats) == 0 {
		t.Fatalf("tier %q declares no media seats — this gate went blind", tier)
	}
	p.ConfigSeed = map[string]string{}
	for k, v := range p.RawSeed {
		switch tv := v.(type) {
		case string:
			p.ConfigSeed[k] = tv
		case float64:
			p.ConfigSeed[k] = strconv.FormatFloat(tv, 'f', -1, 64)
		}
	}
	return p
}

func seatOfKind(seats []rosterSeat, kind string) rosterSeat {
	for _, s := range seats {
		if s.Kind == kind {
			return s
		}
	}
	return rosterSeat{}
}
