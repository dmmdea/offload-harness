package hostneed

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// THE CARD-ONLY IMAGE AND EDIT PLACEMENT (--placement multicard on render/comfy-generate.mjs, comfy-render.mjs and comfy-edit.mjs).
//
// The runner (render/multicard-image.mjs) puts the text encoder and the VAE on one leased card and the DiT on the other, or splits the DiT
// between them, through the MultiGPU loaders, and maps every weight file read-only, so nothing streams from the host. Whether a CALL declares 0 host RAM
// takes two things, both:
//
//  1. a recipe whose host growth was MEASURED with the read-only loader. Only the 2511 fp8mixed edit has one (benchmark 2026-10-11, section 4.6:
//     1.87 GiB of growth over ComfyUI's idle). Every image arm of that benchmark ran the stock loader, which charges each weight file to host commit,
//     so the other rows are PREDICTED, and a predicted row says so and declares the ordinary estimate (the honest streaming figure: it errs toward
//     waiting for the host's room, never toward admitting on a guess). A live run with the read-only loader promotes a row: flip Measured here and
//     "basis" in render/multicard-recipes.json (a test pins the two together).
//  2. the row's per-card peaks fitting the TWO cards the lease holds, each less the runner's --reserve-vram: the larger card computes, the other stores.
//
// The figures are render/multicard-recipes.json's, copied; TestTheMulticardRowsAreTheRunnersRecipes fails when either side moves.

// PlacementMulticard is the value of --placement (and of imagegen_placement / gen_edit_placement) that asks for the card-only placement.
const PlacementMulticard = "multicard"

// MulticardRow is one recipe of the card-only placement and the figures that decide a declaration.
type MulticardRow struct {
	ID, Family, Quant string
	Route             Route
	// UnetMatch and ClipMatch are case-insensitive regular expressions on the DiT file and the text-encoder file; DefaultClip stands in for
	// a call that names no text encoder (the builder's own).
	UnetMatch, ClipMatch, DefaultClip string
	// Plain: the whole DiT sits on the compute card (no resident share to set). ResidentGiB is the share of a split DiT kept there.
	Plain       bool
	ResidentGiB float64
	// ComputeGiB and StorageGiB are the per-card VRAM peaks a call on this row is held to.
	ComputeGiB, StorageGiB float64
	// Measured: the row's host growth was read in a run that used the read-only loader.
	Measured bool
	// MaxMegapixels is the largest edit canvas the compute peak is scaled to (0 = the row has none).
	MaxMegapixels float64
}

var multicardRows = []MulticardRow{
	{ID: "qwen-image-2.1/int8_convrot", Route: RouteImage, Family: "qwen-image-2.1", Quant: "int8_convrot", UnetMatch: `int8.*\.safetensors$`, ClipMatch: "bf16", Plain: true, ComputeGiB: 9.0, StorageGiB: 13.6},
	{ID: "qwen-image-2.1/bf16", Route: RouteImage, Family: "qwen-image-2.1", Quant: "bf16", UnetMatch: `bf16.*\.safetensors$`, ClipMatch: "bf16", ResidentGiB: 8, ComputeGiB: 10.8, StorageGiB: 14.2},
	{ID: "qwen-image/gguf-q5_1", Route: RouteImage, Family: "qwen-image", Quant: "gguf-q5_1", UnetMatch: `Q5_1\.gguf$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors", ResidentGiB: 3, ComputeGiB: 4.9, StorageGiB: 12.6},
	{ID: "qwen-image-edit-2511/fp8mixed", Route: RouteEdit, Family: "qwen-image-edit-2511", Quant: "fp8mixed", UnetMatch: `fp8mixed.*\.safetensors$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors", ResidentGiB: 9, ComputeGiB: 13.0, StorageGiB: 11.0, Measured: true, MaxMegapixels: 2.0},
	{ID: "qwen-image-edit-2511/gguf-q5_1", Route: RouteEdit, Family: "qwen-image-edit-2511", Quant: "gguf-q5_1", UnetMatch: `Q5_1\.gguf$`, ClipMatch: "fp8", DefaultClip: "qwen_2.5_vl_7b_fp8_scaled.safetensors", ResidentGiB: 3, ComputeGiB: 4.9, StorageGiB: 12.6, MaxMegapixels: 2.0},
}

// bannedTextEncoder: an int8 text encoder is never placed (render/multicard-recipes.mjs), so a call naming one has no recipe.
var bannedTextEncoder = regexp.MustCompile(`(?i)int8|int4|nf4`)

// MulticardRows is a copy of the recipe table, for the tests that pin it to the runner's file and for the docs.
func MulticardRows() []MulticardRow { return append([]MulticardRow(nil), multicardRows...) }

// multicardRow finds the recipe for a call: the route and family, the DiT file and the text-encoder file (the binding's own, else the builder's
// default). nil = no recipe; the runner refuses such a call by name, and this package sizes it as it always did.
func multicardRow(route Route, family string, files []mediacap.ModelFile) *MulticardRow {
	var unet, clip string
	for _, mf := range files {
		switch mf.Role {
		case mediacap.RoleDiffusion:
			if unet == "" {
				unet = mf.Name
			}
		case mediacap.RoleTextEncoder:
			if clip == "" {
				clip = mf.Name
			}
		}
	}
	if unet == "" || bannedTextEncoder.MatchString(clip) {
		return nil
	}
	for i := range multicardRows {
		r := &multicardRows[i]
		if r.Route != route || r.Family != family || !regexp.MustCompile("(?i)"+r.UnetMatch).MatchString(unet) {
			continue
		}
		want := clip
		if want == "" {
			want = r.DefaultClip
		}
		if want == "" || !regexp.MustCompile("(?i)"+r.ClipMatch).MatchString(want) {
			return nil
		}
		return r
	}
	return nil
}

// estimatePlaced is estimate for a route that may ask for the card-only placement (image and edit). Without the placement, or with a call that has no
// recipe, it is estimate exactly. With one it adds what the recipe says: a measured row whose peaks fit the two leased cards declares 0, and
// anything else keeps the ordinary figure and says why.
func estimatePlaced(route Route, family string, files []mediacap.ModelFile, f Facts) (Need, bool) {
	n, ok := estimate(family, files, f)
	if !ok || !strings.EqualFold(strings.TrimSpace(f.Placement), PlacementMulticard) {
		return n, ok
	}
	row := multicardRow(route, family, files)
	if row == nil {
		return n, ok
	}
	note := func(why string) (Need, bool) {
		n.Detail += "; " + why
		return n, true
	}
	if len(f.CardsGiB) != 2 {
		return note(fmt.Sprintf("--placement multicard asks for the card-only recipe %s, which runs on exactly two leased cards, and this lease reports %d", row.ID, len(f.CardsGiB)))
	}
	if row.MaxMegapixels > 0 {
		mp := f.Megapixels
		if mp > row.MaxMegapixels {
			return note(fmt.Sprintf("the card-only recipe %s is held to an edit canvas of at most %.1f megapixels and this call asks for %.1f", row.ID, row.MaxMegapixels, mp))
		}
	}
	small, large := f.CardsGiB[0], f.CardsGiB[1]
	if small > large {
		small, large = large, small
	}
	reserve := f.reserve()
	compute, storage := row.ComputeGiB, row.StorageGiB
	if f.ResidentGiB != nil && !row.Plain {
		compute += *f.ResidentGiB - row.ResidentGiB
	}
	fits := small > 0 && compute <= large-reserve && storage <= small-reserve
	peaks := fmt.Sprintf("the compute card peaks at %.1f GiB (of %.1f usable) and the storage card at %.1f GiB (of %.1f usable)", compute, large-reserve, storage, small-reserve)
	if !fits {
		return note(fmt.Sprintf("the card-only recipe %s does not fit the two leased cards (%.1f + %.1f GiB): %s", row.ID, small, large, peaks))
	}
	if !row.Measured {
		return note(fmt.Sprintf("the card-only recipe %s fits the two leased cards (%.1f + %.1f GiB): %s; but its host growth is predicted, not measured with the read-only loader, so the call declares the set until a run has measured it (render/multicard-recipes.json, basis predicted)", row.ID, small, large, peaks))
	}
	return Need{GiB: 0, Source: n.Source, Detail: fmt.Sprintf("%s: the card-only placement (--placement multicard, recipe %s) on 2 leased cards (%.1f + %.1f GiB): %s, measured with the read-only safetensors loader (1.9 GiB of host growth over the runtime's idle), so no weights stream from RAM", family, row.ID, small, large, peaks)}, true
}
