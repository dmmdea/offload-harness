package main

// Which cards a `gpu reserve` holds (plan P3). The lease used to be whole-node: a job
// pinned to one card fenced all of them. Now a reservation can name its cards, ask for N
// of them, or take the set its wrapped command names, and a host that has not turned
// card-scoped leases on behaves exactly as before.
//
//	gpu reserve --devices 0,GPU-aaaa ...   named cards (nvidia-smi index or UUID prefix)
//	gpu reserve --cards 2 | 1..3 ...       the allocator picks (see internal/gpulease/allocator.go)
//	gpu reserve --whole-node ...           everything, as before
//	gpu reserve ... -- <cmd>               the cards <cmd> pins itself to, else whole node
//
// RULES, each pinned by a test:
//   - A host WITHOUT card-scoped leases never reads the card table for a reserve that
//     names nothing and never derives a device set: the lease is whole-node, byte for
//     byte as before. Naming cards there (--devices, --cards) is a hard error, not a
//     silent widening, because the reservation would not mean what it says.
//   - Evidence from the wrapped command that cannot be turned into a card degrades to a
//     whole-node lease WITH a note (the wider fence is the safe direction); it never
//     fails a reserve that worked before.
//   - Cards named with --devices are the operator's word: queued FIFO behind whoever
//     holds them, display card included. `--cards` never auto-assigns the display card
//     while the operator is at the desk (I6).
//   - A busy card is a place in line: `--cards N` that cannot be met right now queues FIFO
//     on a fixed set (the cards free right now first, topped up from the claimed ones that
//     would fit) or polls until enough cards qualify for a reason a lease does not
//     explain; it never refuses while --wait lasts. Elastic re-picking while queued is
//     the fan-out work (P13/P14), not here.
//   - Allocate and claim are one loop (acquireAutoCards): two reserves that read the same
//     free card must not both pick it, so the claim is a non-blocking acquire and a lost
//     one re-runs the allocator with the winner's claim visible.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// reserveDeviceFlags are the flags that choose the card set.
type reserveDeviceFlags struct {
	devices   string
	cards     string
	wholeNode bool
	vramGiB   float64
	ramGiB    float64
}

// devicePlan is the decision: a concrete set (IDs, nil = whole node), or an allocation
// still to be made (Auto, Min..Max).
type devicePlan struct {
	IDs      []string
	Auto     bool
	Min, Max int
	// Explicit is true when the operator named the cards (--devices) or asked for N of them
	// (--cards): the wrapped command is then confined to them (confineWrapped). A set
	// derived from the command's own pin, or a whole-node lease, is not.
	Explicit bool
	// Source says where the set came from, for the one line the verb prints.
	Source string
	// Note is a warning to print (a derivation that fell back to the whole node).
	Note string
}

// parseCardCount reads --cards: "N" or "MIN..MAX", at least 1.
func parseCardCount(s string) (min, max int, err error) {
	s = strings.TrimSpace(s)
	lo, hi, ranged := strings.Cut(s, "..")
	min, err = strconv.Atoi(strings.TrimSpace(lo))
	if err != nil || min < 1 {
		return 0, 0, fmt.Errorf("--cards %q: want a count of at least 1, or MIN..MAX", s)
	}
	max = min
	if ranged {
		max, err = strconv.Atoi(strings.TrimSpace(hi))
		if err != nil || max < min {
			return 0, 0, fmt.Errorf("--cards %q: the upper bound must be a count no smaller than the lower", s)
		}
	}
	return min, max, nil
}

// planReserveDevices applies the rules above. loadCards is called only when the plan
// needs the card table.
func planReserveDevices(f reserveDeviceFlags, cmdArgs []string, env func(string) string, scoped bool,
	loadCards func() ([]gpuprobe.Card, string, error)) (devicePlan, error) {
	chosen := 0
	for _, on := range []bool{f.devices != "", f.cards != "", f.wholeNode} {
		if on {
			chosen++
		}
	}
	if chosen > 1 {
		return devicePlan{}, errors.New("give only one of --devices, --cards and --whole-node")
	}
	if f.wholeNode {
		return devicePlan{Source: "--whole-node"}, nil
	}
	named := f.devices != "" || f.cards != ""
	if named && !scoped {
		return devicePlan{}, fmt.Errorf("%w: --devices and --cards name cards, which this host cannot write; "+
			"reserve the whole node with --whole-node (or no flag), or enable gpu_card_scoped_leases once `gpu doctor --write-audit` is green",
			gpulease.ErrCardScopedOff)
	}
	if !scoped {
		return devicePlan{}, nil // whole node, exactly as before this feature
	}

	if f.cards != "" {
		min, max, err := parseCardCount(f.cards)
		if err != nil {
			return devicePlan{}, err
		}
		if _, _, err := loadCards(); err != nil {
			return devicePlan{}, fmt.Errorf("--cards needs the card table and nvidia-smi gave none: %w", err)
		}
		return devicePlan{Auto: true, Explicit: true, Min: min, Max: max, Source: "--cards " + strings.TrimSpace(f.cards)}, nil
	}

	cards, note, cerr := loadCards()
	if f.devices != "" {
		if cerr != nil {
			return devicePlan{}, fmt.Errorf("--devices needs the card table to resolve %q and nvidia-smi gave none: %w", f.devices, cerr)
		}
		var keys []string
		for _, k := range strings.Split(f.devices, ",") {
			keys = append(keys, k)
		}
		got, err := gpuprobe.ResolveCards(cards, keys)
		if err != nil {
			return devicePlan{}, fmt.Errorf("--devices %q: %w", f.devices, err)
		}
		p := devicePlan{Explicit: true, Source: "--devices", Note: note}
		for _, c := range got {
			p.IDs = append(p.IDs, c.LeaseID())
		}
		return p, nil
	}

	// Default: what the wrapped command says about its own cards.
	if cerr != nil {
		if gpulease.WouldDerive(cmdArgs, env) {
			return devicePlan{Note: fmt.Sprintf("no card table (%v): the command names a card but it cannot be resolved, so this is a whole-node lease", cerr)}, nil
		}
		return devicePlan{}, nil
	}
	d, err := gpulease.DevicesFromCommand(cmdArgs, env, cards)
	if err != nil {
		return devicePlan{Note: fmt.Sprintf("%v; reserving as a whole-node lease instead (the wider fence is the safe one)", err)}, nil
	}
	if len(d.IDs) == 0 {
		return devicePlan{Note: note}, nil
	}
	return devicePlan{IDs: d.IDs, Source: "derived from " + d.Source, Note: note}, nil
}

// confinement is how the wrapped command is kept to the cards its lease holds.
type confinement struct {
	Env  []string // environment entries to add to the child
	Note string   // a warning to print
}

// confineWrapped decides how a command is kept to the cards its lease holds. THE LEASE
// ALONE CONFINES NOTHING: a lease on cards 0 and 2 leaves the held cards idle and lets an
// unpinned job run on CUDA's default fastest-first card, which on the reference box is the
// display card, the very card the allocator refuses to hand out.
//
// A reservation that NAMED its cards (--devices) or had them ALLOCATED (--cards) is the
// operator's statement of where the job runs, so a command that names no card of its own is
// pinned to them: CUDA_VISIBLE_DEVICES by driver UUID (the one id the three index spaces
// cannot disagree about) and CUDA_DEVICE_ORDER=PCI_BUS_ID (the spike's finding: what pins
// reliably).
//
// A command that pins itself is another matter, and the two kinds of pin differ:
//   - `--cuda-device` on its command line and COMFY_CUDA_DEVICE are the PROGRAM's own choice
//     (ComfyUI resets CUDA_VISIBLE_DEVICES from its flag), so overriding the variable would
//     be futile and a bare index the lease did not count the same way would land on the
//     wrong card: those are left alone, and the wrapper says so when the pin falls outside
//     the held cards or cannot be resolved.
//   - a CUDA_VISIBLE_DEVICES the command merely INHERITS (the operator's shell) is not a
//     choice about this job: when it reaches outside the held cards, or cannot be resolved,
//     it is REPLACED with the held cards (the wrapper's value is the last in the child's
//     environment, so the override is effective) and the wrapper says so. A pin inside the
//     held cards is tighter than the lease and is left alone.
//
// A set derived from the command's own pin, and a whole-node lease, are not confined here.
func confineWrapped(explicit bool, held, cmdArgs []string, env func(string) string, cards []gpuprobe.Card) confinement {
	if !explicit || len(held) == 0 {
		return confinement{}
	}
	pin := confinement{}
	uuids := make([]string, 0, len(held))
	for _, id := range held {
		uuids = append(uuids, driverUUID(id, cards))
	}
	pin.Env = []string{"CUDA_VISIBLE_DEVICES=" + strings.Join(uuids, ","), "CUDA_DEVICE_ORDER=PCI_BUS_ID"}
	if !gpulease.WouldDerive(cmdArgs, env) {
		return pin
	}

	var problem string
	d, err := gpulease.DevicesFromCommand(cmdArgs, env, cards)
	if err != nil {
		problem = fmt.Sprintf("the wrapper cannot confirm the command stays within the cards this lease holds (%s): %v", strings.Join(held, ", "), err)
	} else {
		var outside []string
		for _, id := range d.IDs {
			if !containsID(held, id) {
				outside = append(outside, id)
			}
		}
		if len(outside) == 0 {
			return confinement{}
		}
		problem = fmt.Sprintf("the command pins itself (%s) to %s, outside the cards this lease holds (%s)",
			d.Source, strings.Join(outside, ", "), strings.Join(held, ", "))
	}
	if raw := strings.TrimSpace(env("CUDA_VISIBLE_DEVICES")); raw != "" && raw != "-1" {
		pin.Note = fmt.Sprintf("%s; the inherited CUDA_VISIBLE_DEVICES=%s was replaced with the held cards", problem, raw)
		return pin
	}
	return confinement{Note: problem + ": it is not confined to the lease and may trespass on cards another lease can be granted"}
}

// driverUUID is the UUID as the driver reports it for a lease id (the lease records it
// lower-cased). With no card table it is rebuilt: the driver's form is "GPU-" and the
// lower-case hex the lease id already carries.
func driverUUID(id string, cards []gpuprobe.Card) string {
	for _, c := range cards {
		if c.LeaseID() == id {
			return strings.TrimSpace(c.UUID)
		}
	}
	if rest, ok := strings.CutPrefix(id, "gpu-"); ok {
		return "GPU-" + rest
	}
	return id
}

func containsID(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// --cards N: allocate, or queue
// ---------------------------------------------------------------------------

// pickAutoCards turns an Auto plan into a concrete set, from live state. free reports which
// kind of set it is: true = cards that qualify and are free right now (the caller claims
// them and, if another reserve got there first, picks again); false = no qualifying set is
// free, and ids is the fixed set to queue FIFO on (the free cards first, topped up from the
// cards a live lease claims). When too few cards qualify for a reason waiting for a lease
// does not fix (a display card, quarantine, host RAM, VRAM) it polls until they do or wait
// runs out.
func pickAutoCards(plan devicePlan, wait time.Duration, build func() (gpulease.AllocInput, error),
	out io.Writer, sleep func(time.Duration), now func() time.Time) (ids []string, free bool, err error) {
	deadline := now().Add(wait)
	told := false
	for {
		in, err := build()
		if err != nil {
			return nil, false, err
		}
		in.Min, in.Max = plan.Min, plan.Max
		a, aerr := gpulease.Allocate(in)
		if aerr == nil {
			return a.Devices, true, nil
		}
		var none *gpulease.NoCardsError
		if !errors.As(aerr, &none) {
			return nil, false, aerr
		}
		if none.HostReason == "" && len(none.Waitable) >= plan.Min {
			target := none.Waitable[:plan.Min]
			fmt.Fprintf(out, "gpu reserve: no %d card(s) are free right now; queueing for %s (%s)\n", plan.Min, strings.Join(target, ", "), skipSummary(none))
			return target, false, nil
		}
		if wait <= 0 || !now().Before(deadline) {
			return nil, false, fmt.Errorf("%w; pass --wait <duration> to keep waiting for cards to qualify", none)
		}
		if !told {
			told = true
			fmt.Fprintf(out, "gpu reserve: fewer than %d card(s) qualify (%s); waiting up to %s for that to change\n", plan.Min, skipSummary(none), wait)
		}
		sleep(2 * time.Second)
	}
}

// acquireAutoCards allocates AND claims, as one loop, for a `--cards` request: the
// allocator reads live state over a window of seconds, so another reserve can take the card
// it picked before this one claims it (two simultaneous `--cards 1` over free cards both
// pick the lowest id). The claim is therefore a non-blocking acquire; when it loses, the
// winner's claim is visible and the allocator runs again, and the request queues FIFO only
// when the allocator itself says no qualifying set is free.
func acquireAutoCards(m *gpulease.Manager, class gpulease.Class, opts gpulease.Options, plan devicePlan, wait time.Duration,
	build func() (gpulease.AllocInput, error), out io.Writer, sleep func(time.Duration), now func() time.Time) (*gpulease.Lease, error) {
	deadline := now().Add(wait)
	remaining := func() time.Duration {
		if wait <= 0 {
			return 0
		}
		if r := deadline.Sub(now()); r > 0 {
			return r
		}
		return 0
	}
	for lost := 0; ; lost++ {
		ids, free, err := pickAutoCards(plan, remaining(), build, out, sleep, now)
		if err != nil {
			return nil, err
		}
		opts.Devices = ids
		// Every lost claim means a competitor took cards in the meantime, so the next read
		// sees them and the loop makes progress; the cap only bounds a pathological churn, after
		// which the picked set is queued on like any other.
		if !free || lost >= maxAutoClaimRetries {
			return acquireQueued(m, class, opts, remaining())
		}
		lease, err := m.TryAcquire(class, opts)
		var held *gpulease.ErrHeld
		switch {
		case err == nil:
			return lease, nil
		case errors.As(err, &held):
			continue // another reserve claimed one of these first; allocate again with its claim visible
		default:
			return nil, err
		}
	}
}

// maxAutoClaimRetries bounds how many times a `--cards` request re-allocates after losing a
// claim race before it queues on the set it last picked.
const maxAutoClaimRetries = 16

func skipSummary(e *gpulease.NoCardsError) string {
	var parts []string
	if e.HostReason != "" {
		parts = append(parts, e.HostReason)
	}
	for _, s := range e.Skipped {
		parts = append(parts, fmt.Sprintf("card %d %s", s.Index, s.Reason))
	}
	if len(parts) == 0 {
		return "no card qualifies"
	}
	return strings.Join(parts, ", ")
}

// Seams for the live reads behind the allocator's input, so tests assemble a host.
var (
	foreignBusyFn   = foreignBusyByCard
	residentSeatsFn = residentSeatsByCard
	hostFreeRAMFn   = gpuprobe.HostFreeRAMGiB
)

// buildAllocInput assembles the allocator's input from live state: the card table, the
// live leases, quarantine sidecars, foreign compute processes, resident seats, the
// presence guard and host RAM. Every read is best-effort except the card table, which is
// the point; an unreadable extra reads as "nothing to report" (and, for foreign
// processes, is empty on Windows by nvidia-smi's own limit).
func buildAllocInput(ctx context.Context, m *gpulease.Manager, cfg config.Config, f reserveDeviceFlags) (gpulease.AllocInput, error) {
	cards, _, err := cardTable(ctx, cfg)
	if err != nil {
		return gpulease.AllocInput{}, fmt.Errorf("the card table: %w", err)
	}
	in := gpulease.AllocInput{
		Cards:           cards,
		Claimed:         map[string]bool{},
		Quarantined:     m.QuarantinedCards(),
		ForeignBusy:     foreignBusyFn(ctx, cfg),
		Resident:        residentSeatsFn(ctx, cfg, cards),
		FootprintGiB:    f.vramGiB,
		HostNeedGiB:     f.ramGiB,
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
	in.HostFreeGiB, in.HostFreeOK = hostFreeRAMFn()
	p := placement.ProbePresence(cfg.PresenceMode(), cfg.OperatorIdle())
	in.AllowDisplay = p.Known && p.Away
	return in, nil
}

// foreignBusyByCard maps a card (lease id) to the first non-harness compute process the
// driver lists on it. On Windows (WDDM) nvidia-smi lists no per-process memory, so this
// is empty there: foreign-busy is Linux-only evidence today.
func foreignBusyByCard(ctx context.Context, cfg config.Config) map[string]string {
	out := map[string]string{}
	pctx, cancel := context.WithTimeout(ctx, foreignDisplayProbeTimeout*2)
	defer cancel()
	procs, err := gpuactivity.SampleProcesses(pctx)
	if err != nil {
		return out
	}
	min := effectiveForeignMinMiB(cfg)
	for _, p := range procs {
		if !p.UsedKnown || p.UsedMiB < min || isHarnessOwned(p.Name) || p.GPUUUID == "" {
			continue
		}
		id := strings.ToLower(p.GPUUUID)
		if _, dup := out[id]; !dup {
			out[id] = fmt.Sprintf("%s pid %d, %d MiB", shortProcessName(p.Name), p.PID, p.UsedMiB)
		}
	}
	return out
}

// residentSeatsByCard maps each card to the configured layer seats that are loaded in
// llama-swap right now and pinned to it. Pins are read as nvidia-smi indices or UUID
// prefixes (the placement package's own convention). Best-effort: an unreadable
// /running or a pin that does not resolve contributes nothing.
func residentSeatsByCard(ctx context.Context, cfg config.Config, cards []gpuprobe.Card) map[string]gpulease.ResidentInfo {
	out := map[string]gpulease.ResidentInfo{}
	if len(cfg.Layers) == 0 || cfg.Endpoint == "" {
		return out
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := seatload.Occupants(rctx, maintenanceClient, cfg.Endpoint)
	if err != nil {
		return out
	}
	loaded := map[string]bool{}
	for _, r := range rows {
		switch strings.ToLower(r.State) {
		case "stopped", "shutdown":
			continue
		}
		loaded[strings.ToLower(r.Model)] = true
	}
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
