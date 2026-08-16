package tierdocs

import "testing"

// TestMediaSeedKeyCoversMediacapRouteKeys pins mediaSeedKey against the route
// keys internal/mediacap actually derives CONFIGURED / NOT CONFIGURED from
// (mediacap.go: inpaint_script/inpaint_ckpt, gen_edit_script/gen_edit_unet,
// run_graph_script, comfy_dir, edit_python) plus the seed families the docs
// already classified as media. A key on this list drifting to non-media makes
// a tier page assert "ships no media configuration" over a tier that ships
// exactly that — the false-claim wart the partition exists to prevent.
func TestMediaSeedKeyCoversMediacapRouteKeys(t *testing.T) {
	media := []string{
		// mediacap's own route-deciding keys:
		"inpaint_script", "inpaint_ckpt", "inpaint_vae",
		"gen_edit_script", "gen_edit_unet", "gen_edit_preset",
		"run_graph_script", "comfy_dir", "edit_python",
		// spawn-per-job seed families:
		"imagegen_family", "imagegen_ckpt", "imagegen_pool_vvram_gb",
		"videogen_family", "videogen_transformer", "videogen_width",
		"musicgen_script", "voicegen_script",
		// sd.cpp engine family:
		"sdcpp_bin", "vae_mode",
	}
	for _, k := range media {
		if !mediaSeedKey(k) {
			t.Errorf("mediaSeedKey(%q) = false — a media route key classified non-media", k)
		}
	}
	nonMedia := []string{
		"agent_model", "agent_timeout_sec", "agent_ctx_tokens",
		"escalation_model", "reasoning_model", "triage_model",
		"fleet_sampler", "nim_model", "nim_endpoint",
	}
	for _, k := range nonMedia {
		if mediaSeedKey(k) {
			t.Errorf("mediaSeedKey(%q) = true — harness config classified as media", k)
		}
	}
}
