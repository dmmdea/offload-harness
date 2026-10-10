package pipeline

// MediaLaneFree: is this image call's local lane free right now? (P0 plan, S1; ADR 0082.)
//
// A media call on a box that has the lane used to wait for it however many other nodes of the fleet stood idle. The
// router (internal/mediaremote) now asks this first, and places the call on a matching idle node only when the answer
// is "not free". The question is answered by the pipeline, from the state its own admission reads, and it is a
// RELAXATION of that admission, by construction and by test:
//
//   - it checks only conditions that make the real wait-0 grant refuse (a lease on the cards the call would use, a
//     place in line ahead of it, an in-process slot, the host's memory, the allocator finding no card), each read with
//     the SAME helper the admission reads it with (planMedia, gpualloc.Claims and QueuedClaims, mediaSlots, the
//     grant's own host-RAM function, the allocator itself for a call that names no card), so it cannot say busy for a
//     lane the grant would have granted;
//   - it may say "free" for a lane the grant then refuses (a race, a card the allocator would skip at the grant, a
//     table it could not read): the call then runs locally as it did before this probe existed (after the probe's one bounded read), waits, and leaves its place in
//     line. Every doubt degrades toward today's behaviour and never toward a placement made on a guess.
//
// TestMediaLaneFreeNeverRefusesAGrant drives the real admission over a table of lane states and holds the prober to
// that. It is NOT an extraction of acquireCards (which interleaves the read and the grant over many lines that the
// host-RAM guard and the card-table retry both edit): nothing here edits it.
//
// It creates nothing: no lease, no place in line, no epoch, no waiter record, no ledger row, no PAIR card, no ComfyUI
// instance. And it removes nothing: the readers of the line it shares with the admission prune a record they find dead
// (an expired place in line, a waiter that stopped polling or whose process is gone) as housekeeping for the next
// reader, so the probe reads the lease root through a view that does not (gpulease.Manager.ReadOnly) and leaves such
// a record for a reader that is allowed to write. TestMediaLaneFreeWritesNothing snapshots the lease root over each
// state of the table, those records included.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// laneProbeBound is how long the probe may spend reading the card table before it gives up and calls the lane free
// (the call then reads the table itself, with its own retry). Chosen, not measured: a read that has not answered in
// this long is one the call must not wait behind twice.
const laneProbeBound = 4 * time.Second

var _ core.LaneProber = (*Pipeline)(nil)

// MediaLaneFree implements core.LaneProber for an image generation request.
func (p *Pipeline) MediaLaneFree(ctx context.Context, req core.Request) core.LaneVerdict {
	notJudged := func(why string) core.LaneVerdict { return core.LaneVerdict{Free: true, Why: "not judged: " + why} }
	if req.Task != core.TaskGenerateImage {
		return notJudged("only image generation is probed")
	}
	if strings.TrimSpace(req.Input) == "" {
		return notJudged("the request has no prompt")
	}
	// The binding the call would render with, resolved the way runGenerateImage resolves it.
	cfg, _, ferr := p.cfg.ResolveImageFamily(paramStr(req.Params, "family"))
	if ferr != nil {
		return notJudged(ferr.Error())
	}
	if cfg.ImageGenEngine == "sdcpp" || cfg.ImageGenScript == "" {
		return notJudged("this binding is not a ComfyUI one")
	}
	token := paramStr(req.Params, "waiter_token")
	need := imageNeed(cfg, token)

	// A call that runs under a lease its parent holds (`gpu reserve -- local-offload ...`) has a card chosen for it.
	if inherited, ierr := ambientLeaseEnv(); ierr != nil || inherited != nil {
		return notJudged("this process runs under a GPU lease it inherited, so the lane is its parent's")
	}
	m, err := p.scopedManager()
	if err != nil {
		return notJudged("the lease directory cannot be opened (" + err.Error() + "); the call reports it itself")
	}
	// Every read below goes through a view that prunes nothing: the question must not change the state it asks about.
	m = m.ReadOnly()
	// The card table, once, under a bound of its own. It sizes the host-RAM need (the call declares what does not fit
	// its card) and, on a card-scoped host, fixes the plan.
	rctx, cancel := context.WithTimeout(ctx, laneProbeBound)
	table, _, terr := p.alloc.CardTable(rctx, p.cfg)
	cancel()
	if terr != nil || len(table) == 0 {
		why := "it listed no card"
		if terr != nil {
			why = terr.Error()
		}
		return notJudged("the card table could not be read (" + why + "); the call reads it itself")
	}
	plan := mediaPlan{whole: true}
	if m.CardScoped() {
		pl, perr := planMedia(need, table, nil)
		if perr != nil {
			return notJudged(perr.Error())
		}
		plan = pl
	}

	r := &laneReading{p: p, m: m, cards: table, token: token}
	// The smallest declaration the call can make (its largest card): if that is refused, every card's would be.
	r.askRAM = p.declaredRAM(ctx, need, table, nil)
	switch {
	case plan.whole:
		r.wholeNode()
	case plan.auto:
		r.allocated(ctx, need)
	default:
		r.named(ctx, need, plan.ids)
	}
	return r.verdict()
}

// laneReading collects what a probe finds in the way.
type laneReading struct {
	p      *Pipeline
	m      *gpulease.Manager
	cards  []gpuprobe.Card
	token  string
	askRAM float64

	reasons []string
	holders []core.LaneHolder
	ahead   int
	hostRAM string
}

func (r *laneReading) verdict() core.LaneVerdict {
	if len(r.reasons) == 0 {
		return core.LaneVerdict{Free: true}
	}
	return core.LaneVerdict{Free: false, Why: strings.Join(r.reasons, "; "), Holders: r.holders, Ahead: r.ahead, HostRAM: r.hostRAM}
}

func (r *laneReading) block(reason string) { r.reasons = append(r.reasons, reason) }

// subjectOf says what the call would hold.
func subjectOf(ids []string) string {
	if len(ids) == 0 {
		return "the whole node"
	}
	return "card(s) " + strings.Join(ids, ", ")
}

// leasesIn records the live leases in the way of ids (nil = the whole node): a lease that names no cards is in the
// way of everything, one that names cards only of what it touches (the grant's own conflict rule, as queuedAnswerWhy
// words it).
func (r *laneReading) leasesIn(ids []string) {
	now := time.Now()
	before := len(r.holders)
	for _, l := range r.m.Leases() {
		if len(ids) != 0 && len(l.Devices) != 0 && !intersects(l.Devices, ids) {
			continue
		}
		h := core.LaneHolder{Epoch: l.Epoch, Class: string(l.Class), Reason: l.Reason, Devices: l.Devices}
		if !l.ExpiresAt.IsZero() {
			if s := int(l.ExpiresAt.Sub(now).Round(time.Second) / time.Second); s > 0 {
				h.RemainingSec = s
			}
		}
		r.holders = append(r.holders, h)
	}
	if len(r.holders) == before {
		return
	}
	first := r.holders[before]
	why := fmt.Sprintf("%s held by %s (%q)", subjectOf(ids), gpulease.Class(first.Class).LeasePhrase(), first.Reason)
	if first.RemainingSec > 0 {
		why += fmt.Sprintf(", about %ds of its declared term left", first.RemainingSec)
	}
	if n := len(r.holders) - before; n > 1 {
		why += fmt.Sprintf(" and %d more lease(s)", n-1)
	}
	r.block(why)
}

// placeSince is the arrival time of the place in line the call resumes (its waiter_token), zero when it resumes none. A
// call that resumes a place registers with the arrival time it left with (gpulease registerWaiter), so on the whole-node
// plan, which queues by arrival time (waiterBefore, tokenBlocks), a caller that joined the line after that place is
// BEHIND the call even though it is in the directory now.
func (r *laneReading) placeSince() time.Time {
	if r.token == "" {
		return time.Time{}
	}
	if tok, ok := r.m.ResumeToken(r.token); ok {
		return tok.Since()
	}
	return time.Time{}
}

// callersAhead records the places in line a new arrival for ids (nil = the whole node) would queue behind: the live
// waiters whose cards conflict with ids, except one that waits only on host RAM when the call declares none (G6: it
// does not hold its cards against such a call), and the live tokens (a place held for a caller who may come back) on
// those cards. The call's own token is never counted against it. since, when not zero, is the arrival time the call
// registers with (placeSince): only a caller that arrived strictly before it is ahead, as the lease queue itself
// orders them (a tie is not counted: the prober may only err toward "free"). The pinned and the allocated plans pass the
// zero time because their admission does not order by arrival: every other caller in line claims the cards before the
// call looks (gpualloc.QueuedClaims), a call that holds a place included.
func (r *laneReading) callersAhead(ids []string, since time.Time) {
	arrivedBefore := func(sinceMs int64) bool { return since.IsZero() || sinceMs < since.UnixMilli() }
	n := 0
	for _, w := range r.m.Waiters() {
		if r.token != "" && w.Token == r.token {
			continue
		}
		if arrivedBefore(w.SinceMs) && w.BlocksArrival(ids, r.askRAM) {
			n++
		}
	}
	for _, t := range r.m.Tokens() {
		if t.ID == r.token || !r.m.TokenLive(t) {
			continue
		}
		if arrivedBefore(t.SinceMs) && conflict(t.Devices, ids) {
			n++
		}
	}
	if n > 0 {
		r.ahead += n
		r.block(fmt.Sprintf("%d caller(s) hold a place in line ahead of this call for %s", n, subjectOf(ids)))
	}
}

// slots records an in-process job holding what the call needs: the lease directory shows nothing for a job that
// inherits its parent's lease.
func (r *laneReading) slots(ids []string) {
	switch {
	case mediaSlots.wholeHeld():
		r.block("another generation job in this process holds the whole node")
	case len(ids) == 0 && len(mediaSlots.held()) > 0:
		r.block("another generation job in this process holds a card")
	case anyHeld(mediaSlots.held(), ids):
		r.block("another generation job in this process holds " + subjectOf(ids))
	}
}

func anyHeld(held map[string]bool, ids []string) bool {
	for _, id := range ids {
		if held[id] {
			return true
		}
	}
	return false
}

// hostRAMFor puts the call's declared need, for the cards it would run on, to the node's host-RAM verdict.
func (r *laneReading) hostRAMFor(ctx context.Context, need mediaNeed, ids []string) {
	if chk := r.m.HostRAMCheck(r.p.declaredRAM(ctx, need, r.cards, ids)); !chk.OK {
		r.hostRAM = chk.Why
		r.block(chk.Why)
	}
}

// wholeNode reads the whole-node path: any live lease, any slot held in this process and any caller in line is in
// the way, and the host must take the declared need.
func (r *laneReading) wholeNode() {
	r.slots(nil)
	r.leasesIn(nil)
	r.callersAhead(nil, r.placeSince())
	if chk := r.m.HostRAMCheck(r.askRAM); !chk.OK {
		r.hostRAM = chk.Why
		r.block(chk.Why)
	}
}

// claimed is what is taken that the lease directory cannot show: the in-process slots and the cards callers in line
// are promised (the same two sets acquireCards counts).
func (r *laneReading) claimed() map[string]bool {
	c := mediaSlots.held()
	for id := range gpualloc.QueuedClaims(r.m, r.cards, r.token, r.askRAM > 0) {
		c[id] = true
	}
	return c
}

// named reads a plan whose cards are fixed (a pin, a pool, a declared device): they are taken, or they are not.
func (r *laneReading) named(ctx context.Context, need mediaNeed, ids []string) {
	whole, claims := gpualloc.Claims(r.m, gpualloc.Need{Claimed: r.claimed()})
	taken := whole || mediaSlots.wholeHeld()
	for _, id := range ids {
		if claims[id] {
			taken = true
		}
	}
	if !taken {
		r.hostRAMFor(ctx, need, ids)
		return
	}
	before := len(r.reasons)
	r.leasesIn(ids)
	r.slots(ids)
	r.callersAhead(ids, time.Time{})
	if len(r.reasons) == before { // taken by something that ended while it was being read
		r.block(subjectOf(ids) + " are held, or promised to callers ahead of this one")
	}
}

// allocated reads a call that names no card: the allocator chooses, from the same input the admission gives it, and
// one reading of the world (a wait of 0, so no polling).
func (r *laneReading) allocated(ctx context.Context, need mediaNeed) {
	if mediaSlots.wholeHeld() {
		r.slots(nil)
		return
	}
	stored := r.p.alloc
	table := r.cards
	stored.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return table, "", nil }
	build := func() (gpulease.AllocInput, error) {
		return gpualloc.BuildInput(ctx, r.m, r.p.cfg, gpualloc.Need{Claimed: r.claimed(), RAMGiB: r.askRAM}, stored)
	}
	ids, free, err := gpualloc.PickAuto(gpualloc.Plan{Min: 1, Max: 1}, 0, build, io.Discard, time.Sleep, time.Now)
	if err != nil {
		var none *gpulease.NoCardsError
		if errors.As(err, &none) {
			if none.HostReason != "" {
				r.hostRAM = none.HostReason
			}
			r.block("no card can take this call right now: " + gpualloc.SkipSummary(none))
		}
		// Any other failure is an input that could not be built (a table that stopped answering): nothing to refute.
		return
	}
	if !free {
		// Every card that qualifies is taken; the set the call would queue on says by whom.
		before := len(r.reasons)
		r.leasesIn(ids)
		r.slots(ids)
		r.callersAhead(ids, time.Time{})
		if len(r.reasons) == before {
			r.block(subjectOf(ids) + " are held, or promised to callers ahead of this one")
		}
		return
	}
	r.hostRAMFor(ctx, need, ids)
}
