// Package gpualloc is where a request for cards becomes a decision: the live state the card
// allocator reads (the card table, the leases, the quarantine sidecars, resident seats, the
// operator's presence, host RAM), the choice itself (PickAuto), and the rule that says which
// seats a lease on some cards may unload (LeaseScope, UnloadModels).
//
// It exists so the two callers that need a card decision read ONE rule: `gpu reserve --cards`
// (package main) and the media admission path (internal/pipeline), which takes a card per
// generation call. The allocator itself is internal/gpulease's pure function; this package
// assembles its input and interprets its answer. Nothing here acquires a lease: the callers
// differ in how they claim (the reserve verb queues in the foreground, the pipeline holds a
// place with a token), so claiming stays with them.
package gpualloc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// The deadlines of a card-table read. The read is one nvidia-smi exec: a tenth of a second on a quiet
// box, and on a loaded one (three cards at 100 %, a model loading, a CPU with no slot for the exec)
// a read that answered in a second has then run out five. A read that runs out its first deadline
// is made once more under the longer one, so one slow read does not change a placement
// (F24, 2026-10-09: the allocation's re-read of the table ran out and hard-deferred a media call
// that its own admission read had just served).
const (
	DefaultCardRead      = 5 * time.Second
	DefaultCardReadRetry = 15 * time.Second
)

// Need is what the job asks of each card and of the host.
type Need struct {
	// VRAMGiB is the VRAM the job needs free on EACH card (0 = not declared).
	VRAMGiB float64
	// RAMGiB is the host RAM the job declares (0 = not declared).
	RAMGiB float64
	// Claimed names cards the caller already holds on behalf of work the lease directory cannot
	// show (the media path's in-process per-card slots): they are not allocatable.
	Claimed map[string]bool
}

// Deps are the live reads behind the allocator's input, so a test assembles a host. A nil
// member uses the production reader.
type Deps struct {
	// Cards reads the card table and a warning about the declared ComfyUI order. It is ONE attempt
	// and takes its deadline from ctx: CardTable sets it, and again, longer, for the one retry, so a
	// reader that adds a deadline of its own would cap that retry as well.
	Cards func(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error)
	// ReadDeadline bounds the first attempt of a card-table read and RetryDeadline the one retry
	// that follows an attempt which ran out of time (zero = DefaultCardRead, DefaultCardReadRetry).
	ReadDeadline, RetryDeadline time.Duration
	// ForeignBusy maps a card (lease id) to a description of the foreign compute process on it.
	// The default sees none: foreign-busy is Linux-only evidence (WDDM lists no per-process rows),
	// and the reserve verb's reader lives with its denylist in package main, which passes it in.
	ForeignBusy func(ctx context.Context, cfg config.Config) map[string]string
	// Resident maps a card to the configured seats loaded on it.
	Resident func(ctx context.Context, cfg config.Config, cards []gpuprobe.Card) map[string]gpulease.ResidentInfo
	// HostFreeRAM reads free host RAM in GiB.
	HostFreeRAM func() (float64, bool)
	// Presence says whether the operator is known to be away from the desk.
	Presence func(cfg config.Config) (known, away bool)
}

// DefaultDeps is the production wiring.
func DefaultDeps() Deps {
	return Deps{}.withDefaults()
}

// ReadCards is the production card-table reader, and the one every caller that does not inject its own
// uses (the allocator's default, and the root package's cardTableFn). It is gpuprobe's one per-device
// query (--query-gpu fields) and nothing else: the allocation never lists processes, the slowest thing
// nvidia-smi does under load (the foreign-process reader is a separate, best-effort call that only the
// verbs which want it make). It adds NO deadline of its own and takes the one in ctx, because CardTable
// puts a deadline on every attempt and a reader that capped itself would cap the retry's longer deadline
// too: the 5 s the base kept inside the default reader made the 15 s retry a 5 s one. Only a run against
// a real process can tell, so TestTheProductionReaderIsNotCappedBelowTheRetryDeadline does that, with a
// stand-in nvidia-smi (internal/gpuprobe/smitest), and TestTheAllocationNeverListsProcesses pins what
// the allocation execs.
func ReadCards(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
	return gpuprobe.ReadCards(ctx, cfg.GPUComfyOrder)
}

func (d Deps) withDefaults() Deps {
	if d.Cards == nil {
		d.Cards = ReadCards
	}
	if d.ForeignBusy == nil {
		d.ForeignBusy = func(context.Context, config.Config) map[string]string { return nil }
	}
	if d.Resident == nil {
		d.Resident = func(ctx context.Context, cfg config.Config, cards []gpuprobe.Card) map[string]gpulease.ResidentInfo {
			return ResidentSeats(ctx, cfg, cards, nil)
		}
	}
	if d.HostFreeRAM == nil {
		d.HostFreeRAM = gpuprobe.HostFreeRAMGiB
	}
	if d.Presence == nil {
		d.Presence = func(cfg config.Config) (bool, bool) {
			p := placement.ProbePresence(cfg.PresenceMode(), cfg.OperatorIdle())
			return p.Known, p.Away
		}
	}
	return d
}

// CardTable reads the card table the way the allocator does: one attempt under ReadDeadline and,
// when that attempt ran out of time, ONE more under the longer RetryDeadline. Only a timeout is
// retried: an attempt that failed outright (nvidia-smi not on PATH, a table with no card) fails the
// same way again, and a caller whose own context is done has nobody left to read for. The error of
// a read that failed twice says so, so the answer a caller builds from it can.
func (d Deps) CardTable(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
	d = d.withDefaults()
	first, retry := d.ReadDeadline, d.RetryDeadline
	if first <= 0 {
		first = DefaultCardRead
	}
	if retry <= 0 {
		retry = DefaultCardReadRetry
	}
	r := d.attempt(ctx, cfg, first)
	if r.err == nil || !r.ranOut {
		return r.cards, r.note, r.err
	}
	log.Printf("gpualloc: the card table read ran out of time after %s (%v); reading it once more under %s", first, r.err, retry)
	r = d.attempt(ctx, cfg, retry)
	if r.err != nil {
		return nil, "", fmt.Errorf("%w (read twice: no answer within %s, then none within %s)", r.err, first, retry)
	}
	return r.cards, r.note, nil
}

// cardRead is one attempt at the card table.
type cardRead struct {
	cards []gpuprobe.Card
	note  string
	err   error
	// ranOut says the attempt's own deadline ended it while the caller's context was still good:
	// the one failure a longer deadline can fix.
	ranOut bool
}

func (d Deps) attempt(ctx context.Context, cfg config.Config, deadline time.Duration) cardRead {
	actx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var r cardRead
	r.cards, r.note, r.err = d.Cards(actx, cfg)
	// Asked before cancel, which would make every attempt read as expired. A deadline the CALLER's
	// context carries (shorter than ours, and spent) also reads DeadlineExceeded here, which is why
	// the caller's context is asked too.
	r.ranOut = r.err != nil && errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	return r
}

// CardTableError is BuildInput's failure when the card table could not be read, so a caller that
// can place without a fresh table (the media path keeps the one it read at admission) tells that
// failure from any other. The words are what they always were: "the card table: <cause>".
type CardTableError struct{ Err error }

func (e *CardTableError) Error() string { return "the card table: " + e.Err.Error() }
func (e *CardTableError) Unwrap() error { return e.Err }

// Claims reads what is taken, without the card table: whether a whole-node lease is live (it claims
// every card) and the lease ids of the cards a live lease claims, plus the ones the caller claims
// for work the lease directory cannot show (Need.Claimed). A caller that NAMED its cards (a pin, a
// pool, a declared device) has nothing to choose, so this is all it asks of the allocator's input:
// it never needs nvidia-smi for it.
func Claims(m *gpulease.Manager, need Need) (wholeNodeHeld bool, claimed map[string]bool) {
	claimed = map[string]bool{}
	for _, l := range m.Leases() {
		if len(l.Devices) == 0 {
			wholeNodeHeld = true
		}
		for _, d := range l.Devices {
			claimed[d] = true
		}
	}
	for id, on := range need.Claimed {
		if on {
			claimed[id] = true
		}
	}
	return wholeNodeHeld, claimed
}

// BuildInput assembles the allocator's input from live state: the card table, the live leases,
// quarantine sidecars, foreign compute processes, resident seats, the presence guard and host
// RAM. Every read is best-effort except the card table, which is the point (a *CardTableError,
// after the one retry of Deps.CardTable); an unreadable extra reads as "nothing to report" (and,
// for foreign processes, is empty on Windows by nvidia-smi's own limit).
func BuildInput(ctx context.Context, m *gpulease.Manager, cfg config.Config, need Need, deps Deps) (gpulease.AllocInput, error) {
	deps = deps.withDefaults()
	cards, _, err := deps.CardTable(ctx, cfg)
	if err != nil {
		return gpulease.AllocInput{}, &CardTableError{Err: err}
	}
	in := gpulease.AllocInput{
		Cards:           cards,
		Quarantined:     m.QuarantinedCards(),
		ForeignBusy:     deps.ForeignBusy(ctx, cfg),
		Resident:        deps.Resident(ctx, cfg, cards),
		FootprintGiB:    need.VRAMGiB,
		HostNeedGiB:     need.RAMGiB,
		HostHeadroomGiB: cfg.GPUHostRAMHeadroom(),
	}
	in.WholeNodeHeld, in.Claimed = Claims(m, need)
	in.HostFreeGiB, in.HostFreeOK = deps.HostFreeRAM()
	known, away := deps.Presence(cfg)
	in.AllowDisplay = known && away
	// The display card, once presence opens it, keeps the desktop floor the display layer guards
	// with (display_floor_gib), read from the same declaration, so the allocator and the layer
	// cannot disagree about how much of that card is the desktop's.
	in.DisplayFloorGiB = cfg.DisplayFloorGiB()
	return in, nil
}

// ResidentSeats maps each card to the configured layer seats that are loaded in llama-swap right
// now and pinned to it. Pins are read as nvidia-smi indices or UUID prefixes (the placement
// package's own convention). Best-effort: a pin that does not resolve contributes nothing. client,
// when given, reads /running through it (the reserve verb's shared maintenance client); nil reads it
// through the llama-swap client, with a short timeout.
//
// An unreadable /running is not "nothing is loaded". The one card whose occupancy changes the ORDER
// is the display card, which a loaded twin makes a card with something to evict: read as empty it
// sorts first, and the allocator hands the desktop's card to a job while a twin may be sitting on it
// (the floor, which the allocator also applies, only holds back what does not fit). So when /running
// cannot be read the display layer's models are counted as loaded, each costed as a loaded twin is,
// and the display card sorts as occupied. Other layers' seats still contribute nothing then: nothing
// about them is conservative to assume, and the cost of treating a card as busy falls on the display
// card alone, which the allocator only opens while the operator is away.
func ResidentSeats(ctx context.Context, cfg config.Config, cards []gpuprobe.Card, client *http.Client) map[string]gpulease.ResidentInfo {
	if len(cfg.Layers) == 0 || cfg.Endpoint == "" {
		return map[string]gpulease.ResidentInfo{}
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	unreadable := func() map[string]gpulease.ResidentInfo {
		return ResidentFrom(cfg, cards, displayModels(cfg))
	}
	loaded := map[string]bool{}
	if client != nil {
		rows, err := seatload.Occupants(rctx, client, cfg.Endpoint)
		if err != nil {
			return unreadable()
		}
		for _, r := range rows {
			if leaving(r.State) {
				continue
			}
			loaded[strings.ToLower(r.Model)] = true
		}
	} else {
		c, err := swapclient.New(cfg.Endpoint, 3*time.Second)
		if err != nil {
			return unreadable()
		}
		rows, err := c.Running(rctx)
		if err != nil {
			return unreadable()
		}
		for _, r := range rows {
			if leaving(r.State) {
				continue
			}
			loaded[strings.ToLower(r.ID)] = true
		}
	}
	return ResidentFrom(cfg, cards, loaded)
}

// displayModels is every model the display layer can have loaded (its seats' own models and each router
// twin), as the set ResidentFrom reads, for the reading that cannot see what is loaded.
func displayModels(cfg config.Config) map[string]bool {
	out := map[string]bool{}
	for _, l := range cfg.Layers {
		if l.Name != placement.LayerDisplay {
			continue
		}
		for _, s := range l.Seats {
			if s.Model != "" {
				out[strings.ToLower(s.Model)] = true
			}
			for _, twin := range s.ModelMap {
				if twin != "" {
					out[strings.ToLower(twin)] = true
				}
			}
		}
	}
	return out
}

// leaving reports a llama-swap state that is on its way out and holds no seat for a new job.
func leaving(state string) bool {
	switch strings.ToLower(state) {
	case "stopped", "shutdown":
		return true
	}
	return false
}

// ResidentFrom is the mapping alone: which configured layer seats, among the loaded models
// (lower-cased ids), sit on which card. Pure, so it is tested without llama-swap.
//
// A seat is its model AND, for a router seat, every model_map twin: the display layer's rungs
// (gemma-4-e4b-display, gemma-4-e2b-display) are named only there, and a loaded twin sits on the
// display card as surely as the pair's seat sits on its own. Counting only s.Model left that card
// reading as empty, so it sorted first as the card with nothing to evict while it held a twin.
// The same walk over the twins is config.ModelPins's, which answers the other direction (model →
// pins) and is what the lease's unload list already uses.
//
// The cost of taking the card is what the loaded models hold on it: per loaded model, footprint_gib,
// else for the display layer's router (which declares only display_footprint_gib) that. The footprint
// is declared per seat and is what ONE of its models puts on the card (the two display twins are about
// 6.2 GiB each), so a router with both twins loaded costs both: taking the card evicts both, and the
// order prices the eviction it would really make.
func ResidentFrom(cfg config.Config, cards []gpuprobe.Card, loaded map[string]bool) map[string]gpulease.ResidentInfo {
	out := map[string]gpulease.ResidentInfo{}
	for _, l := range cfg.Layers {
		for _, s := range l.Seats {
			here := loadedNames(s, loaded)
			if len(here) == 0 {
				continue
			}
			pinned, perr := gpuprobe.ResolveCards(cards, s.DeviceList())
			if perr != nil {
				continue
			}
			cost := s.FootprintGiB
			if cost <= 0 {
				cost = s.DisplayFootprintGiB
			}
			cost *= float64(len(here))
			for _, c := range pinned {
				id := c.LeaseID()
				r := out[id]
				r.Seats = append(r.Seats, here...)
				r.CostGiB += cost
				out[id] = r
			}
		}
	}
	return out
}

// loadedNames lists the names of one seat that are loaded: its model, and each model_map twin (in
// name order, so the answer does not depend on map iteration). Compared lower-cased, as /running
// ids are.
func loadedNames(s config.LayerSeat, loaded map[string]bool) []string {
	var here []string
	if s.Model != "" && loaded[strings.ToLower(s.Model)] {
		here = append(here, s.Model)
	}
	twins := make([]string, 0, len(s.ModelMap))
	for _, twin := range s.ModelMap {
		if twin != "" {
			twins = append(twins, twin)
		}
	}
	sort.Strings(twins)
	for _, twin := range twins {
		if loaded[strings.ToLower(twin)] {
			here = append(here, twin)
		}
	}
	return here
}
