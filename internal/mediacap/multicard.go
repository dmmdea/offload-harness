package mediacap

import "regexp"

// THE CARD-ONLY RECIPES OF THE IMAGE AND EDIT ROUTES (--placement multicard on render/comfy-generate.mjs, comfy-render.mjs, comfy-edit.mjs).
//
// render/multicard-recipes.json is the runner's file: which loader holds which weight on which card, the DiT's resident share and the per-card VRAM
// peak a call is held to. This is the Go copy of the part the harness needs to decide with: which call has a recipe (the doctor's node classes,
// internal/hostneed's host-RAM declaration) and what it was measured to hold. TestTheMulticardRowsAreTheRunnersRecipes fails when either side moves.
//
// Measured is true only for a row whose host growth was read in a run that used the read-only safetensors loader (a card-only run that charged no
// weight file to host commit). The 2511 fp8mixed edit, the 2512 Q5_1 GGUF, the krea2 bf16 DiT, the qwen-image-2.1 bf16 and int8_convrot DiTs and the
// qwen-image-2.1 bf16 edit are measured; the 2511 Q5_1 GGUF edit and the qwen-image-2.1 int8_convrot edit rows are predicted. A live run with the read-only loader promotes a row: flip Measured here and "basis" in the JSON.
//
// A row can be held back (Blocked): it stays in the table, but it is no recipe to FindMulticardRow. No row is held back today. The qwen-image-2.1 rows'
// 16.33 GiB bf16 text encoder is bigger than a card, and under --gpu-only ComfyUI would build the whole of it on one card before a DisTorch2 split
// applies; the "distorch2-host" clip loader is the harness's own node that builds it off the cards and places it block by block. The 2.1 rows launch
// compute-first (the JSON's "cardOrder"; cuda:0 is the compute card) because the 2.1 model's fused and int8 kernels run on cuda:0 only.

// MulticardRow is one recipe: the identity of the call it serves, how the runner loads it and the figures that decide a declaration.
type MulticardRow struct {
	ID, Route, Family, Quant string
	// UnetMatch and ClipMatch are case-insensitive regular expressions on the DiT file and the text-encoder file; DefaultClip stands in for a call
	// that names no text encoder (the builder's own).
	UnetMatch, ClipMatch, DefaultClip string
	// UnetLoader is plain (the whole DiT on the compute card), distorch2 or gguf-distorch2 (a split DiT); ClipLoader is plain (the encoder whole on
	// the storage card), distorch2 (a split encoder that fits one card while it is built) or distorch2-host (a bf16 encoder too big for one card, built off
	// the cards and split over both by the harness's own loader).
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
	// For the two qwen-image-2.1 edit rows it is the DiT-sequence cap, judged by the runner alone (render/comfy-edit.mjs planEdit); the typed door's forCanvas is image-only.
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
		UnetLoader: "plain", ClipLoader: "distorch2-host", ClipResidentGiB: 5, ComputeGiB: 12.3, StorageGiB: 5.9, Measured: true, MaxPixels: 4194304, Packs: []string{"multigpu"}},
	{ID: "qwen-image-2.1/bf16", Route: "image", Family: "qwen-image-2.1", Quant: "bf16", UnetMatch: `bf16.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "distorch2", ResidentGiB: 8, ClipLoader: "distorch2-host", ClipResidentGiB: 8, ComputeGiB: 10.9, StorageGiB: 8.2, Measured: true, MaxPixels: 4194304, Packs: []string{"multigpu"}},
	{ID: "qwen-image/gguf-q5_1", Route: "image", Family: "qwen-image", Quant: "gguf-q5_1", UnetMatch: `Q5_1\.gguf$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "gguf-distorch2", ResidentGiB: 10, ClipLoader: "plain", ComputeGiB: 11.9, StorageGiB: 8.3, Measured: true, MaxPixels: 1769472, LoraLoader: "LoraLoaderModelOnly", Packs: []string{"multigpu", "gguf"}},
	{ID: "krea2/bf16", Route: "image", Family: "krea2", Quant: "bf16", UnetMatch: `bf16.*\.safetensors$`, ClipMatch: "bf16", DefaultClip: "qwen3vl_4b_bf16.safetensors",
		UnetLoader: "distorch2", ResidentGiB: 11, ClipLoader: "plain", ComputeGiB: 13.2, StorageGiB: 13.1, Measured: true, MaxPixels: 1572864, Packs: []string{"multigpu"}},
	{ID: "flux3-klein/bf16", Route: "image", Family: "flux3-klein", Quant: "bf16", UnetMatch: `bf16.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "plain", ClipLoader: "distorch2-host", ClipResidentGiB: 6, ComputeGiB: 13.5, StorageGiB: 6.5, MaxPixels: 1572864, Packs: []string{"multigpu"}},
	{ID: "flux3-klein/nvfp4", Route: "image", Family: "flux3-klein", Quant: "nvfp4", UnetMatch: `nvfp4.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "plain", ClipLoader: "distorch2-host", ClipResidentGiB: 6, ComputeGiB: 12.5, StorageGiB: 6.5, MaxPixels: 1572864, Packs: []string{"multigpu"}},
	{ID: "qwen-image-edit-2511/fp8mixed", Route: "edit", Family: "qwen-image-edit-2511", Quant: "fp8mixed", UnetMatch: `fp8mixed.*\.safetensors$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "distorch2", ResidentGiB: 9, ClipLoader: "plain", ComputeGiB: 13.0, StorageGiB: 11.0, Measured: true, MaxMegapixels: 2.0, LoraLoader: "LoraLoaderBypassModelOnly", Packs: []string{"multigpu"}},
	{ID: "qwen-image-edit-2511/gguf-q5_1", Route: "edit", Family: "qwen-image-edit-2511", Quant: "gguf-q5_1", UnetMatch: `Q5_1\.gguf$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		UnetLoader: "gguf-distorch2", ResidentGiB: 3, ClipLoader: "plain", ComputeGiB: 5.6, StorageGiB: 12.6, MaxMegapixels: 2.0, LoraLoader: "LoraLoaderModelOnly", Packs: []string{"multigpu", "gguf"}},
	{ID: "qwen-image-2.1-edit/int8_convrot", Route: "edit", Family: "qwen-image-2.1", Quant: "int8_convrot", UnetMatch: `int8.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "plain", ClipLoader: "distorch2-host", ClipResidentGiB: 5, ComputeGiB: 13.9, StorageGiB: 8.0, MaxPixels: 4194304, Packs: []string{"multigpu"}},
	{ID: "qwen-image-2.1-edit/bf16", Route: "edit", Family: "qwen-image-2.1", Quant: "bf16", UnetMatch: `bf16.*\.safetensors$`, ClipMatch: "bf16",
		UnetLoader: "distorch2", ResidentGiB: 8, ClipLoader: "distorch2-host", ClipResidentGiB: 8, ComputeGiB: 14.5, StorageGiB: 11.0, Measured: true, MaxPixels: 4194304, Packs: []string{"multigpu"}},
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
// the text encoder's and the VAE's. All of them come from ComfyUI-MultiGPU, and the GGUF DisTorch2 loader needs ComfyUI-GGUF beside it. A distorch2-host
// text encoder is the runner's OffloadCLIPLoaderDisTorch2, a class of the harness's own pack (never in custom_nodes: the launch the runner makes loads it, and its
// preflight asks for it by name); it wraps ComfyUI-MultiGPU's CLIPLoaderDisTorch2MultiGPU, so that is the class named here for the doctor to check.
func (r MulticardRow) NodeClasses() []string {
	unet := map[string]string{"plain": "UNETLoaderMultiGPU", "distorch2": "UNETLoaderDisTorch2MultiGPU", "gguf-distorch2": "UnetLoaderGGUFDisTorch2MultiGPU"}[r.UnetLoader]
	clip := map[string]string{"plain": "CLIPLoaderMultiGPU", "distorch2": "CLIPLoaderDisTorch2MultiGPU", "distorch2-host": "CLIPLoaderDisTorch2MultiGPU"}[r.ClipLoader]
	return []string{unet, clip, "VAELoaderMultiGPU"}
}
