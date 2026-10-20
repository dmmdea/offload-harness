package hostneed

import (
	"fmt"
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
//  2. the row's per-card peaks fitting the TWO cards the lease holds, each less the runner's --reserve-vram: the larger card computes, the other stores
//     (within 1 percent of one size the runner gives the role to the faster board instead, render/wan-multicard.mjs chooseRoles, so the card that
//     computes may be the one up to 1 percent smaller; the compute stage is judged here against the larger card, so a row that fits by less than
//     that 1 percent can be declared 0 here and refused by the runner's peak check, before ComfyUI starts; WAN_COMPUTE_UUID can name the smaller card of any pair, and
//     then that gap is not bounded: the runner's resident and peak checks refuse what the named card cannot hold, again before ComfyUI starts).
//
// The figures are render/multicard-recipes.json's, copied into internal/mediacap/multicard.go (which also finds the recipe of a call);
// mediacap's TestTheMulticardRowsAreTheRunnersRecipes fails when either side moves.

// PlacementMulticard is the value of --placement (and of imagegen_placement / gen_edit_placement) that asks for the card-only placement.
const PlacementMulticard = "multicard"

// estimatePlaced is estimate for a route that may ask for the card-only placement (image and edit). Without the placement, or with a call that has no
// recipe, it is estimate exactly. With one it adds what the recipe says: a measured row whose peaks fit the two leased cards declares 0, and
// anything else keeps the ordinary figure and says why.
func estimatePlaced(route Route, family string, files []mediacap.ModelFile, f Facts) (Need, bool) {
	n, ok := estimate(family, files, f)
	if !ok || !strings.EqualFold(strings.TrimSpace(f.Placement), PlacementMulticard) {
		return n, ok
	}
	row := mediacap.FindMulticardRow(string(route), family, files)
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
	if f.ResidentGiB != nil && !row.Plain() {
		// The DiT's total is the invariant: the allocation string keeps --resident-gb GiB of it on the compute card and sends the REMAINDER to the
		// storage card. A larger share raises the compute peak; a smaller one puts the same bytes on the storage card, which is the card whose
		// room is thinner (a 5 GiB share of the 19.1 GiB fp8mixed DiT is 15.0 GiB there, and a whole DiT on one card ran out of memory at load). The
		// storage figure is never LOWERED by a larger share: no run measured that, so the row's own figure stands. For a row whose storage peak is
		// its text-encoder stage (the 2.1 bf16 one), a smaller share is refused somewhat early; that is the conservative side.
		shift := *f.ResidentGiB - row.ResidentGiB
		compute += shift
		if shift < 0 {
			storage -= shift
		}
	}
	fits := small > 0 && compute <= large-reserve && storage <= small-reserve
	peaks := fmt.Sprintf("the compute card peaks at %.1f GiB (of %.1f usable) and the storage card at %.1f GiB (of %.1f usable)", compute, large-reserve, storage, small-reserve)
	if !fits {
		return note(fmt.Sprintf("the card-only recipe %s does not fit the two leased cards (%.1f + %.1f GiB): %s", row.ID, small, large, peaks))
	}
	if !row.Measured {
		return note(fmt.Sprintf("the card-only recipe %s fits the two leased cards (%.1f + %.1f GiB): %s; but its host growth is predicted, not measured with the read-only loader, so the call declares the set until a run has measured it (render/multicard-recipes.json, basis predicted)", row.ID, small, large, peaks))
	}
	return Need{GiB: 0, Source: n.Source, CardOnly: true, Detail: fmt.Sprintf("%s: the card-only placement (--placement multicard, recipe %s) on 2 leased cards (%.1f + %.1f GiB): %s, measured with the read-only safetensors loader, so no weights stream from RAM", family, row.ID, small, large, peaks)}, true
}
