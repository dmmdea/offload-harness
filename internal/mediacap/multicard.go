package mediacap

import "regexp"

// THE CARD-ONLY RECIPES OF THE IMAGE AND EDIT ROUTES (--placement multicard on render/comfy-generate.mjs, comfy-render.mjs, comfy-edit.mjs).
//
// render/multicard-recipes.json is the runner's file: which loader holds which weight on which card, the DiT's resident share and the per-card VRAM
// peak a call is held to. This is the Go copy of the part the harness needs to decide with: which call has a recipe (the doctor's node classes,
// internal/hostneed's host-RAM declaration) and what it was measured to hold. TestTheMulticardRowsAreTheRunnersRecipes fails when either side moves.
//
// Measured is true only for a row whose host growth was read in a run that used the read-only safetensors loader (a card-only run that charged no
// weight file to host commit). Every image arm of the 2026-10-11 two-card benchmark ran the stock loader, so only the 2511 fp8mixed edit is measured;
// the other four are predicted. A live run with the read-only loader promotes a row: flip Measured here and "basis" in the JSON.

// MulticardRow is one recipe: the identity of the call it serves, how the runner loads it and the figures that decide a declaration.
type MulticardRow struct {
	ID, Route, Family, Quant string
	// UnetMatch and ClipMatch are case-insensitive regular expressions on the DiT file and the text-encoder file; DefaultClip stands in for a call
	// that names no text encoder (the builder's own).
	UnetMatch, ClipMatch, DefaultClip string
	// UnetLoader is plain (the whole DiT on the compute card), distorch2 or gguf-distorch2 (a split DiT); ClipLoader is plain (the encoder whole on
	// the storage card) or distorch2 (a bf16 encoder too big for one card, split over both).
	UnetLoader, ClipLoader string
	// ResidentGiB is the share of a split DiT kept on the compute card; ClipResidentGiB the share of a split text encoder.
	ResidentGiB, ClipResidentGiB float64
	// ComputeGiB and StorageGiB are the per-card VRAM peaks a call on this row is held to.
	ComputeGiB, StorageGiB float64
	// Measured: the row's host growth was read in a run that used the read-only loader.
	Measured bool
	// MaxMegapixels is the largest edit canvas the compute peak is scaled to (0 = the row has none).
	MaxMegapixels float64
	// LoraLoader is the node class that applies a LoRA to the DiT under this recipe ("" = the recipe has none to name).
	LoraLoader string
	// Packs are the custom-node packs the recipe's loaders need: multigpu, and gguf for a GGUF DiT.
	Packs []string
}

// Plain reports a recipe that puts the whole DiT on the compute card (no resident share to set).
func (r MulticardRow) Plain() bool { return r.UnetLoader == "plain" }

var multicardRows = []MulticardRow{
	{ID: "qwen-image-2.1/int8_convrot", Route: "image", Family: "qwen-image-2.1", Quant: "int8_convrot", UnetMatch: `int8.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "plain", ClipLoader: "distorch2", ClipResidentGiB: 5, ComputeGiB: 9.0, StorageGiB: 13.6, Packs: []string{"multigpu"}},
	{ID: "qwen-image-2.1/bf16", Route: "image", Family: "qwen-image-2.1", Quant: "bf16", UnetMatch: `bf16.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "distorch2", ResidentGiB: 8, ClipLoader: "distorch2", ClipResidentGiB: 5, ComputeGiB: 10.8, StorageGiB: 14.2, Packs: []string{"multigpu"}},
	{ID: "qwen-image/gguf-q5_1", Route: "image", Family: "qwen-image", Quant: "gguf-q5_1", UnetMatch: `Q5_1\.gguf$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "gguf-distorch2", ResidentGiB: 3, ClipLoader: "plain", ComputeGiB: 4.9, StorageGiB: 12.6, LoraLoader: "LoraLoaderModelOnly", Packs: []string{"multigpu", "gguf"}},
	{ID: "qwen-image-edit-2511/fp8mixed", Route: "edit", Family: "qwen-image-edit-2511", Quant: "fp8mixed", UnetMatch: `fp8mixed.*\.safetensors$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "distorch2", ResidentGiB: 9, ClipLoader: "plain", ComputeGiB: 13.0, StorageGiB: 11.0, Measured: true, MaxMegapixels: 2.0, LoraLoader: "LoraLoaderBypassModelOnly", Packs: []string{"multigpu"}},
	{ID: "qwen-image-edit-2511/gguf-q5_1", Route: "edit", Family: "qwen-image-edit-2511", Quant: "gguf-q5_1", UnetMatch: `Q5_1\.gguf$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "gguf-distorch2", ResidentGiB: 3, ClipLoader: "plain", ComputeGiB: 4.9, StorageGiB: 12.6, MaxMegapixels: 2.0, LoraLoader: "LoraLoaderModelOnly", Packs: []string{"multigpu", "gguf"}},
}

// multicardMatchers are the rows' regular expressions, compiled once, in the rows' order.
var multicardMatchers = func() []struct{ unet, clip *regexp.Regexp } {
	out := make([]struct{ unet, clip *regexp.Regexp }, len(multicardRows))
	for i, r := range multicardRows {
		out[i].unet = regexp.MustCompile("(?i)" + r.UnetMatch)
		out[i].clip = regexp.MustCompile("(?i)" + r.ClipMatch)
	}
	return out
}()

// bannedTextEncoder: an int8 text encoder is never placed (render/multicard-recipes.mjs: PSNR 28.65 dB against the 30 dB gate), so a call naming one has no recipe.
var bannedTextEncoder = regexp.MustCompile(`(?i)int8|int4|nf4`)

// MulticardRows is a copy of the recipe table, for the tests that pin it to the runner's file and for the docs.
func MulticardRows() []MulticardRow {
	out := make([]MulticardRow, len(multicardRows))
	for i, r := range multicardRows {
		r.Packs = append([]string(nil), r.Packs...)
		out[i] = r
	}
	return out
}

// FindMulticardRow finds the recipe for a call: the route ("image" or "edit"), the graph family, the DiT file and the text-encoder file (the binding's own,
// else the builder's default), taken from the call's model files. nil = no recipe; the runner refuses such a call by name, and everything else sizes it as it
// always did.
func FindMulticardRow(route, family string, files []ModelFile) *MulticardRow {
	var unet, clip string
	for _, mf := range files {
		switch mf.Role {
		case RoleDiffusion:
			if unet == "" {
				unet = mf.Name
			}
		case RoleTextEncoder:
			if clip == "" {
				clip = mf.Name
			}
		}
	}
	if unet == "" || bannedTextEncoder.MatchString(clip) {
		return nil
	}
	for i := range multicardRows {
		r := multicardRows[i]
		if r.Route != route || r.Family != family || !multicardMatchers[i].unet.MatchString(unet) {
			continue
		}
		want := clip
		if want == "" {
			want = r.DefaultClip
		}
		if want == "" || !multicardMatchers[i].clip.MatchString(want) {
			return nil
		}
		r.Packs = append([]string(nil), r.Packs...)
		return &r
	}
	return nil
}

// NodeClasses are the node classes the runner's graph names for this recipe's loaders (render/multicard-recipes.mjs multicardLoaders): the DiT's,
// the text encoder's and the VAE's. All of them come from ComfyUI-MultiGPU, and the GGUF DisTorch2 loader needs ComfyUI-GGUF beside it.
func (r MulticardRow) NodeClasses() []string {
	unet := map[string]string{"plain": "UNETLoaderMultiGPU", "distorch2": "UNETLoaderDisTorch2MultiGPU", "gguf-distorch2": "UnetLoaderGGUFDisTorch2MultiGPU"}[r.UnetLoader]
	clip := map[string]string{"plain": "CLIPLoaderMultiGPU", "distorch2": "CLIPLoaderDisTorch2MultiGPU"}[r.ClipLoader]
	return []string{unet, clip, "VAELoaderMultiGPU"}
}
