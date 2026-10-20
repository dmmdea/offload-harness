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
//
// A row can be held back (Blocked): it stays in the table, but it is no recipe to FindMulticardRow. Both qwen-image-2.1 rows are held back today:
// int8_convrot (its DiT on cuda:1 never ran) and bf16 (ComfyUI builds the whole 16.33 GiB text encoder on the 15.9 GiB storage card before the split
// applies, and a live card-only run ran out of memory there), so the launchable image rows are the 2512 Q5_1 GGUF and the krea2 bf16 DiT.

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
	// MaxMegapixels is the largest edit canvas the compute peak is scaled to (0 = the row has none). The runner refuses a pinned canvas above it
	// (render/comfy-edit.mjs planEdit), and so does the typed door before it leases the pair (internal/pipeline multicardNeed).
	MaxMegapixels float64
	// MaxPixels is the largest canvas (width times height) a call on this row may ask for (0 = the row has none). It is the JSON's "maxPixels", pinned by
	// TestTheMulticardRowsAreTheRunnersRecipes, and the number is chosen, not measured (the JSON's "maxPixelsNote"). The runner refuses a larger canvas
	// (render/multicard-recipes.mjs canvasRefusal) only after the pair was held; the typed door refuses it first (internal/pipeline mediaNeed.forCanvas).
	MaxPixels int64
	// Blocked is why the row may not launch (empty = it may): the row stays in the table, but it is no recipe to FindMulticardRow, so the estimate, the
	// doctor and the typed doors treat the binding as one without a recipe, and the runner refuses it by name (render/multicard-recipes.mjs findRecipe).
	// The sentence is the JSON's "blocked", pinned by TestTheMulticardRowsAreTheRunnersRecipes.
	Blocked string
	// LoraLoader is the node class that applies a LoRA to the DiT under this recipe ("" = the recipe has none to name).
	LoraLoader string
	// Packs are the custom-node packs the recipe's loaders need: multigpu, and gguf for a GGUF DiT.
	Packs []string
}

// Plain reports a recipe that puts the whole DiT on the compute card (no resident share to set).
func (r MulticardRow) Plain() bool { return r.UnetLoader == "plain" }

var multicardRows = []MulticardRow{
	{ID: "qwen-image-2.1/int8_convrot", Route: "image", Family: "qwen-image-2.1", Quant: "int8_convrot", UnetMatch: `int8.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "plain", ClipLoader: "distorch2", ClipResidentGiB: 5, ComputeGiB: 9.0, StorageGiB: 13.6, Packs: []string{"multigpu"},
		Blocked: "the card-only placement puts this recipe's int8 DiT on cuda:1, and no run has: the benchmark arms the row cites ran the int8 DiT on cuda:0 with the bf16 text encoder on cuda:1, the reverse. A comfy_kitchen int8 model run through ComfyUI-MultiGPU with the compute device off cuda:0 is a recorded illegal memory access in the int8 dispatch path (upstream issue 220, unfixed; it is why the pooled LTX-2.5 seat keeps compute cuda:0). Run one int8 arm with the DiT on cuda:1, then clear \"blocked\" on this row in render/multicard-recipes.json and Blocked in internal/mediacap/multicard.go; the bf16 DiT row of this family is held back too (the 16.33 GiB bf16 text encoder this row also names cannot be built on the storage card), so lifting this row needs that fixed as well, and no qwen-image-2.1 file has a card-only recipe today"},
	{ID: "qwen-image-2.1/bf16", Route: "image", Family: "qwen-image-2.1", Quant: "bf16", UnetMatch: `bf16.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "distorch2", ResidentGiB: 8, ClipLoader: "distorch2", ClipResidentGiB: 5, ComputeGiB: 10.8, StorageGiB: 14.2, Packs: []string{"multigpu"},
		Blocked: "under --gpu-only ComfyUI builds the whole 16.33 GiB bf16 text encoder on the storage card (cuda:0, a 15.9 GiB card) before the DisTorch2 allocation that splits it is applied, so a live card-only run of this recipe ran out of memory 1.4 s in, with 14.10 GiB allocated, while the encoder was still being built; no resident share or peak figure in this row changes that. A loader that builds the text encoder per card, or one measured arm that fits the pair, lifts it: then clear blocked on this row in render/multicard-recipes.json and Blocked in internal/mediacap/multicard.go"},
	{ID: "qwen-image/gguf-q5_1", Route: "image", Family: "qwen-image", Quant: "gguf-q5_1", UnetMatch: `Q5_1\.gguf$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "gguf-distorch2", ResidentGiB: 3, ClipLoader: "plain", ComputeGiB: 4.9, StorageGiB: 12.6, MaxPixels: 1472700, LoraLoader: "LoraLoaderModelOnly", Packs: []string{"multigpu", "gguf"}},
	{ID: "krea2/bf16", Route: "image", Family: "krea2", Quant: "bf16", UnetMatch: `bf16.*\.safetensors$`, ClipMatch: "bf16", DefaultClip: "qwen3vl_4b_bf16.safetensors",
		UnetLoader: "distorch2", ResidentGiB: 12, ClipLoader: "plain", ComputeGiB: 14.3, StorageGiB: 13.7, MaxPixels: 1572864, Packs: []string{"multigpu"}},
	{ID: "qwen-image-edit-2511/fp8mixed", Route: "edit", Family: "qwen-image-edit-2511", Quant: "fp8mixed", UnetMatch: `fp8mixed.*\.safetensors$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "distorch2", ResidentGiB: 9, ClipLoader: "plain", ComputeGiB: 13.0, StorageGiB: 11.0, Measured: true, MaxMegapixels: 2.0, LoraLoader: "LoraLoaderBypassModelOnly", Packs: []string{"multigpu"}},
	{ID: "qwen-image-edit-2511/gguf-q5_1", Route: "edit", Family: "qwen-image-edit-2511", Quant: "gguf-q5_1", UnetMatch: `Q5_1\.gguf$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "gguf-distorch2", ResidentGiB: 3, ClipLoader: "plain", ComputeGiB: 5.6, StorageGiB: 12.6, MaxMegapixels: 2.0, LoraLoader: "LoraLoaderModelOnly", Packs: []string{"multigpu", "gguf"}},
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
// else the builder's default), taken from the call's model files. nil = no recipe (a row that is held back is no recipe); the runner refuses such a call by
// name, and everything else sizes it as it always did.
func FindMulticardRow(route, family string, files []ModelFile) *MulticardRow {
	if r := matchMulticardRow(route, family, files); r != nil && r.Blocked == "" {
		return r
	}
	return nil
}

// BlockedMulticardRow is the row a call matches when that row is held back (Blocked), else nil: a call with no row at all, or a launchable one, has none.
// The typed door uses it to say which row and why when it refuses the binding.
func BlockedMulticardRow(route, family string, files []ModelFile) *MulticardRow {
	if r := matchMulticardRow(route, family, files); r != nil && r.Blocked != "" {
		return r
	}
	return nil
}

// matchMulticardRow is the row of the call's files whether or not it is held back.
func matchMulticardRow(route, family string, files []ModelFile) *MulticardRow {
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
