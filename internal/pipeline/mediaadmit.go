package pipeline

// Per-card media admission (plan P13).
//
// A generation call used to take the machine-wide media lease: one card or all of them, the
// same thing, because a lease named no cards. On a host that has turned card-scoped leases on
// (config gpu_card_scoped_leases, a green reader audit) a call now asks for what it needs:
//
//   - a single-card route (image, edit, inpaint, upscale, animate, music, un-pooled video) asks
//     the allocator for ONE card, holds a lease on it, and runs in the ComfyUI instance bound to
//     that card (its own port, directories and launch marker, pinned by GPU uuid). Two such calls
//     run on two cards at once; the display card is open to the allocator only up to the
//     operator's cap (gpu_display_cap_pct) and to a call that declares a VRAM footprint, which a
//     media call does not yet, so it is not auto-assigned to one today (which card is the display
//     card is the shared rule in gpuprobe, which reads display_attached as well as
//     display_active);
//   - an EXPLICIT pin (comfy_cuda_device) is a hard constraint, never re-picked: the call queues
//     for that card, FIFO, however many others are free. The pin is in ComfyUI's order, so it is
//     turned into a card through the order the box declares (gpu_comfy_order); an order that is
//     not declared, or a pin that names several cards, falls back to the whole-node lease and
//     today's `--cuda-device`, with one logged reason, never a guess;
//   - a POOLED route computes on cards its pool keys name (also ComfyUI order), through the
//     default instance with every card visible, so its launch is unchanged. It leases the
//     pool's cards when that is provably safe (see poolMayBeScoped) and otherwise the whole node;
//   - run-graph with ONE declared device runs in that card's own instance; with several, or none,
//     it holds the whole node (a graph in the default instance sees every card, so a lease on
//     some of them would not confine it);
//   - everything else (sdcpp, voice, a host with card-scoped leases off or no card table) takes
//     the whole-node lease exactly as before, byte for byte.
//
// A call that cannot get a card inside its window does not fail with "gpu busy". It leaves a
// place-keeping token (gpulease/tokens.go) and answers with it, its position and an ETA; the
// caller re-calls with waiter_token and resumes the place it left. A busy card is a place in
// line.
//
// WHY A KEYED INSTANCE FOR EVERY SINGLE-CARD JOB. ComfyUI's default instance (port 8188) is one
// process for the whole box: two jobs that both use it contend on its port, its output
// directory and its launch marker, and a second job whose launch profile differs is refused
// (COMFY-PROFILE-MISMATCH) while one that asks for none silently reuses the first one's
// instance, on the first one's cards. A job that holds only SOME of the box's cards therefore
// must not run in it. Single-card jobs get an instance per card; the jobs that DO use the
// default instance hold the whole node, or (pooled) cards that every other pooled job on a box
// of at most three cards also needs, so they cannot run beside each other either.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpualloc"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// mediaKind says how a call chooses its cards.
type mediaKind int

const (
	needWhole     mediaKind = iota // the whole node (and the default instance): the legacy lease
	needSingle                     // one card: the allocator's, or the explicit pin's
	needPooled                     // the cards a pool's keys name
	needDeclared                   // cards the operator declared (run-graph `devices`)
	needMulticard                  // the box's two non-display cards, leased as a set (imagegen_placement / gen_edit_placement: multicard)
)

// mediaNeed is what one media call asks of the GPU.
type mediaNeed struct {
	Kind mediaKind
	// Pin is comfy_cuda_device as configured (needSingle): "" = let the allocator choose.
	Pin string
	// Pool is the pool's ComfyUI device keys, "cuda:1" or "1" (needPooled).
	Pool []string
	// Devices are the cards the operator declared (needDeclared): nvidia-smi indices or UUID prefixes.
	Devices []string
	// Token is the waiter_token the caller got from an earlier queued answer ("" = a new arrival).
	Token string
	// Resumable says the call came through a door that can hand a place in line back and take it
	// again (core.Request.Resumable). Only then is a place kept for a call that waited its window
	// with no card; any other gets the plain "gpu busy" and leaves nothing behind, because a
	// token it can never claim would hold a card back from the next caller for the grace.
	Resumable bool
	// RAM resolves the host RAM this call declares once the card it will run on is known (the
	// estimate depends on whether its weights fit that card, internal/hostneed). nil declares
	// nothing: a route whose weights always sit on the card (upscale, voice) adds no host memory the
	// grant needs to see. The grant admits it against the available memory and the commit charge against physical RAM, the one figure taken for both and no commit stated (gpulease/hostram.go).
	RAM func(vramGiB float64) hostneed.Need
	// Foot is the footprint-store key this call's renders are recorded under (the key the same route's sampling
	// hook uses), so a declaration can be raised to what those renders were measured to hold (calibratedRAM). The
	// zero value is not calibrated; only the image and video routes set it.
	Foot footKey
	// RAMPair is RAM for a card-only call (needMulticard): it also reads the VRAM of EACH card of the lease, because the recipe's verdict is on the two
	// cards together. Set instead of RAM, never beside it.
	RAMPair func(vramGiB float64, cardsGiB []float64) hostneed.Need
	// PlacementKey is the config key that asked for the card-only placement (needMulticard), for the answers that tell the operator what to change.
	PlacementKey string
	// Refusal, when not empty, is why the call is refused before any lease is spent (a card-only binding with no recipe). The runner would refuse it
	// by name after the pair was held and the host's memory declared.
	Refusal string
	// Row is the card-only recipe the binding matched (needMulticard, nil = none), kept so forCanvas can hold the request's canvas to the row's cap.
	Row *mediacap.MulticardRow
}

// footKey is a footprint-store identity: family, quant, task.
type footKey struct{ family, quant, task string }

// withFootprint names the store key the call's renders are recorded under.
func (n mediaNeed) withFootprint(k footKey) mediaNeed {
	n.Foot = k
	return n
}

// hostPeakMinRuns is how many measured runs of a key must exist before a measurement may raise a declaration.
// Chosen 2026-10-10, not measured: a single run is one reading of a number that moves with the workflow, the
// cached families and the desktop, and three is the fewest that lets one outlier be seen against two others in the
// store's own records. A measurement only ever raises the declaration, so the number errs toward waiting for more
// data, not toward admitting on less.
const hostPeakMinRuns = 3

// hostPeakMaxRaise is the most a measurement may raise a declaration above the sum of the model files, as a multiple of it.
// Chosen 2026-10-10, not measured: the files are what must stream and the tree around them is the runtime's own baseline
// plus caches, so a tree that reads more than twice its files is an estimate that is wrong or a sampler that is charging
// card memory to the process (a driver does, on Windows), and either is a finding for the estimate and not a number to
// declare. Unclamped, a declaration above what the box can ever admit makes the lane read "can never admit" for every
// later call, and `gpu reserve --ram` is no remedy for a pipeline call (F56). The clamp is said in the declaration.
const hostPeakMaxRaise = 2.0

// calibratedRAM raises a declared need to what renders of the call's key were measured to hold: the footprint
// store's peak RESIDENT memory of the render's process tree, once at least hostPeakMinRuns runs exist, when that is
// higher than the estimate from the model files. Three properties are the point:
//
//   - It only RAISES. The sampled tree can miss memory (a kept ComfyUI reused from an earlier lease is not under
//     the runner), so a low reading says nothing about what must stream, and a measurement must never be what
//     waves a lane through that the files say needs RAM.
//   - It leaves 0 alone. A render whose weights fit its card streams nothing from RAM; the measured tree carries
//     the runtime's own baseline and, on a driver that charges card allocations to the process, the card's memory,
//     neither of which the guard is for.
//   - It is RESIDENT, not private, because a declaration is in RAM units and the touched half of the not-yet-loaded
//     sum subtracts resident memory from it (gpulease.descendantsHeld). The private peak is recorded beside it for
//     the commit question and not consumed here.
//
// The unit that is still open is whether a declaration should ALSO cover the card memory a render's commit
// carries; the commit term of the admission reads actual commit, so a lane already running is counted whole.
func (p *Pipeline) calibratedRAM(n hostneed.Need, k footKey) hostneed.Need {
	if n.GiB <= 0 || k.task == "" {
		return n
	}
	store := p.FootprintStore()
	if store == nil {
		return n
	}
	hp, ok := store.HostPeak(k.family, k.quant, k.task)
	if !ok || hp.Runs < hostPeakMinRuns || hp.ResidentGiB <= n.GiB {
		return n
	}
	// The figure is a high percentile of the last runs, not the all-time maximum (fleetnode.HostPeaks): one inflated run is
	// not the declaration. And the raise is clamped (hostPeakMaxRaise).
	declared, clamp := hp.ResidentGiB, ""
	if limit := n.GiB * hostPeakMaxRaise; declared > limit {
		declared = limit
		clamp = fmt.Sprintf("; clamped at %.0fx the files' sum (%.1f GiB): a tree that reads more than that is an estimate that is wrong or a sampler that counts card memory, not a need to declare", hostPeakMaxRaise, limit)
	}
	runs := hp.Window
	if runs == 0 {
		runs = hp.Runs // a store written before the window existed: its maxima over its runs
	}
	return hostneed.Need{GiB: declared, Source: hostneed.SourceMeasured,
		Detail: fmt.Sprintf("the resident memory of %s/%s renders measured %.1f GiB at the %dth percentile of the last %d runs (%d recorded, the largest %.1f GiB), above the %.1f GiB their model files add up to%s",
			k.family, k.task, hp.ResidentGiB, fleetnode.HostPeakPercentile, runs, hp.Runs, hp.MaxResidentGiB, n.GiB, clamp)}
}

// declaresRAM sets how the call's host RAM need is resolved.
func (n mediaNeed) declaresRAM(f func(vramGiB float64) hostneed.Need) mediaNeed {
	n.RAM = f
	return n
}

// routeRAM resolves a route's need from the effective binding it is about to render with. A binding
// whose files cannot be sized takes the media class default (this box's largest bound family), never
// a guess, and never zero: an unknown is not "fits".
func routeRAM(route hostneed.Route, cfg config.Config) func(float64) hostneed.Need {
	return func(vram float64) hostneed.Need {
		f := hostneed.Facts{Cfg: cfg, VRAMGiB: vram}
		if n, ok := hostneed.ForRoute(route, cfg, f); ok {
			return n
		}
		return hostneed.ClassDefault(f)
	}
}

// videoRAM is routeRAM for the family that will render a video request, with the weight files the
// request itself names (over) put over that family's binding: the runner loads those, so those are
// what the host is asked to hold.
func videoRAM(cfg config.Config, renderFamily string, over hostneed.VideoOverrides) func(float64) hostneed.Need {
	return func(vram float64) hostneed.Need {
		f := hostneed.Facts{Cfg: cfg, VRAMGiB: vram}
		if n, ok := hostneed.ForVideoWith(cfg, renderFamily, over, f); ok {
			return n
		}
		return hostneed.ClassDefault(f)
	}
}

// classDefaultRAM is for a call whose weights the pipeline cannot name (the audio.cpp lanes, a graph graphRAM cannot size): the media class
// default, the largest ComfyUI render family this box binds. A box that binds none (sd.cpp only) gets 0 (hostneed.ClassDefault says
// why). The sd.cpp lanes start from it and add the weights they park themselves (sdcppDeclared, F60).
func classDefaultRAM(cfg config.Config) func(float64) hostneed.Need {
	return func(vram float64) hostneed.Need {
		return hostneed.ClassDefault(hostneed.Facts{Cfg: cfg, VRAMGiB: vram})
	}
}

// declaredRAM is the host RAM a call declares on the card(s) it runs on: ids are lease ids (nil =
// the whole node), cards the card table (nil = read it here, best effort: a table that cannot be
// read leaves the card's VRAM unknown, and then nothing is assumed to fit; an empty, non-nil table
// is one the caller already tried and could not read, so it is not read again). Said once per
// distinct answer in the log, the way a fallback to the whole node is.
func (p *Pipeline) declaredRAM(ctx context.Context, need mediaNeed, cards []gpuprobe.Card, ids []string) float64 {
	if need.RAM == nil && need.RAMPair == nil {
		return 0
	}
	if cards == nil {
		if t, _, err := p.alloc.CardTable(ctx, p.cfg); err == nil {
			cards = t
		}
	}
	var declared hostneed.Need
	if need.RAMPair != nil {
		declared = need.RAMPair(hostneed.LargestCardGiB(cards, ids), hostneed.LeasedCardsGiB(cards, ids))
	} else {
		declared = need.RAM(hostneed.LargestCardGiB(cards, ids))
	}
	n := p.calibratedRAM(declared, need.Foot)
	if n.GiB > 0 || n.CardOnly { // a measured card-only 0 is said too: nothing corrects it afterwards, so the log is where a false one can be seen
		key := "declaring host RAM: " + n.String()
		if n.Source == hostneed.SourceMeasured {
			// A measured declaration's text carries the run counts, which move with every render: the note is made once
			// per key and figure, not once per render (one map entry and one log line per render, for the life of a
			// fleet-serve, was F56). A figure that moves is a new note.
			key = fmt.Sprintf("measured|%s|%s|%s|%.1f", need.Foot.family, need.Foot.quant, need.Foot.task, n.GiB)
		}
		planNoteKeyed(key, "declaring host RAM: "+n.String())
	}
	return n.GiB
}

// askRAMFor is the host RAM a call declares against the cards its PLAN names, for the readers that need the figure before the
// card is held: the allocator's pre-filter and the lane probe, and whether the call declares any at all, which decides whether a
// waiter that waits only on host RAM holds its cards against it (gpualloc.QueuedClaims, G6). A plan that names its cards (a
// pin, a pool, a declared device) is sized on THEM, as the lease the call then takes is (optsFor): sized on the largest card of
// the whole table a krea2 render pinned to a 16 GiB card declared nothing on a box that also has a 48 GiB one, and disagreed
// with its own grant (F58). A plan that does not name them (the whole node, an allocator pick that cannot be known yet) keeps
// the largest card on the box: the smallest answer, which can only err toward "nothing in the way".
func (p *Pipeline) askRAMFor(ctx context.Context, need mediaNeed, plan mediaPlan, cards []gpuprobe.Card) float64 {
	var ids []string
	if !plan.auto && !plan.whole {
		ids = plan.ids
	}
	return p.declaredRAM(ctx, need, cards, ids)
}

// resumableBy marks the need with whether the request's door can resume a place in line.
func (n mediaNeed) resumableBy(req core.Request) mediaNeed {
	n.Resumable = req.Resumable
	return n
}

// wholeNeed is a call that holds the whole node. token is the waiter_token it resumes, if any.
func wholeNeed(token string) mediaNeed { return mediaNeed{Kind: needWhole, Token: token} }

// singleCardNeed is a route that renders on one card. The binding's explicit pin, if any, is a
// hard constraint on which.
func singleCardNeed(cfg config.Config, token string) mediaNeed {
	return mediaNeed{Kind: needSingle, Pin: strings.ReplaceAll(cfg.ComfyCudaDevice, " ", ""), Token: token}
}

// pooledNeed is a pooled route: it computes on the cards its keys name.
func pooledNeed(token string, keys ...string) mediaNeed {
	return mediaNeed{Kind: needPooled, Pool: keys, Token: token}
}

// declaredNeed is a call whose operator named the cards (run-graph). No devices = the whole node.
func declaredNeed(devices []string, token string) mediaNeed {
	if len(devices) == 0 {
		return wholeNeed(token)
	}
	return mediaNeed{Kind: needDeclared, Devices: devices, Token: token}
}

// imageNeed is the need of the image-generation route (and its batch) for the resolved binding.
func imageNeed(cfg config.Config, token string) mediaNeed {
	fam, quant := imageFootprintKey(cfg)
	foot := footKey{fam, quant, "image-gen"}
	if cfg.ImageMulticard() {
		// The card-only placement ignores the pool keys and the pin: the pair is the lease's (mediamulticard.go).
		return multicardNeed(hostneed.RouteImage, cfg, token).withFootprint(foot)
	}
	if cfg.ImagePooled() {
		return pooledNeed(token, cfg.ImageGenPoolCompute, cfg.ImageGenPoolDonor).declaresRAM(routeRAM(hostneed.RouteImage, cfg)).withFootprint(foot)
	}
	return singleCardNeed(cfg, token).declaresRAM(routeRAM(hostneed.RouteImage, cfg)).withFootprint(foot)
}

// editNeed is the need of the generative-edit route for the resolved binding.
func editNeed(cfg config.Config, token string) mediaNeed {
	if cfg.EditMulticard() {
		return multicardNeed(hostneed.RouteEdit, cfg, token)
	}
	return singleCardNeed(cfg, token).declaresRAM(routeRAM(hostneed.RouteEdit, cfg))
}

// videoNeed is the need of the video route: pooled, or a single card like image generation.
func videoNeed(cfg config.Config, token, renderFamily string, over hostneed.VideoOverrides) mediaNeed {
	// The key the video route's sampling hook records under (runGenerateVideo): the family that renders and the
	// quant of the binding it renders with.
	fb := cfg.ResolveVideoFamilyBinding(renderFamily)
	foot := footKey{videoFootprintFamily(renderFamily), videoFootprintQuant(fb, renderFamily), "video-gen"}
	if cardOnlyVideo(fb, renderFamily) {
		// The card-only Wan and LTX-2.5 placement ignores the pool keys and the pin: the pair is the lease's (mediamulticard.go).
		return videoMulticardNeed(cfg, token, renderFamily, over, fb).withFootprint(foot)
	}
	if cfg.VideoPooled() {
		return pooledNeed(token, cfg.VideoGenPoolCompute, cfg.VideoGenPoolDonor).declaresRAM(videoRAM(cfg, renderFamily, over)).withFootprint(foot)
	}
	return singleCardNeed(cfg, token).declaresRAM(videoRAM(cfg, renderFamily, over)).withFootprint(foot)
}

// mediaGrant is what a call holds while it renders.
type mediaGrant struct {
	// Env hands the lease down to the runner (GPU_LEASE_*).
	Env []string
	// Card and API are set when the call runs in the ComfyUI instance bound to one card.
	Card *gpuprobe.Card
	API  string
	// Devices are the lease ids of the two cards a card-only call (needMulticard) runs on, sorted. Set only when the grant holds that pair, so the answer
	// of every other call names no cards; the image, edit and batch answers carry them as "devices", the key the queued answer already uses.
	Devices []string
	// Release gives everything back; call it exactly when the render is done (it is idempotent).
	Release func()
}

// name adds the cards a card-only call ran on to its answer as "devices", the key the queued answer names them under. A grant that holds no such pair
// leaves the answer exactly as it was.
func (g mediaGrant) name(payload map[string]any) {
	if len(g.Devices) > 0 {
		payload["devices"] = g.Devices
	}
}

// launch applies the grant to a binding's launch profile: a call on a per-card instance is
// pinned by the card's uuid at the instance's endpoint, and the legacy device index goes blank
// (the runner refuses both).
func (g mediaGrant) launch(l imagegen.ComfyLaunch) imagegen.ComfyLaunch {
	if g.Card == nil {
		return l
	}
	l.CardUUID, l.API, l.CudaDevice = g.Card.UUID, g.API, ""
	return l
}

// errGPUQueued is a media call's answer when it waited its window and has no card: the place it
// holds in line. The caller resumes it with the token.
type errGPUQueued struct {
	Token    string
	Position int
	ETASec   int
	Devices  []string
	HeldBy   []uint64
	Why      string
}

func (e *errGPUQueued) Error() string {
	// The estimate is the declared end of the LONGEST lease in the way: a request is served when
	// every lease it conflicts with has ended. 0 means nothing in the way declares an end (the
	// places ahead are callers, not leases), which is not "no wait".
	wait := "no estimate (nothing ahead of you declares an end)"
	if e.ETASec > 0 {
		wait = fmt.Sprintf("at most %ds until the lease(s) in the way end", e.ETASec)
	}
	return fmt.Sprintf("gpu queued: %s; your place in line is #%d (token %s), %s; call again with waiter_token=%s to keep it",
		e.Why, e.Position, e.Token, wait, e.Token)
}

// queuedPayload is the machine-readable half of the answer.
func (e *errGPUQueued) payload() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"queued":         true,
		"waiter_token":   e.Token,
		"queue_position": e.Position,
		// An upper bound: the declared window of the lease in the way, which for media is a ceiling
		// the holder usually finishes well inside, not a promise.
		"eta_s":   e.ETASec,
		"devices": e.Devices,
		"held_by": e.HeldBy,
		"resume":  "call again with the same request and waiter_token=" + e.Token + " within 10 minutes to resume this place; a place not re-polled for 30 seconds stops holding back later callers",
	})
	return b
}

// mediaClaimRetries bounds how many times a call re-allocates after losing a claim race before
// it queues on the card it last picked.
const mediaClaimRetries = 16

var scopedWarnOnce sync.Once

// scopedManager opens the lease Manager with the per-host card-scoped switch applied. A host that
// asked for card-scoped leases and has no green reader audit keeps the writer off; that is said
// once per process, not per call.
func (p *Pipeline) scopedManager() (*gpulease.Manager, error) {
	m, err := gpulease.OpenAt(p.cfg.GPULockPath, p.cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if err := m.ApplyCardScopedConfig(p.cfg.GPUCardScopedLeases); err != nil {
		scopedWarnOnce.Do(func() { log.Printf("media admission: %v", err) })
	}
	return m, nil
}

var planNoteOnce sync.Map // note -> struct{}

// planNote logs why a call fell back to the whole-node lease, once per distinct reason.
func planNote(why string) { planNoteKeyed(why, why) }

// planNoteKeyed is planNote for a text that carries something that changes without the fact changing: the note is made
// once per key.
func planNoteKeyed(key, text string) {
	if _, dup := planNoteOnce.LoadOrStore(key, struct{}{}); !dup {
		log.Printf("media admission: %s", text)
	}
}

// mediaPlan is the decision for one call.
type mediaPlan struct {
	whole    bool     // the legacy whole-node lease
	auto     bool     // the allocator picks the card
	ids      []string // the cards, when fixed (lease ids)
	instance bool     // run in the per-card instance of ids[0]
	pair     bool     // ids are the card-only placement's two non-display cards (needMulticard), which the call's answer names
}

// planMedia turns a need into a plan. cards is the card table (nil when it could not be read).
func planMedia(need mediaNeed, cards []gpuprobe.Card, tableErr error) (mediaPlan, error) {
	whole := func(why string) (mediaPlan, error) {
		if why != "" {
			planNote(why)
		}
		return mediaPlan{whole: true}, nil
	}
	if need.Kind == needWhole {
		return whole("")
	}
	if need.Kind == needMulticard {
		// Never the whole node, never one card: the pair, or the reason there is none.
		return planMulticard(need, cards, tableErr)
	}
	if tableErr != nil || len(cards) == 0 {
		// Said by the caller, on every such call and in the answer (unreadableAtAdmission): the
		// once-per-reason line of whole() would hide the second incident of a process's life.
		return mediaPlan{whole: true}, nil
	}
	switch need.Kind {
	case needSingle:
		if need.Pin == "" {
			return mediaPlan{auto: true, instance: true}, nil
		}
		if strings.Contains(need.Pin, ",") {
			return whole(fmt.Sprintf("comfy_cuda_device %q names several cards: this call holds the whole node and keeps today's --cuda-device", need.Pin))
		}
		n, err := strconv.Atoi(need.Pin)
		if err != nil {
			return whole(fmt.Sprintf("comfy_cuda_device %q is not a ComfyUI device index: this call holds the whole node", need.Pin))
		}
		c, ok := gpuprobe.CardByComfyOrder(cards, n)
		if !ok {
			return whole(fmt.Sprintf("comfy_cuda_device %q is in ComfyUI's device order, which this box has not declared (config gpu_comfy_order), so it cannot be turned into a card: this call holds the whole node and keeps today's --cuda-device", need.Pin))
		}
		// The explicit pin is a hard constraint: this card and no other.
		return mediaPlan{ids: []string{c.LeaseID()}, instance: true}, nil
	case needPooled:
		ids, why := poolCards(cards, need.Pool)
		if why != "" {
			return whole(why)
		}
		return mediaPlan{ids: ids}, nil
	case needDeclared:
		got, err := gpuprobe.ResolveCards(cards, need.Devices)
		if err != nil {
			return mediaPlan{}, fmt.Errorf("devices %q: %w", strings.Join(need.Devices, ","), err)
		}
		ids := make([]string, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.LeaseID())
		}
		sort.Strings(ids)
		// One device runs in that card's own instance, pinned by uuid: the graph sees one card and
		// can touch no other. Several would run in the DEFAULT instance, which sees every card, and
		// an arbitrary graph may place work on any of them, including a card another call's own
		// instance holds and the display card; a lease on only some of the cards would not stop
		// that, so several devices hold the whole node (unlike a pooled route, whose graph the
		// harness itself builds for its pool's cards).
		if len(ids) == 1 {
			return mediaPlan{ids: ids, instance: true}, nil
		}
		return whole(fmt.Sprintf("devices %q name several cards of a box with %d: the graph runs in the default instance, which sees every card, so a lease on some of them would not keep it off the others: this call holds the whole node", strings.Join(need.Devices, ","), len(cards)))
	}
	return whole("")
}

// poolCards resolves a pool's ComfyUI device keys to cards, through the order the box declares.
func poolCards(cards []gpuprobe.Card, keys []string) (ids []string, why string) {
	seen := map[string]bool{}
	for _, k := range keys {
		k = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(k)), "cuda:"))
		if k == "" {
			continue
		}
		n, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Sprintf("pool key %q is not a ComfyUI device: this call holds the whole node", k)
		}
		c, ok := gpuprobe.CardByComfyOrder(cards, n)
		if !ok {
			return nil, fmt.Sprintf("pool key cuda:%d is in ComfyUI's device order, which this box has not declared (config gpu_comfy_order), so it cannot be turned into a card: this call holds the whole node", n)
		}
		if !seen[c.LeaseID()] {
			seen[c.LeaseID()] = true
			ids = append(ids, c.LeaseID())
		}
	}
	sort.Strings(ids)
	if !poolMayBeScoped(len(ids), len(cards)) {
		return nil, fmt.Sprintf("a pool of %d card(s) on a box with %d may not be leased as a set: this call holds the whole node", len(ids), len(cards))
	}
	return ids, ""
}

// poolMayBeScoped says whether a job that runs in the DEFAULT instance may lease only some
// cards. Two default-instance jobs must never run at once (one process, one port, one marker; a
// second would reuse the first's instance on the first's cards), and the lease is what keeps
// them apart, so the sets have to overlap whatever they are. Two sets of at least two cards on a
// box of at most three always overlap; on a larger box they need not, so the whole node is held.
func poolMayBeScoped(set, total int) bool { return set >= 2 && total <= 3 }

// mediaGrantEnv is the lease env a runner is handed. Devices (lease ids) are exported only for a
// card-scoped lease, so a whole-node call's env is what it always was.
func (p *Pipeline) mediaGrantEnv(lease *gpulease.Lease, ids []string, unload string) []string {
	env := []string{
		"GPU_LEASE_DIR=" + lease.Dir(),
		"GPU_LEASE_EPOCH=" + strconv.FormatUint(lease.Epoch(), 10),
		"GPU_LEASE_CLASS=" + string(lease.Class()),
	}
	if len(ids) > 0 {
		env = append(env, "GPU_LEASE_DEVICES="+strings.Join(ids, ","))
	}
	if unload != "" {
		env = append(env, unload)
	}
	return append(env, p.lockEnv()...)
}

// unloadEnv is the GPU_LEASE_UNLOAD_MODELS entry for a lease on ids: the models the runner's
// unload may take (gpualloc.UnloadModels): every model of the roster except the memory stack and the
// models positively placed on another card, by layers or by the serving config's device pin. A model
// placed on the call's card goes, and so does a model nothing places, as before F15. Without the
// variable the render lane unloads every model off the memory stack, and a render on one card
// empties the seats placed on the others (register C-86, F15). "" for a whole-node call (no ids),
// which holds every card, and "" for a card call whose roster could not be read: nothing then says
// what is resident, a list built from the config alone could leave a resident model on the leased card
// (or, on a box that declares nothing, unload nothing), so the lane keeps its own rule and the log says
// so. Otherwise a card call gets a list, and with no llama-swap endpoint it is the none sentinel.
//
// Which seat sits on which card needs the card LIST, not a fresh reading (uuids and indices do not
// change under a running call), so it is scoped from the table the call already holds: a third
// nvidia-smi read here, with the lease already granted, could only cost time, and one that ran out
// used to widen the list to every seat, so a render on one card emptied the seats on the others.
func (p *Pipeline) unloadEnv(ctx context.Context, ids []string, cards []gpuprobe.Card) string {
	if len(ids) == 0 {
		return ""
	}
	if strings.TrimSpace(p.cfg.Endpoint) == "" {
		return gpualloc.UnloadEnv(nil, true)
	}
	var roster []string
	r, rosterErr := swapclient.FetchRoster(ctx, p.cfg.Endpoint, 5*time.Second)
	if rosterErr == nil {
		roster = r.IDs()
	}
	held := p.alloc
	held.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return cards, "", nil }
	models, known := gpualloc.UnloadModels(ctx, p.cfg, roster, rosterErr, ids, held, log.Writer())
	return gpualloc.UnloadEnv(models, known)
}

// instanceEndpoint is the endpoint of the ComfyUI instance bound to a card: the base port plus
// the card's nvidia-smi index (8189, 8190, 8191 on the 3-card box). Stable per card, because the
// render layer refuses to reuse an instance whose marker records another port.
func (p *Pipeline) instanceEndpoint(c gpuprobe.Card) string {
	if p.instanceAPI != nil {
		return p.instanceAPI(c)
	}
	return defaultInstanceAPI(c)
}

// instancePortBase is the first port of the per-card range (COMFY_PORT_BASE moves the range, as
// it does in the render layer).
const instancePortBase = 8189

func defaultInstanceAPI(c gpuprobe.Card) string {
	base := instancePortBase
	if v := strings.TrimSpace(os.Getenv("COMFY_PORT_BASE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1024 && n <= 65535 {
			base = n
		}
	}
	return "http://127.0.0.1:" + strconv.Itoa(base+c.NvidiaIndex)
}

// stopKeptInstances stops the ComfyUI instances kept under a lease epoch (internal/comfyinst),
// before the lease is released, except those that bound their own process (ADR 0097): those stay
// for the next job and end themselves once idle. A failure is logged and never blocks the release.
func (p *Pipeline) stopKeptInstances(epoch uint64) {
	stop := p.stopKept
	if stop == nil {
		stop = func(ctx context.Context, dir string, epoch uint64) []comfyinst.Outcome {
			return comfyinst.StopForRelease(ctx, dir, epoch, comfyinst.RealDeps())
		}
	}
	if strings.TrimSpace(p.cfg.ComfyDir) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, o := range stop(ctx, p.cfg.ComfyDir, epoch) {
		if o.Stopped {
			log.Printf("media admission: stopped the ComfyUI instance %s (pid %d) kept under lease epoch %d", o.Key, o.PID, epoch)
		} else if o.Spared {
			log.Printf("media admission: left the ComfyUI instance %s (pid %d) of lease epoch %d running: %s", o.Key, o.PID, epoch, o.Why)
		} else if o.Why != "" {
			log.Printf("media admission: the ComfyUI instance %s (pid %d) kept under lease epoch %d was not stopped: %s", o.Key, o.PID, epoch, o.Why)
		}
	}
}

// acquireMediaLease takes what one generation job needs of the GPU and returns the grant. See the
// file comment for what a call asks for; a call that needs the whole node, or runs on a host that
// cannot lease cards, takes the whole-node lease exactly as it always did (acquireWholeNode).
func (p *Pipeline) acquireMediaLease(ctx context.Context, reason string, ttl, wait time.Duration, need mediaNeed) (mediaGrant, error) {
	if need.Refusal != "" {
		return mediaGrant{}, &errMulticard{why: need.Refusal}
	}
	start := time.Now()
	// tableNote is set when the call asked for the whole node ONLY because the card table could not
	// be read: the answer says so.
	tableNote := ""
	// readTable is what the whole-node path's host-RAM declaration sizes against: nil = read the table
	// there; an empty, non-nil table = the admission already read it (twice) and it gave nothing, so the
	// declaration takes the card as unknown and does not ask nvidia-smi again (F24: a wedged driver costs a
	// call one retry, not one per stage).
	var readTable []gpuprobe.Card
	if need.Kind != needWhole {
		g, handled, note, err := p.acquireCardScoped(ctx, reason, ttl, wait, need)
		if handled {
			return g, err
		}
		if tableNote = note; note != "" {
			readTable = []gpuprobe.Card{}
		}
	}
	// The whole-node path. A host that leases cards also keeps places in line for it: the place a
	// caller resumes is looked up BEFORE the wait, because the wait's own waiter consumes the token
	// and the answer below must still carry the arrival time it left with.
	var place *wholePlace
	if need.Token != "" {
		if m, err := p.scopedManager(); err == nil && m.CardScoped() {
			if tok, ok := m.ResumeToken(need.Token); ok {
				place = &wholePlace{m: m, tok: tok}
			}
		}
	}
	// The place a whole-node call resumed is kept held while it waits, as a single-card call's is
	// (keepPlace); the keeper is stopped before the place is spent or left again.
	stopKeep := func() {}
	if place != nil {
		stopKeep = keepPlace(place.m, place.tok.ID)
	}
	env, release, err := p.acquireWholeNodeWith(ctx, reason, ttl, wait, need.Token, p.declaredRAM(ctx, need, readTable, nil))
	stopKeep()
	if err != nil {
		return mediaGrant{}, withTableNote(p.wholeNodeBusy(err, reason, ttl, start, place, need.Resumable), tableNote)
	}
	if place != nil {
		place.m.DropToken(place.tok.ID) // served: the place is spent (a grant without a wait never consumed it)
	}
	return mediaGrant{Env: env, Release: release}, nil
}

// acquireMediaLeaseHeld is acquireMediaLease for a caller that waits a long time for its card (a batch with nowhere else to
// go): the wait is the lease queue's, so the place in line is kept for the whole of it, and it ends the moment ctx does. The
// queue's wait takes no context, so it runs beside the caller; if the card is granted after the caller left, the grant is
// given straight back, so an interrupted batch leaves no lease behind. The abandoned wait ends with the process or at its own
// deadline.
func (p *Pipeline) acquireMediaLeaseHeld(ctx context.Context, reason string, ttl, wait time.Duration, need mediaNeed) (mediaGrant, error) {
	return holdAcquire(ctx, func() (mediaGrant, error) { return p.acquireMediaLease(ctx, reason, ttl, wait, need) })
}

// holdAcquire runs acquire beside the caller and returns when it does or when ctx ends, whichever is first. A grant that
// arrives after the caller left is released at once.
func holdAcquire(ctx context.Context, acquire func() (mediaGrant, error)) (mediaGrant, error) {
	type result struct {
		g   mediaGrant
		err error
	}
	done := make(chan result, 1)
	go func() {
		g, err := acquire()
		done <- result{g, err}
	}()
	select {
	case r := <-done:
		return r.g, r.err
	case <-ctx.Done():
		go func() {
			if r := <-done; r.err == nil && r.g.Release != nil {
				r.g.Release()
			}
		}()
		return mediaGrant{}, fmt.Errorf("interrupted while waiting for the GPU lane: %w", ctx.Err())
	}
}

// wholePlace is a place in line a whole-node call resumes.
type wholePlace struct {
	m   *gpulease.Manager
	tok gpulease.Token
}

// wholeNodeBusy turns the whole-node path's busy refusal into a place in line on a host that
// leases cards: the token replaces the refusal there, for a call that holds the whole node as for
// one that holds a card. A host that cannot lease cards (no flag, or no green reader audit) keeps
// the refusal byte for byte, and any other error is returned as it is.
func (p *Pipeline) wholeNodeBusy(err error, reason string, ttl time.Duration, start time.Time, place *wholePlace, resumable bool) error {
	var busy *errGPUBusy
	if !errors.As(err, &busy) && !errors.Is(err, gpulease.ErrStillQueued) {
		return err
	}
	if !resumable {
		// No door to hand a token back through: the refusal it always got.
		if busy != nil {
			return err
		}
		return &errGPUBusy{detail: err.Error()}
	}
	m, merr := p.scopedManager()
	if merr != nil || !m.CardScoped() {
		return err
	}
	since, tokenID := start, ""
	if place != nil {
		since, tokenID = place.tok.Since(), place.tok.ID
	}
	opts := gpulease.Options{Reason: reason, Origin: "pipeline", TTL: ttl, ResumeToken: tokenID}
	if busy != nil && busy.hostRAM != nil {
		// The whole node is free; the host's memory is not. The place in line says so.
		return p.queuedAnswerWhy(m, nil, since, tokenID, opts, reason, nil, true, busy.hostRAM.Error())
	}
	if qerr := p.queuedAnswer(m, nil, since, tokenID, opts, reason, nil, true); qerr != nil {
		return qerr
	}
	return err
}

// acquireCardScoped is the per-card path. handled is false when the call is not eligible for it
// (an ambient whole-node lease, a host that cannot write card-scoped leases, a plan that falls
// back to the whole node): the caller then takes the legacy path. note is non-empty only when the
// fall back to the whole node is because the card table could not be read (after its one retry):
// the legacy path's answer carries it (withTableNote).
func (p *Pipeline) acquireCardScoped(ctx context.Context, reason string, ttl, wait time.Duration, need mediaNeed) (g mediaGrant, handled bool, note string, err error) {
	inherited, ierr := ambientLeaseEnv()
	if ierr != nil {
		return mediaGrant{}, true, "", ierr
	}
	if inherited != nil {
		return p.acquireInherited(ctx, inherited, wait, need)
	}
	m, err := p.scopedManager()
	if err != nil || !m.CardScoped() {
		if need.Kind == needMulticard {
			return mediaGrant{}, true, "", noCardLeases(need, err) // a whole-node lease names no cards, and the placement needs two
		}
		return mediaGrant{}, false, "", nil // the legacy path reports the open error, or runs as before
	}
	cards, _, terr := p.alloc.CardTable(ctx, p.cfg)
	plan, perr := planMedia(need, cards, terr)
	if perr != nil {
		return mediaGrant{}, true, "", perr
	}
	if plan.whole {
		if terr != nil || len(cards) == 0 {
			note = unreadableAtAdmission(terr)
			log.Printf("media admission: %s", note)
		}
		return mediaGrant{}, false, note, nil
	}
	g, err = p.acquireCards(ctx, m, reason, ttl, wait, need, plan, cards)
	return g, true, "", err
}

// unreadableAtAdmission is what a call says when the card table could not be read at all: it holds
// the whole node, as it did before cards were leased, and the answer it may leave in line says why
// it is waiting for the node and not for a card.
func unreadableAtAdmission(tableErr error) string {
	cause := "it listed no card"
	if tableErr != nil {
		cause = tableErr.Error()
	}
	return fmt.Sprintf("the card table could not be read (%s), so this call asks for the whole node, as it did before cards were leased", cause)
}

// withTableNote adds the words about an unreadable card table to the answer of a call that has to
// wait (a place in line, or the plain busy answer): the reader of "gpu queued: card(s) x in use"
// would otherwise not know the call was placed without a fresh table. An error that is neither
// passes through unchanged, as does an empty note.
func withTableNote(err error, note string) error {
	if err == nil || note == "" {
		return err
	}
	var queued *errGPUQueued
	if errors.As(err, &queued) {
		queued.Why += "; " + note
		return err
	}
	var busy *errGPUBusy
	if errors.As(err, &busy) {
		return fmt.Errorf("%w; %s", err, note)
	}
	return err
}

// lookupCard finds the card with a lease id.
func lookupCard(cards []gpuprobe.Card, id string) (gpuprobe.Card, bool) {
	for _, c := range cards {
		if c.LeaseID() == id {
			return c, true
		}
	}
	return gpuprobe.Card{}, false
}

// placeKeepEvery is how often a call that resumed a place in line re-asserts it while it waits in
// this process: a third of the grace, so one missed tick still leaves the place held. A var so a
// test can shorten it.
var placeKeepEvery = gpulease.TokenGrace / 3

// keepPlace re-asserts the place in line token id holds, every placeKeepEvery, until the returned
// func is called (which also JOINS the goroutine, so nothing touches the token after the caller
// moves on to spend, consume or re-leave it). A token's life is its last poll; a resumed call that
// then waits for the card (in this process, for its slot) polls nothing, so without this its place
// read as absent after the grace and later callers and other processes' waiters skipped it. It is
// one timer for the length of a wait that is already bounded by gpu_wait_ms, and it only ever
// refreshes a token that still exists (gpulease.TouchToken). An empty id keeps nothing.
func keepPlace(m *gpulease.Manager, id string) (stop func()) {
	if id == "" {
		return func() {}
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(placeKeepEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				m.TouchToken(id)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
}

// tableReads is what one allocation knows about the card table across its stages: the pick, the
// grant-time check, the pick again after a lost claim. Each stage wants the table as it is NOW, and
// a read that fails (after the retry of gpualloc.Deps.CardTable) must not fail the call: nvidia-smi
// under load is slow, not gone, and the call has just been served by a table it read itself. So the
// call goes on from the newest table it did read (the admission one, at worst): which cards exist,
// which are the screen's and what they hold do not change in the seconds a call is placed in, and
// who holds what comes from the lease directory, read fresh every time. Two things are kept
// conservative: the screen's card stays closed to the call (the free VRAM that decides whether the
// operator's desktop keeps its cap is the one thing a stale table could misstate), and the call
// does not ask nvidia-smi again, so a wedged driver costs one retry per call and not one per stage.
type tableReads struct {
	cards []gpuprobe.Card // the newest table this call read
	at    time.Time       // when it was read
	// cause is why the call stopped reading: the failure of the read that sent it to cards.
	cause error
	age   string // how old cards were at that moment, in words
}

// stood reports whether the call is placing from a stored table.
func (r *tableReads) stood() bool { return r.cause != nil }

// fail records the read that failed and says so in the log, once per call.
func (r *tableReads) fail(cause error) {
	r.cause = cause
	r.age = "less than a second earlier"
	if d := time.Since(r.at).Round(time.Second); d >= time.Second {
		r.age = d.String() + " earlier"
	}
	log.Printf("media admission: the card table could not be re-read (%v): this call is placed from the table it read %s, with the screen's card kept closed to it", cause, r.age)
}

// note is the sentence the call's answer carries when it waits after a failed read.
func (r *tableReads) note() string {
	if !r.stood() {
		return ""
	}
	return fmt.Sprintf("the card table could not be re-read (%v): this call was placed from the table it read %s, with the screen's card kept closed to it", r.cause, r.age)
}

// mediaAllocInput is the allocator's input for a call that names no card. On a live table it is exactly
// gpualloc.BuildInput's: the display card is open up to the operator's cap (gpu_display_cap_pct) and the door
// forces nothing, so a call that declares a VRAM footprint (Need.VRAMGiB) can be given the screen's card
// within it. When the table cannot be re-read the call goes on from the one it has (tableReads), and then
// the display card is closed for that input (AllocInput.DisplayStale): the cap is judged on the VRAM in use now,
// which is the one number a stale table can misstate, so the screen's card stays closed to a call placed from it,
// as F24 kept it. The cap itself stays what the config says, so the skip names the table, not the key.
func (p *Pipeline) mediaAllocInput(ctx context.Context, m *gpulease.Manager, need gpualloc.Need, reads *tableReads) (gpulease.AllocInput, error) {
	if !reads.stood() {
		in, berr := gpualloc.BuildInput(ctx, m, p.cfg, need, p.alloc)
		if berr == nil {
			reads.cards, reads.at = in.Cards, time.Now()
			return in, nil
		}
		var unread *gpualloc.CardTableError
		if !errors.As(berr, &unread) || ctx.Err() != nil {
			return in, berr // not the table, or nobody is waiting for the answer any more
		}
		reads.fail(unread.Err)
	}
	stored := p.alloc
	table := reads.cards
	stored.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return table, "", nil }
	in, berr := gpualloc.BuildInput(ctx, m, p.cfg, need, stored)
	in.DisplayStale = true
	return in, berr
}

// acquireCards holds a lease on the planned cards, allocating the card when the plan says so. A card
// table that cannot be read while it does so is never the reason the call fails (tableReads).
func (p *Pipeline) acquireCards(ctx context.Context, m *gpulease.Manager, reason string, ttl, wait time.Duration,
	need mediaNeed, plan mediaPlan, cards []gpuprobe.Card) (grant mediaGrant, err error) {
	start := time.Now()
	deadline := start.Add(wait)
	remaining := func() time.Duration {
		if r := time.Until(deadline); r > 0 {
			return r
		}
		return 0
	}
	reads := &tableReads{cards: cards, at: start}
	defer func() { err = withTableNote(err, reads.note()) }()
	// askRAM is the host RAM the call declares IN THE GRANT: against the cards its plan names, else against the largest card
	// (askRAMFor, the smallest answer), and 0 for a call that defers its figure until its own unload has run (askRAMGate). What
	// the allocator's pre-filter reads, and whether the call declares any at all, which decides whether a waiter that waits
	// only on host RAM holds its cards against it (gpualloc.QueuedClaims).
	askRAM := p.askRAMGate(ctx, m, need, plan, cards, 0)
	// A call that resumes a place keeps the arrival time it left with, and keeps the place itself
	// held while it waits (keepPlace). The keeper is stopped before the call spends the place
	// (grantCards), consumes it (the lease wait registers a waiter) or leaves it again.
	since, tokenID := start, ""
	stopKeep := func() {}
	if tok, ok := m.ResumeToken(need.Token); ok {
		since, tokenID = tok.Since(), tok.ID
		stopKeep = keepPlace(m, tok.ID)
	}
	defer stopKeep()
	// deferrals holds, per card set, whether the call is compared to the host in the grant or after its own unload
	// (hostRAMDeferral). One reading per set for the whole call: the grant and the answers it leaves must agree on it.
	deferrals := map[string]hostRAMDeferral{}
	deferralFor := func(ids []string) hostRAMDeferral {
		key := strings.Join(ids, ",")
		if d, ok := deferrals[key]; ok {
			return d
		}
		d := p.hostRAMDeferral(ctx, m, p.declaredRAM(ctx, need, cards, ids), ids, cards, 0)
		deferrals[key] = d
		return d
	}
	optsFor := func(ids []string) gpulease.Options {
		// The host RAM this call declares on THESE cards: whether its weights fit decides it. A call that defers declares
		// nothing in the grant; the figure travels as HostRAMWaitGiB and is admitted after the clear.
		d := deferralFor(ids)
		return gpulease.Options{Reason: reason, Origin: "pipeline", TTL: ttl, Devices: ids, ResumeToken: tokenID,
			HostRAMGiB: d.gate(), HostRAMWaitGiB: d.later()}
	}
	// tokenOptsFor is optsFor for an answer that leaves a place in line: the place keeps the DECLARED figure, so it still reads
	// as waiting only on host RAM for the callers behind it.
	tokenOptsFor := func(ids []string) gpulease.Options {
		o := optsFor(ids)
		o.HostRAMGiB, o.HostRAMWaitGiB = deferralFor(ids).declared, 0
		return o
	}
	// refuseFor turns the end of the wait after the clear into the answer the grant gave for a host that stayed short: the
	// same words, the same defer class, the same place in line.
	refuseFor := func(ids []string) func(error) error {
		return func(err error) error {
			var short *gpulease.ErrHostRAM
			if errors.As(err, &short) && !short.Impossible {
				return p.hostRAMAnswer(m, ids, since, tokenID, tokenOptsFor(ids), reason, short, need.Resumable)
			}
			return err
		}
	}
	// claimedNow is what is taken that the lease directory cannot show: the in-process slots, and the
	// cards callers in line are promised.
	claimedNow := func() map[string]bool {
		claimed := mediaSlots.held()
		for id := range gpualloc.QueuedClaims(m, cards, tokenID, askRAM > 0) {
			claimed[id] = true
		}
		return claimed
	}
	// build is the allocator's input for an auto plan (the pick and the grant-time check). The
	// allocator applies the same host-RAM rule the grant does before it picks a card, advisory: the
	// call's need is read against the largest card (the smallest answer), so it never refuses what
	// the grant, which knows the card, would admit.
	build := func() (gpulease.AllocInput, error) {
		return p.mediaAllocInput(ctx, m, gpualloc.Need{Claimed: claimedNow(), RAMGiB: askRAM}, reads)
	}

	// An auto plan chooses its card from the display card's cap and the VRAM in use as they are when it
	// chooses, and the place in line it takes can be long. The grant puts the same rule again
	// (GrantCheck): a card that no longer qualifies is given back and the choice is made again from
	// fresh readings, with the arrival time kept, so the screen's card is never granted because it was
	// open when the call joined the line.
	for refusals := 0; ; refusals++ {
		var ids []string
		// hostShort is set when the gated try was refused for the HOST's memory, with the cards free.
		var hostShort *gpulease.ErrHostRAM
		for lost := 0; ; lost++ {
			free := true
			if plan.auto {
				picked, isFree, err := gpualloc.PickAuto(gpualloc.Plan{Min: 1, Max: 1}, remaining(), build, io.Discard, time.Sleep, time.Now)
				if err != nil {
					var none *gpulease.NoCardsError
					if errors.As(err, &none) {
						return mediaGrant{}, p.noCardsAnswer(m, none, since, tokenID, tokenOptsFor, reason, need.Resumable)
					}
					return mediaGrant{}, err
				}
				ids, free = picked, isFree
			} else {
				// The caller named these cards (a pin, a pool, a declared device): there is nothing
				// to choose, so nothing to read from the card table. What the lease directory, this
				// process and the line say is taken is all the answer depends on.
				ids = plan.ids
				whole, claimed := gpualloc.Claims(m, gpualloc.Need{Claimed: claimedNow()})
				free = !whole
				for _, id := range ids {
					if claimed[id] {
						free = false
					}
				}
				// A waiter that registers between the read above and the gated claim below is one the
				// read did not see; the claim must say it is ahead. The seam lands one exactly here.
				if p.afterClaimsRead != nil {
					p.afterClaimsRead()
				}
			}
			if free {
				if !mediaSlots.tryTake(ids) {
					// A job in this process holds it and has no lease record to show: it is claimed.
					if plan.auto && lost < mediaClaimRetries {
						continue
					}
					free = false
				} else {
					// One gated attempt (register D-1xx-3): a waiter registered for these cards that
					// the allocation above did not see answers ErrStillQueued and sends this request
					// to the queue below. A lost attempt must cost a call that resumed a place
					// nothing. So it does not hand the token over: a waiter that resumes a token
					// CONSUMES it, and the queue below would then register as a new arrival (an
					// explicit-card call sets no arrival time of its own, so the place it had would
					// be gone for the length of that wait). It carries the place's arrival time
					// instead, so the live token that still holds the place, older than any new
					// arrival, does not hold this attempt back: a token blocks only the waiters
					// that arrived after it.
					try := optsFor(ids)
					try.ResumeToken = ""
					if tokenID != "" {
						try.QueuedSince = since
					}
					lease, err := m.Acquire(gpulease.ClassMedia, try)
					if err == nil {
						stopKeep()
						return p.grantCards(ctx, m, lease, ids, plan, cards, tokenID, deferralFor(ids), start, refuseFor(ids))
					}
					mediaSlots.release(ids)
					var held *gpulease.ErrHeld
					var short *gpulease.ErrHostRAM
					switch {
					case errors.As(err, &short):
						// The cards are free and the HOST's memory is not. Allocating again cannot
						// help, so this call goes to the queue below, which waits for the room in the
						// same line; a need no state of this host admits is the refusal it is.
						if short.Impossible {
							return mediaGrant{}, err
						}
						hostShort = short
					case !errors.As(err, &held) && !errors.Is(err, gpulease.ErrStillQueued):
						return mediaGrant{}, err
					case plan.auto && lost < mediaClaimRetries:
						continue // another claimant took it first: allocate again with its claim visible
					}
					free = false
				}
			}
			break
		}

		// Nothing is free: take a place in line. The in-process slots first (bounded, FIFO among this
		// process's callers), then the lease queue, FIFO across processes, keeping the arrival time.
		//
		// A call with no time left (gpu_wait_ms 0, or a window spent) answers here, and the answer must say
		// what is true: when the gated try was refused for the host's memory the cards are free and nobody is
		// ahead, so it carries the guard's own sentence (hostRAMAnswer, as the waiting path below does) and
		// not the line's "promised to callers ahead", which named nobody (G2 of the P0 plan).
		noTime := func() error {
			if hostShort != nil {
				return p.hostRAMAnswer(m, ids, since, tokenID, tokenOptsFor(ids), reason, hostShort, need.Resumable)
			}
			return p.queuedAnswer(m, ids, since, tokenID, tokenOptsFor(ids), reason, nil, need.Resumable)
		}
		if !mediaSlots.take(ids, remaining()) {
			return mediaGrant{}, noTime()
		}
		if remaining() <= 0 {
			mediaSlots.release(ids)
			return mediaGrant{}, noTime()
		}
		qo := optsFor(ids)
		qo.Wait, qo.WaitOut = remaining(), true
		if plan.auto {
			// The set was chosen from the presence and the floor as they were; the grant asks again.
			qo.QueuedSince = since
			qo.GrantCheck = gpualloc.GrantCheck(build, ids)
		}
		stopKeep() // the waiter the lease wait registers stands for the place from here
		lease, err := m.Acquire(gpulease.ClassMedia, qo)
		if err != nil {
			mediaSlots.release(ids)
			var refused *gpulease.ErrGrantRefused
			if plan.auto && errors.As(err, &refused) && refusals < mediaClaimRetries {
				continue // the card no longer qualifies: choose again from fresh readings
			}
			var held *gpulease.ErrHeld
			if errors.As(err, &held) || errors.Is(err, gpulease.ErrStillQueued) {
				return mediaGrant{}, p.queuedAnswer(m, ids, since, tokenID, tokenOptsFor(ids), reason, held, need.Resumable)
			}
			var short *gpulease.ErrHostRAM
			if errors.As(err, &short) && !short.Impossible {
				return mediaGrant{}, p.hostRAMAnswer(m, ids, since, tokenID, tokenOptsFor(ids), reason, short, need.Resumable)
			}
			return mediaGrant{}, err
		}
		return p.grantCards(ctx, m, lease, ids, plan, cards, tokenID, deferralFor(ids), start, refuseFor(ids))
	}
}

// noCardsAnswer is the answer for a call that waited its window and the allocator still found no
// card for it although the cards may be idle: the host is short of RAM, a transient nvidia-smi
// failure left the monitor's card unknown, every card is the operator's screen or quarantined. It
// used to leave through the raw allocator error, classed as a lease fault
// (gpu_lease_unavailable). A call that can resume keeps a place on the cards that would qualify
// but for what is short (the allocator lists them as Waitable), with the reason; when no card
// could qualify however long it waited there is nothing to hold a place on, and every call gets
// the plain busy defer, with the reason.
func (p *Pipeline) noCardsAnswer(m *gpulease.Manager, none *gpulease.NoCardsError, since time.Time, tokenID string,
	optsFor func([]string) gpulease.Options, reason string, resumable bool) error {
	if none.HostImpossible {
		return none // no card and no wait makes it fit: the refusal names the flag and the key to change
	}
	why := "no card can take this call right now: " + gpualloc.SkipSummary(none)
	ids := none.Waitable
	if !resumable || len(ids) == 0 {
		return &errGPUBusy{detail: why}
	}
	return p.queuedAnswerWhy(m, ids, since, tokenID, optsFor(ids), reason, nil, true, why)
}

// hostRAMAnswer is the answer for a call that waited its window and the cards were free but the host's
// memory was not: a call that can resume keeps its place in line with the reason, and any other gets
// the plain busy defer carrying the same words.
func (p *Pipeline) hostRAMAnswer(m *gpulease.Manager, ids []string, since time.Time, tokenID string, opts gpulease.Options, reason string, short *gpulease.ErrHostRAM, resumable bool) error {
	if !resumable {
		return &errGPUBusy{detail: short.Error(), hostRAM: short}
	}
	return p.queuedAnswerWhy(m, ids, since, tokenID, opts, reason, nil, true, short.Error())
}

// queuedAnswer leaves the place-keeping token for a call that waited its window and still has no
// card, and builds the answer that carries it. A call whose door cannot resume a place (resumable
// false) leaves nothing and gets the plain busy answer.
func (p *Pipeline) queuedAnswer(m *gpulease.Manager, ids []string, since time.Time, tokenID string, opts gpulease.Options, reason string, held *gpulease.ErrHeld, resumable bool) error {
	return p.queuedAnswerWhy(m, ids, since, tokenID, opts, reason, held, resumable, "")
}

// queuedAnswerWhy is queuedAnswer with the reason the call is queued stated by the caller, for a
// wait that no lease explains (why != "").
func (p *Pipeline) queuedAnswerWhy(m *gpulease.Manager, ids []string, since time.Time, tokenID string, opts gpulease.Options, reason string, held *gpulease.ErrHeld, resumable bool, why string) error {
	if !resumable {
		return busyAnswer(ids, held)
	}
	tok, err := m.LeaveToken(gpulease.ClassMedia, opts, since)
	if err != nil {
		// Without a place to keep, the honest answer is the old one: the card is busy.
		return &errGPUBusy{detail: fmt.Sprintf("the card(s) %s are held and a place in line could not be kept: %v", strings.Join(ids, ", "), err)}
	}
	e := &errGPUQueued{Token: tok.ID, Position: m.QueuePosition(ids, since, tok.ID), Devices: ids}
	now := time.Now()
	for _, l := range m.Leases() {
		// A request that names cards is in the way of, and blocked by, the leases on those cards;
		// a request for the whole node (no ids) is blocked by every lease, as conflict() in
		// mediaslots.go has it. A whole-node lease (no devices) blocks everyone.
		if len(ids) != 0 && len(l.Devices) != 0 && !intersects(l.Devices, ids) {
			continue
		}
		e.HeldBy = append(e.HeldBy, l.Epoch)
		if !l.ExpiresAt.IsZero() {
			if s := int(l.ExpiresAt.Sub(now).Round(time.Second) / time.Second); s > e.ETASec {
				e.ETASec = s
			}
		}
	}
	subject := "the whole node"
	if len(ids) != 0 {
		subject = "card(s) " + strings.Join(ids, ", ")
	}
	switch {
	case why != "":
		e.Why = why
	case held != nil:
		// "held by a media-class lease", never "held by media": the bare class read as a seat (gpulease.Class.LeasePhrase).
		e.Why = fmt.Sprintf("%s held by %s (%q)", subject, held.Info.Class.LeasePhrase(), held.Info.Reason)
	case len(e.HeldBy) > 0:
		e.Why = fmt.Sprintf("%s in use", subject)
	default:
		e.Why = fmt.Sprintf("%s promised to callers ahead of this one", subject)
	}
	return e
}

// busyAnswer is the refusal a call gets when it waited its window with no card and keeps no place:
// the holder when there is one, else the cards (or the node) it was waiting for.
func busyAnswer(ids []string, held *gpulease.ErrHeld) error {
	if held != nil {
		return &errGPUBusy{info: held.Info}
	}
	subject := "the whole node"
	if len(ids) != 0 {
		subject = "card(s) " + strings.Join(ids, ", ")
	}
	return &errGPUBusy{detail: subject + " are held, or promised to callers ahead of this one, and the wait ended before they freed"}
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// grantCards builds the grant for a lease just taken (the slots for ids are already held). A lease that deferred its host RAM
// (d.defers) clears the models its own unload takes now, under the heartbeat, and asks the host afterwards; when the host never
// has the room the lease is released and refuse gives the answer (grantCards then returns no grant).
func (p *Pipeline) grantCards(ctx context.Context, m *gpulease.Manager, lease *gpulease.Lease, ids []string, plan mediaPlan, cards []gpuprobe.Card, tokenID string,
	d hostRAMDeferral, start time.Time, refuse func(error) error) (mediaGrant, error) {
	if tokenID != "" {
		m.DropToken(tokenID) // the call is served: its place is spent
	}
	stopHB := startHeartbeat(lease)
	var once sync.Once
	release := func() {
		once.Do(func() {
			stopHB()
			// The instances kept under this lease go BEFORE it is released, so the next holder never
			// finds one on its card, except an instance that ends itself (ADR 0097: bare between jobs,
			// about 240 MiB of the card, gone after 300 s idle), which stays for the next job on these
			// cards to reuse; and only while the lease is still this call's: a call whose lease
			// was taken away (released from outside, reclaimed after a suspend) is a straggler, and
			// the instance may already be the next lease's job. It leaves them, as Release leaves
			// the claim.
			if err := lease.Check(); err != nil {
				log.Printf("media admission: leaving the ComfyUI instances kept under lease epoch %d running: the lease is no longer this call's (%v)", lease.Epoch(), err)
			} else {
				p.stopKeptInstances(lease.Epoch())
			}
			_ = lease.Release()
			mediaSlots.release(ids)
		})
	}
	unload := d.unload
	if d.defers {
		if err := p.clearThenAdmitHostRAM(ctx, lease, d, start); err != nil {
			release()
			return mediaGrant{}, refuse(err)
		}
	} else {
		unload = p.unloadEnv(ctx, ids, cards)
	}
	// The card is ours from here: the call's PAIR card turns running.
	core.MarkWorking(ctx)
	g := mediaGrant{Env: p.mediaGrantEnv(lease, ids, unload), Release: release}
	if plan.pair {
		g.Devices = append([]string(nil), ids...)
	}
	if plan.instance {
		if c, ok := lookupCard(cards, ids[0]); ok {
			g.Card, g.API = &c, p.instanceEndpoint(c)
		}
	}
	return g, nil
}

// startHeartbeat renews a lease every 15 s while a render runs: at most ONE timer for the
// duration of GPU work that lasts minutes, nothing ticking while the harness is idle. The returned
// func stops it and JOINS: Release mutates the Lease a Renew already in flight is reading, and
// would let that Renew re-create the heartbeat file after the release swept it.
func startHeartbeat(lease *gpulease.Lease) func() {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = lease.Renew()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-stopped
		})
	}
}

// acquireInherited serves a call running under a lease its parent holds on some cards
// (`gpu reserve --devices ... -- <cmd>`): GPU_LEASE_DEVICES names them. The lease is not ours to
// renew or release; the in-process slots arbitrate which of its cards this call runs on, so two
// calls under a two-card lease run on both cards. handled is false when the lease holds the whole
// node, or the call cannot be placed on its cards (the legacy path then serves it). note is non-empty
// only when the legacy path serves it because the card table could not be read (after its one retry):
// the call goes on in the default instance with no card of its own, which nobody asked for, so it is
// logged on every such call and rides the answer if the legacy path then has to refuse (withTableNote).
func (p *Pipeline) acquireInherited(ctx context.Context, inherited []string, wait time.Duration, need mediaNeed) (g mediaGrant, handled bool, note string, err error) {
	var held []string
	for _, id := range strings.Split(os.Getenv("GPU_LEASE_DEVICES"), ",") {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			held = append(held, id)
		}
	}
	if need.Kind == needMulticard {
		return p.acquireInheritedPair(ctx, inherited, held, wait, need)
	}
	if len(held) == 0 || need.Kind != needSingle {
		return mediaGrant{}, false, "", nil
	}
	cards, _, terr := p.alloc.CardTable(ctx, p.cfg)
	if terr != nil {
		note = unreadableUnderParent(terr)
		log.Printf("media admission: %s", note)
		return mediaGrant{}, false, note, nil
	}
	var cands []string
	for _, id := range held {
		if _, ok := lookupCard(cards, id); ok {
			cands = append(cands, id)
		}
	}
	if len(cands) == 0 {
		return mediaGrant{}, false, "", nil
	}
	if need.Pin != "" {
		// An explicit pin is the one card; it must be among the parent's, or the legacy path (and its
		// own --cuda-device) serves the call exactly as it did.
		plan, perr := planMedia(need, cards, nil)
		if perr != nil || plan.whole || len(plan.ids) != 1 || !containsString(cands, plan.ids[0]) {
			return mediaGrant{}, false, "", nil
		}
		cands = plan.ids
	}
	picked, ok := mediaSlots.takeAny(cands, wait)
	if !ok {
		return mediaGrant{}, true, "", &errGPUBusy{detail: fmt.Sprintf("every card of the lease this process runs under (%s) is in use by another job here after %s", strings.Join(cands, ", "), wait)}
	}
	core.MarkWorking(ctx)
	c, _ := lookupCard(cards, picked)
	var once sync.Once
	return mediaGrant{
		Env:     append(append([]string(nil), inherited...), append([]string{"GPU_LEASE_DEVICES=" + strings.Join(held, ",")}, p.lockEnv()...)...),
		Card:    &c,
		API:     p.instanceEndpoint(c),
		Release: func() { once.Do(func() { mediaSlots.release([]string{picked}) }) },
	}, true, "", nil
}

// unreadableUnderParent is what a call under its parent's lease says when the card table could not be
// read: which of the parent's cards it would have run on is the table's to say, so the call goes on
// the way it did before cards were leased, in the default instance, inside the parent's lease.
func unreadableUnderParent(tableErr error) string {
	return fmt.Sprintf("the card table could not be read (%s), so this call runs in the default instance under its parent's lease, with no card of its own, as it did before cards were leased", tableErr)
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// graphRAM is for a run_graph call: the weight files the graph's own loaders name (hostneed.ForGraphFile, by the family estimate's
// fit rule), and the media class default when the graph cannot be read or one of its files cannot be sized, as routeRAM falls back.
// A TEMPLATE run (params template, with the runtime manifest at manifest_path) carries no graph file: its files are the model paths
// that manifest names (hostneed.ForManifestFile, the same rule and source), and a manifest that is not given, cannot be read or names
// a file the box cannot size keeps the class default exactly as a graph does. The file is read at each call of the closure (the
// grant, the allocator's pre-filter and the lane probe each ask), and it is a few KiB.
func graphRAM(cfg config.Config, params map[string]any) func(float64) hostneed.Need {
	graphPath := paramStr(params, "graph_path")
	manifestPath := ""
	if paramStr(params, "template") != "" {
		manifestPath = paramStr(params, "manifest_path")
		graphPath = "" // the run names a template or a graph and never both (buildRunGraphParams); the template's files are the manifest's
	}
	// reserve_vram goes to the runner as --reserve-vram, the margin it holds back on the card, so the graph is fitted with it; a value the
	// runner would not take (not a number, 0 or less) leaves the config's, else the default.
	var reserve float64
	if r, err := strconv.ParseFloat(strings.TrimSpace(paramStr(params, "reserve_vram")), 64); err == nil && r > 0 {
		reserve = r
	}
	return func(vram float64) hostneed.Need {
		f := hostneed.Facts{Cfg: cfg, VRAMGiB: vram, ReserveVRAMGiB: reserve}
		if manifestPath != "" {
			if n, ok := hostneed.ForManifestFile(manifestPath, f); ok {
				return n
			}
		} else if n, ok := hostneed.ForGraphFile(graphPath, f); ok {
			return n
		}
		return hostneed.ClassDefault(f)
	}
}
