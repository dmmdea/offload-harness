package pipeline

// THE CARD-ONLY PLACEMENT AT THE TYPED DOORS (imagegen_placement / gen_edit_placement: multicard; render/multicard-recipes.json, ADR 0039 amendment).
//
// A binding that asks for it runs on TWO non-display cards at once: the text encoder and the VAE on one, the DiT on the other (or split between them),
// through the MultiGPU loaders, with every weight file mapped read-only. The runner (render/multicard-image.mjs) owns the placement and the ComfyUI
// instance of the pair; the door's job is to lease the cards and say which. So the need takes the box's two non-display cards, as a SET:
//
//   - the plan is NAMED, never allocated: the pair is the box's two non-display cards, or the call is refused with the reason. The display card is never in it,
//     with the operator at the desk or away (the allocator's away-operator rule is for a one-card call), and a card whose display state the driver did not
//     report is not counted (unknown is never "not the screen");
//   - it is queued like any lease: a held card of the pair is a place in line (the token names the pair), never a one-card run;
//   - it is NEVER degraded to the whole node or to the default instance: a host that does not lease cards, an unreadable card table and a box without exactly
//     two non-display cards each get an answer that says so and runs nothing, because the runner refuses a call that names no cards, every time, after the
//     whole node was held for it. The answer is gpu_lease_unavailable, which a router may place elsewhere;
//   - a binding with no recipe (render/multicard-recipes.json; the runner refuses it by name) is refused before any lease is spent;
//   - the host RAM it declares is sized on the PAIR (internal/hostneed estimatePlaced): 0 only for a measured recipe whose per-card peaks fit the two cards,
//     else the ordinary streaming figure;
//   - the runner takes no per-card instance and none of the single-card launch settings (the pin, dynamic VRAM): the pair has an instance of its own, keyed
//     by the two cards, and runs --gpu-only.
//
// Under a parent lease (`local-offload gpu reserve --devices <a>,<b> -- <command>`) the call runs inside it on both cards, and only if the lease holds exactly
// two non-display cards; the in-process slots arbitrate the pair, as they do for a single card.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// errMulticard is the refusal of a call that asked for the card-only placement and cannot have it. Nothing ran and no other job is in the way, so it is
// classed as a lease that could not be taken (gpu_lease_unavailable, deferForLease).
type errMulticard struct{ why string }

func (e *errMulticard) Error() string { return e.why }

// gpuReserveHint is the way out the refusals share: lease the two cards by hand and run the call inside the lease.
const gpuReserveHint = "lease the two you mean with `local-offload gpu reserve --devices <card>,<card> -- <command>`"

// multicardNeed is the need of a binding that asks for the card-only placement on route (image or edit).
func multicardNeed(route hostneed.Route, cfg config.Config, token string) mediaNeed {
	n := mediaNeed{Kind: needMulticard, Token: token, RAMPair: multicardRAM(route, cfg)}
	var family string
	var files []mediacap.ModelFile
	switch route {
	case hostneed.RouteEdit:
		n.PlacementKey = "gen_edit_placement"
		if family = strings.TrimSpace(cfg.GenEditFamily); family == "" {
			family = config.EditFamily2511
		}
		files = mediacap.EditModelFiles(cfg)
	default:
		n.PlacementKey = "imagegen_placement"
		if family = strings.TrimSpace(cfg.ImageGenFamily); family == "" {
			family = "sdxl"
		}
		files = mediacap.ImageModelFiles(cfg)
	}
	if mediacap.FindMulticardRow(string(route), family, files) == nil {
		n.Refusal = noRecipeRefusal(n.PlacementKey, string(route), family, files)
	}
	return n
}

// noRecipeRefusal is the sentence for a binding the recipe table has no row for.
func noRecipeRefusal(key, route, family string, files []mediacap.ModelFile) string {
	var have []string
	for _, f := range files {
		have = append(have, fmt.Sprintf("%s %q", f.Label, f.Name))
	}
	binding := fmt.Sprintf("family %q", family)
	if len(have) > 0 {
		binding += ", " + strings.Join(have, ", ")
	}
	var rows []string
	for _, r := range mediacap.MulticardRows() {
		if r.Route == route {
			rows = append(rows, r.ID)
		}
	}
	return fmt.Sprintf("%s is multicard, but this binding (%s) has no card-only recipe (render/multicard-recipes.json has %s): the runner would refuse it by name after the pair was held, so nothing was leased; unset %s or bind a family and files that have a recipe",
		key, binding, strings.Join(rows, ", "), key)
}

// multicardRAM resolves a card-only call's host RAM once the cards of its lease are known: the recipe's verdict on those two cards, else the ordinary
// estimate (hostneed.estimatePlaced). The facts the recipe reads that the pipeline holds are the edit canvas the binding pins and the placement itself.
func multicardRAM(route hostneed.Route, cfg config.Config) func(vramGiB float64, cardsGiB []float64) hostneed.Need {
	return func(vram float64, cardsGiB []float64) hostneed.Need {
		f := hostneed.Facts{Cfg: cfg, VRAMGiB: vram, CardsGiB: cardsGiB, Placement: hostneed.PlacementMulticard}
		if route == hostneed.RouteEdit {
			f.Megapixels = cfg.GenEditMegapixels
		}
		if n, ok := hostneed.ForRoute(route, cfg, f); ok {
			return n
		}
		return hostneed.ClassDefault(f)
	}
}

// multicardLaunch is the launch profile of a card-only call: the single-card pin (comfy_cuda_device) and dynamic VRAM contradict two cards under
// --gpu-only, so neither reaches the runner (which would drop them for the launch anyway); the operator's extra args are theirs and stay.
func multicardLaunch(cfg config.Config) imagegen.ComfyLaunch {
	l := comfyLaunch(cfg, false)
	l.DynamicVRAM = ""
	return l
}

// nonDisplayCards are the cards the card-only placement may use, by lease id: not the operator's screen and not a card whose display state is unknown.
// uncounted is how many cards were left out because the driver did not report whether they drive a display.
func nonDisplayCards(cards []gpuprobe.Card) (ids []string, uncounted int) {
	for _, c := range cards {
		switch {
		case c.DisplayUnknown:
			uncounted++
		case !c.Display:
			ids = append(ids, c.LeaseID())
		}
	}
	sort.Strings(ids)
	return ids, uncounted
}

// planMulticard is planMedia for a card-only need: the box's two non-display cards, named, or the reason there is no plan.
func planMulticard(need mediaNeed, cards []gpuprobe.Card, tableErr error) (mediaPlan, error) {
	key := need.PlacementKey
	if tableErr != nil || len(cards) == 0 {
		cause := "it listed no card"
		if tableErr != nil {
			cause = tableErr.Error()
		}
		return mediaPlan{}, &errMulticard{fmt.Sprintf("%s is multicard, which needs the card table to name its two non-display cards, and the card table could not be read (%s): the call does not run on the whole node, where the runner would refuse it", key, cause)}
	}
	ids, uncounted := nonDisplayCards(cards)
	if len(ids) != 2 {
		have := "none"
		if len(ids) > 0 {
			have = strings.Join(ids, ", ")
		}
		note := ""
		if uncounted > 0 {
			note = fmt.Sprintf("; %d card(s) whose display state the driver did not report are not counted", uncounted)
		}
		return mediaPlan{}, &errMulticard{fmt.Sprintf("%s is multicard, which runs on exactly two non-display cards, and this box has %d (%s)%s: %s, or unset %s", key, len(ids), have, note, gpuReserveHint, key)}
	}
	return mediaPlan{ids: ids}, nil
}

// noCardLeases is the answer of a card-only call on a host that cannot lease cards: no lease can name the two cards, and the whole-node lease the call
// would otherwise take names none.
func noCardLeases(need mediaNeed, openErr error) error {
	key := need.PlacementKey
	if openErr != nil {
		return &errMulticard{fmt.Sprintf("%s is multicard, which needs card-scoped leases to name its two cards, and the lease directory cannot be opened: %v", key, openErr)}
	}
	return &errMulticard{fmt.Sprintf("%s is multicard but this host does not lease cards (gpu_card_scoped_leases is off, or its reader audit is not green), so no lease can name the two cards the placement runs on and the runner would refuse a whole-node call: turn card-scoped leases on after a green reader audit, %s, or unset %s", key, gpuReserveHint, key)}
}

// acquireInheritedPair serves a card-only call that runs under a lease its parent holds (`gpu reserve --devices <a>,<b> -- <command>`; held is
// GPU_LEASE_DEVICES). The lease is not ours to renew or release. It must hold exactly two non-display cards of this box; anything else is refused with
// the reason, never run on one card or on the whole node. The in-process slots take both cards, so two calls of this process do not share the pair.
func (p *Pipeline) acquireInheritedPair(ctx context.Context, inherited, held []string, wait time.Duration, need mediaNeed) (mediaGrant, bool, string, error) {
	key := need.PlacementKey
	cards, _, terr := p.alloc.CardTable(ctx, p.cfg)
	if terr != nil || len(cards) == 0 {
		_, err := planMulticard(need, cards, terr)
		return mediaGrant{}, true, "", err
	}
	eligible, _ := nonDisplayCards(cards)
	ids := append([]string(nil), held...)
	sort.Strings(ids)
	ok := len(ids) == 2 && ids[0] != ids[1]
	for _, id := range ids {
		if !containsString(eligible, id) {
			ok = false
		}
	}
	if !ok {
		holds := "the whole node"
		if len(held) > 0 {
			holds = strings.Join(held, ", ")
		}
		return mediaGrant{}, true, "", &errMulticard{fmt.Sprintf("%s is multicard, which runs on exactly two non-display cards, and this process runs under a lease on %s: %s, or unset %s", key, holds, gpuReserveHint, key)}
	}
	if !mediaSlots.take(ids, wait) {
		return mediaGrant{}, true, "", &errGPUBusy{detail: fmt.Sprintf("the two cards of the lease this process runs under (%s) are not both free of other jobs here after %s", strings.Join(ids, ", "), wait)}
	}
	core.MarkWorking(ctx)
	var once sync.Once
	return mediaGrant{
		Env:     append(append([]string(nil), inherited...), append([]string{"GPU_LEASE_DEVICES=" + strings.Join(held, ",")}, p.lockEnv()...)...),
		Release: func() { once.Do(func() { mediaSlots.release(ids) }) },
	}, true, "", nil
}
