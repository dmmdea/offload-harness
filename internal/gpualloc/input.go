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
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
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
	// Cards reads the card table and a warning about the declared ComfyUI order.
	Cards func(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error)
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

func (d Deps) withDefaults() Deps {
	if d.Cards == nil {
		d.Cards = func(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return gpuprobe.ReadCards(cctx, cfg.GPUComfyOrder)
		}
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

// CardTable reads the card table the way the allocator does.
func (d Deps) CardTable(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
	return d.withDefaults().Cards(ctx, cfg)
}

// BuildInput assembles the allocator's input from live state: the card table, the live leases,
// quarantine sidecars, foreign compute processes, resident seats, the presence guard and host
// RAM. Every read is best-effort except the card table, which is the point; an unreadable extra
// reads as "nothing to report" (and, for foreign processes, is empty on Windows by nvidia-smi's
// own limit).
func BuildInput(ctx context.Context, m *gpulease.Manager, cfg config.Config, need Need, deps Deps) (gpulease.AllocInput, error) {
	deps = deps.withDefaults()
	cards, _, err := deps.Cards(ctx, cfg)
	if err != nil {
		return gpulease.AllocInput{}, fmt.Errorf("the card table: %w", err)
	}
	in := gpulease.AllocInput{
		Cards:           cards,
		Claimed:         map[string]bool{},
		Quarantined:     m.QuarantinedCards(),
		ForeignBusy:     deps.ForeignBusy(ctx, cfg),
		Resident:        deps.Resident(ctx, cfg, cards),
		FootprintGiB:    need.VRAMGiB,
		HostNeedGiB:     need.RAMGiB,
		HostHeadroomGiB: cfg.GPUHostRAMHeadroom(),
	}
	for _, l := range m.Leases() {
		if len(l.Devices) == 0 {
			in.WholeNodeHeld = true
		}
		for _, d := range l.Devices {
			in.Claimed[d] = true
		}
	}
	for id, on := range need.Claimed {
		if on {
			in.Claimed[id] = true
		}
	}
	in.HostFreeGiB, in.HostFreeOK = deps.HostFreeRAM()
	known, away := deps.Presence(cfg)
	in.AllowDisplay = known && away
	return in, nil
}

// ResidentSeats maps each card to the configured layer seats that are loaded in llama-swap right
// now and pinned to it. Pins are read as nvidia-smi indices or UUID prefixes (the placement
// package's own convention). Best-effort: an unreadable /running or a pin that does not resolve
// contributes nothing. client, when given, reads /running through it (the reserve verb's shared
// maintenance client); nil reads it through the llama-swap client, with a short timeout.
func ResidentSeats(ctx context.Context, cfg config.Config, cards []gpuprobe.Card, client *http.Client) map[string]gpulease.ResidentInfo {
	if len(cfg.Layers) == 0 || cfg.Endpoint == "" {
		return map[string]gpulease.ResidentInfo{}
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	loaded := map[string]bool{}
	if client != nil {
		rows, err := seatload.Occupants(rctx, client, cfg.Endpoint)
		if err != nil {
			return map[string]gpulease.ResidentInfo{}
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
			return map[string]gpulease.ResidentInfo{}
		}
		rows, err := c.Running(rctx)
		if err != nil {
			return map[string]gpulease.ResidentInfo{}
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
func ResidentFrom(cfg config.Config, cards []gpuprobe.Card, loaded map[string]bool) map[string]gpulease.ResidentInfo {
	out := map[string]gpulease.ResidentInfo{}
	for _, l := range cfg.Layers {
		for _, s := range l.Seats {
			if !loaded[strings.ToLower(s.Model)] {
				continue
			}
			pinned, perr := gpuprobe.ResolveCards(cards, s.DeviceList())
			if perr != nil {
				continue
			}
			for _, c := range pinned {
				id := c.LeaseID()
				r := out[id]
				r.Seats = append(r.Seats, s.Model)
				r.CostGiB += s.FootprintGiB
				out[id] = r
			}
		}
	}
	return out
}
