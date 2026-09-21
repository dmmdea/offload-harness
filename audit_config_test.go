package main

import "testing"

// TestClassifyConfigDriftNamesTheHandWiredWin pins the four classes on the exact shape that hid
// the binxarn agent seat: the node carried a hand-set agent_model the seed did not write, while
// the seed wrote a DIFFERENT value, and a node-local endpoint that no tier owns must stay silent.
func TestClassifyConfigDriftNamesTheHandWiredWin(t *testing.T) {
	seed := map[string]any{
		"agent_model":   "gemma4-e2b", // what the fallback derived
		"ctx_size_hint": 32768,        // an int in the seed ...
		"stt_model":     "turbo.bin",  // the node never got it
	}
	live := map[string]any{
		"agent_model":      "qwen3.5-4b-agent",       // the measured winner, hand-set
		"ctx_size_hint":    float64(32768),           // ... and the float64 JSON decodes to: the SAME number
		"agent_seat_tok_s": float64(10),              // owned by some tier's seed, absent from this one
		"endpoint":         "http://127.0.0.1:11436", // node-local, owned by no seed
	}
	owned := map[string]bool{"agent_model": true, "ctx_size_hint": true, "stt_model": true, "agent_seat_tok_s": true}

	got := map[string]configDriftClass{}
	for _, f := range classifyConfigDrift(seed, live, owned, isBindingKey) {
		got[f.Key] = f.Class
	}
	want := map[string]configDriftClass{
		"agent_model":      driftDifferent,
		"ctx_size_hint":    driftMatch,
		"stt_model":        driftSeedOnly,
		"agent_seat_tok_s": driftLiveOnly,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s classified %q, want %q", k, got[k], w)
		}
	}
	if c, present := got["endpoint"]; present {
		t.Errorf("endpoint is owned by no seed and must not be reported, got %q — a report that flags node-local keys buries the real drift", c)
	}
}

// TestClassifyConfigDriftPutsDriftFirst: the report exists to be read, so what differs leads and
// what matches trails.
func TestClassifyConfigDriftPutsDriftFirst(t *testing.T) {
	seed := map[string]any{"a": 1, "b": 2}
	live := map[string]any{"a": float64(1), "b": float64(3), "c": "x"}
	owned := map[string]bool{"a": true, "b": true, "c": true}
	out := classifyConfigDrift(seed, live, owned, isBindingKey)
	order := []configDriftClass{}
	for _, f := range out {
		order = append(order, f.Class)
	}
	want := []configDriftClass{driftDifferent, driftLiveOnly, driftMatch}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got %v, want %v", order, want)
		}
	}
}

// TestClassifyConfigDriftSeesABindingNoTierSeeds pins the blind spot the first cut had: the Qube's
// image-edit and animate routes were wired by hand and are seeded by NO tier, so a comparison over
// seed-owned keys alone could not see them. They must surface as UNSEEDED; a node-local endpoint or
// path must not.
func TestClassifyConfigDriftSeesABindingNoTierSeeds(t *testing.T) {
	live := map[string]any{
		"gen_edit_script":   "D:/render/edit.py",
		"animategen_script": "D:/render/animate.py",
		"comfy_dir":         "D:/ComfyUI",
		"endpoint":          "http://127.0.0.1:11436",
	}
	got := map[string]configDriftClass{}
	for _, f := range classifyConfigDrift(map[string]any{}, live, map[string]bool{}, isBindingKey) {
		got[f.Key] = f.Class
	}
	for _, k := range []string{"gen_edit_script", "animategen_script"} {
		if got[k] != driftUnseeded {
			t.Errorf("%s classified %q, want UNSEEDED — a hand-wired route no tier carries is exactly the win a fresh install loses", k, got[k])
		}
	}
	for _, k := range []string{"comfy_dir", "endpoint"} {
		if c, ok := got[k]; ok {
			t.Errorf("%s is node-local and must not be reported, got %q", k, c)
		}
	}
}
