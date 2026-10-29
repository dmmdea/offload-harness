package pipeline

// The media twin of F82: where a render lease is compared to the host's memory when its own unload frees that memory.
//
// A render's lease was admitted against host RAM INSIDE Acquire, and the llama-swap models the render unloads were unloaded
// only afterwards, by the node runner (render/gpu-lock.mjs withGpuSlot: claimUnload, freeLlamaSwap). The admission counted
// the memory the render was about to free as held, waited for it and deferred the render. The incident (2026-10-24): on a
// 6 GB node a 4B llama-swap seat held 8 GiB of commit; the sd.cpp render declared 6.5 GiB and was told "waiting for host
// RAM: needs 6.5 GiB, committed 18.4 of 31.2 GiB physical, 8.0 GiB headroom", and never ran. `gpu reserve --unload-seat`
// had the same defect and was fixed first (gpu_hostram_await.go): take the cards declaring nothing, unload under the
// lease, then ask the host. The media path now follows that order, with one new step, because the unload is not the CLI's:
//
//  1. hostRAMDeferral decides, before the grant, whether the lease defers its figure: the host is short for it now and at
//     least one llama-swap model its own unload takes is RESIDENT (or residency cannot be read);
//  2. the grant takes the cards declaring nothing (Options.HostRAMWaitGiB carries the figure; the allocator's host term and
//     the queue's pass rule see a call that declares nothing);
//  3. clearForHostRAM unloads those models, with a drain, under the lease and its heartbeat;
//  4. Lease.AwaitHostRAM puts the one admission rule to the figure, still holding the cards, bounded by hostRAMWaitEnd;
//     past the bound the lease is released and the call gets the refusal it always got.
//
// The runner's own unload still runs afterwards (it is the backstop for whatever the clear could not take), and no marker
// or warm-back is written here: a media lease owes the seat nothing.

import (
	"context"
	"log"
	"strings"
	"time"

	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// mediaHostRAMFloor is the least a deferring lease waits for the host after its clear. Memory a llama-swap unload frees
// returns when the process exits, which takes seconds, so a caller that asked to fail fast (gpu_wait_ms 0, or a window the
// queue already spent) must still give it that long: a miss holds the cards for this long and no longer than the hold bound.
const mediaHostRAMFloor = 30 * time.Second

// mediaClearReadTTL is how long the lane probe trusts a roster and residency reading. The probe runs on every scan tick and
// each reading is two HTTP requests; the grant never uses the cache.
const mediaClearReadTTL = 5 * time.Second

// mediaClearDrain bounds the drain of one model of the clear (llamaswap UnloadOpts.DrainTimeout).
const mediaClearDrain = 30 * time.Second

const (
	unloadEnvKey  = "GPU_LEASE_UNLOAD_MODELS="
	unloadEnvNone = unloadEnvKey + "-"
)

// hostRAMDeferral is the decision for one call: its declared host RAM, and whether it is compared to the host in the grant (the
// order the media path always had) or after the clear (the F82 order).
type hostRAMDeferral struct {
	declared float64
	// commit is the commit the estimate states beside declared (hostneed.Need.CommitStated), 0 when it states none: the lease then takes
	// the working set as its commit. It follows declared through gate and later.
	commit float64
	defers   bool
	// unload is the GPU_LEASE_UNLOAD_MODELS entry read for a lease that defers ("" = a whole-node call, or a card call whose
	// roster could not be read: the runner's rule, every model off the memory stack). The grant hands the same one to the runner.
	unload string
}

// gate is what the grant compares to the host and declares: nothing for a call that defers.
func (d hostRAMDeferral) gate() float64 {
	if d.defers {
		return 0
	}
	return d.declared
}

// later is what is asked of the host after the clear.
func (d hostRAMDeferral) later() float64 {
	if d.defers {
		return d.declared
	}
	return 0
}

// gateLoad is the two figures the grant compares to the host for this call: the working set, and its commit, stated when the estimate stated one.
func (d hostRAMDeferral) gateLoad() gpuprobe.HostLoad {
	return hostLoadOf(d.gate(), d.gateCommit())
}

// hostLoadOf is the host load a figure and the commit stated beside it (0 = none stated) declare: the one figure stands in for an unstated commit.
func hostLoadOf(gib, commit float64) gpuprobe.HostLoad {
	if commit > 0 && gib > 0 {
		return gpuprobe.HostLoad{TouchGiB: gib, CommitGiB: commit, CommitStated: true}
	}
	return gpuprobe.HostLoad{TouchGiB: gib, CommitGiB: gib}
}

// gateCommit is the stated commit the grant compares: nothing for a call that defers, as gate.
func (d hostRAMDeferral) gateCommit() float64 {
	if d.defers {
		return 0
	}
	return d.commit
}

// laterCommit is the stated commit asked of the host after the clear.
func (d hostRAMDeferral) laterCommit() float64 {
	if d.defers {
		return d.commit
	}
	return 0
}

// mediaClearRead is what one reading of the llama-swap roster and /running says about the lease's unload set.
type mediaClearRead struct {
	at     time.Time
	unload string
	// known: /running could be read, so victims is the answer and not a guess.
	known bool
	// victims are the resident models of the lease's unload set.
	victims []string
}

// hostRAMDeferral decides whether a call that declares `declared` GiB, on cards ids (nil = the whole node), defers it until its
// own unload has run. The grant and the lane probe both ask it, so a ticket the grant would admit is never reported as held
// by host RAM. It defers only when ALL of these hold, and otherwise the call keeps the order it always had (gate = declared,
// later = 0):
//
//   - it declares something and a llama-swap endpoint is configured (without one there is nothing to unload, and the cards
//     are never held for a shortage no unload can cure);
//   - the host is short for the figure now, but not impossibly and not unreadably (port of screenDeferredHostRAM: a need no
//     state of the box admits is refused at once by the grant, before anything is taken or unloaded, and a host whose memory
//     cannot be read cannot show that the clear made room, so it keeps the grant's comparison);
//   - at least one model of the lease's unload set is resident. The set is the card-scoped list (unloadEnv) or, for the whole
//     node and for a list that could not be made, every resident model off the memory stack: the runner's rule. When
//     /running cannot be read the rule of doubt applies and the call defers, unless the list is the none sentinel.
//
// maxAge 0 reads fresh (the grant); above 0 a reading younger than that is reused (the probe).
func (p *Pipeline) hostRAMDeferral(ctx context.Context, m *gpulease.Manager, declared float64, ids []string, cards []gpuprobe.Card, maxAge time.Duration) hostRAMDeferral {
	return p.hostRAMDeferralLoad(ctx, m, declared, 0, ids, cards, maxAge)
}

// hostRAMDeferralLoad is hostRAMDeferral for a call whose estimate states its commit (commit above 0): the host is asked for the same
// two figures the grant will ask, the commit stated and so judged against the commit limit.
func (p *Pipeline) hostRAMDeferralLoad(ctx context.Context, m *gpulease.Manager, declared, commit float64, ids []string, cards []gpuprobe.Card, maxAge time.Duration) hostRAMDeferral {
	d := hostRAMDeferral{declared: declared, commit: commit}
	if declared <= 0 || m == nil || strings.TrimSpace(p.cfg.Endpoint) == "" {
		return d
	}
	chk := m.HostRAMCheckLoad(hostLoadOf(declared, commit))
	switch {
	case chk.OK, chk.Impossible:
		return d
	case chk.Unreadable:
		planNoteKeyed("hostram-deferral-unreadable", "the host's memory cannot be read, so nothing can show that the render's own unload makes room for its host RAM: it is compared in the grant, as for a render that unloads nothing")
		return d
	}
	read := p.mediaClearRead(ctx, ids, cards, maxAge)
	if read.unload == unloadEnvNone || (read.known && len(read.victims) == 0) {
		return d
	}
	d.defers, d.unload = true, read.unload
	return d
}

// mediaClearRead reads the unload list and what is resident of it, from the cache when maxAge allows.
func (p *Pipeline) mediaClearRead(ctx context.Context, ids []string, cards []gpuprobe.Card, maxAge time.Duration) mediaClearRead {
	key := p.cfg.Endpoint + "|" + strings.Join(ids, ",")
	if maxAge > 0 {
		p.clearMu.Lock()
		c, ok := p.clearReads[key]
		p.clearMu.Unlock()
		if ok && time.Since(c.at) < maxAge {
			return c
		}
	}
	r := mediaClearRead{at: time.Now(), unload: p.unloadEnv(ctx, ids, cards)}
	if r.unload != unloadEnvNone {
		r.victims, r.known = p.residentUnloadSet(ctx, r.unload)
	}
	if maxAge > 0 {
		p.clearMu.Lock()
		if p.clearReads == nil {
			p.clearReads = map[string]mediaClearRead{}
		}
		p.clearReads[key] = r
		p.clearMu.Unlock()
	}
	return r
}

// residentUnloadSet is the models llama-swap's /running shows loaded that the lease's unload takes: those the list names, or,
// without a list, every one off the memory stack (what freeLlamaSwap does in the runner). known is false when /running could
// not be read.
func (p *Pipeline) residentUnloadSet(ctx context.Context, unload string) (victims []string, known bool) {
	c, err := swapclient.New(p.cfg.Endpoint, 5*time.Second)
	if err != nil {
		return nil, false
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	running, err := c.Running(rctx)
	if err != nil {
		return nil, false
	}
	var listed []string
	if strings.HasPrefix(unload, unloadEnvKey) && unload != unloadEnvNone {
		listed = strings.Split(strings.TrimPrefix(unload, unloadEnvKey), ",")
	}
	stack := gpualloc.EffectiveMemoryStack(p.cfg)
	for _, r := range running {
		switch {
		case listed != nil && containsFold(listed, r.ID):
			victims = append(victims, r.ID)
		case listed == nil && !containsFold(stack, r.ID):
			victims = append(victims, r.ID)
		}
	}
	return victims, true
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), s) {
			return true
		}
	}
	return false
}

// askRAMGate is askRAMFor for the readers that run before the card is held (the allocator's pre-filter, QueuedClaims, the lane
// probe): the figure the GRANT will compare, which is 0 for a call that defers. A plan that names no card (the whole node, an
// allocator pick) is judged on the whole-node unload set, a superset of any card's: the answer can only err toward "nothing
// in the way", and the grant then decides on the card it takes.
func (p *Pipeline) askRAMGate(ctx context.Context, m *gpulease.Manager, need mediaNeed, plan mediaPlan, cards []gpuprobe.Card, maxAge time.Duration) float64 {
	var ids []string
	if !plan.auto && !plan.whole {
		ids = plan.ids
	}
	return p.hostRAMDeferral(ctx, m, p.askRAMFor(ctx, need, plan, cards), ids, cards, maxAge).gate()
}

// clearForHostRAM unloads, with a drain, the resident models of the lease's unload set, under the lease it holds. Every failure
// is logged and none is fatal: a failure only costs the wait the clear was meant to save, and the runner's own unload still
// runs. It writes no marker and owes no warm-back (a media lease owes the seat nothing).
func (p *Pipeline) clearForHostRAM(ctx context.Context, lease *gpulease.Lease, d hostRAMDeferral) {
	victims, known := p.residentUnloadSet(ctx, d.unload)
	if !known {
		log.Printf("media admission: lease epoch %d holds its cards for %.1f GiB of host RAM, and llama-swap's /running could not be read: nothing was unloaded before the host was asked (the runner's own unload still runs)", lease.Epoch(), d.declared)
		return
	}
	if len(victims) == 0 {
		return
	}
	c, err := swapclient.New(p.cfg.Endpoint, 10*time.Second)
	if err != nil {
		log.Printf("media admission: the llama-swap client could not be built (%v): nothing was unloaded before the host was asked", err)
		return
	}
	for _, id := range victims {
		res, err := c.Unload(ctx, id, &llamaswap.UnloadOpts{Drain: true, DrainTimeout: mediaClearDrain})
		switch {
		case err != nil:
			log.Printf("media admission: lease epoch %d: %s was not unloaded before the host was asked: %v", lease.Epoch(), id, err)
		case res != nil && res.WasRunning:
			log.Printf("media admission: lease epoch %d unloaded %s (drained) so its %.1f GiB of host RAM is not counted against memory the unload frees", lease.Epoch(), id, d.declared)
		}
	}
}

// hostRAMWaitEnd is when the wait for the host ends: the queue budget (the call's gpu_wait_ms from when it began to queue),
// never under the floor after the clear, and never past the hold bound counted from the end of the clear (0 = no hold bound).
func hostRAMWaitEnd(start time.Time, wait time.Duration, clearEnd time.Time, hold time.Duration) time.Time {
	eff := start.Add(wait)
	if floor := clearEnd.Add(mediaHostRAMFloor); floor.After(eff) {
		eff = floor
	}
	if hold > 0 {
		if bound := clearEnd.Add(hold); bound.Before(eff) {
			eff = bound
		}
	}
	return eff
}

// clearThenAdmitHostRAM runs the clear and then puts the lease's declared figure to the host, still holding the cards (the
// heartbeat is already running: the wait can be minutes). A nil error means the figure is now the lease's declared need. The
// caller releases the lease on any error and answers with the refusal shape the grant always gave.
func (p *Pipeline) clearThenAdmitHostRAM(ctx context.Context, lease *gpulease.Lease, d hostRAMDeferral, start time.Time) error {
	p.clearForHostRAM(ctx, lease, d)
	eff := hostRAMWaitEnd(start, p.gpuWait(), time.Now(), p.cfg.GPUHostRAMHoldMax())
	return lease.AwaitHostRAM(gpulease.AwaitHostRAMOptions{
		Deadline:  eff,
		HoldUntil: eff,
		Cancel:    ctx.Done(),
		OnWait: func(e *gpulease.ErrHostRAM) {
			log.Printf("media admission: %s: the unload is done and the cards are held; waiting up to %s for the host to have the room (then the lease is released and nothing runs under it)", e.Error(), time.Until(eff).Round(time.Second))
		},
	})
}
