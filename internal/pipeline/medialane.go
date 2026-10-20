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
	"log"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// laneProbeBound is how long the probe's FIRST read of the card table may take, and laneProbeRetryBound the one retry
// that follows a read that ran out of time (gpualloc.Deps.CardTable's rule: only a timeout is retried, a failure that is
// not one fails the same way again). A table that does not read in either is one the probe gives up on and calls the
// lane not judged, with one log line (the call then reads the table itself, with the admission's 5 s + 15 s). Both are
// CHOSEN, not measured: 4 s is what the probe has always allowed one read; the retry is 10 s, shorter than the
// admission's 15 s because the probe is asked before every image call and a ticket scan asks it once per waiting ticket,
// so its worst case is 14 s per question, not 20. Before this the probe wrapped the caller's context in the 4 s, which made
// the retry unreachable (a retry needs the caller's context to be good when the attempt ends), so a nvidia-smi that was
// merely slow under load made every probe answer "not judged" at 4 s, silently (F55). They are variables for the tests.
var (
	laneProbeBound      = 4 * time.Second
	laneProbeRetryBound = 10 * time.Second
)

var _ core.LaneProber = (*Pipeline)(nil)

// laneOf is what a question about the lane needs to know before it reads the line: the binding the call would render
// with, its need, the lease directory's manager, the card table (read once) and the plan.
type laneOf struct {
	need  mediaNeed
	token string
	// m is the live manager of the lease directory; a question that only reads works on m.ReadOnly().
	m     *gpulease.Manager
	table []gpuprobe.Card
	plan  mediaPlan
}

// laneCardTable reads the card table for a lane question: one read under laneProbeBound and, when that read ran out of time
// while the caller still wanted the answer, one more under laneProbeRetryBound (gpualloc.Deps.CardTable's rule, which only
// works when the bounds are the Deps' own and not a deadline wrapped around the caller's context: a read that ends because
// the CALLER's context ended is not retried). A bound the Deps already carry (a test's) is kept. A table that is not
// read, or lists no card, comes back as an error and leaves one line in the log: the verdict that follows is "not judged"
// and the router does not log its reason, so without the line a call that waited while another node sat idle had no trace of
// why (F55). kind says which probe asked.
func (p *Pipeline) laneCardTable(ctx context.Context, kind string) ([]gpuprobe.Card, error) {
	deps := p.alloc
	if deps.ReadDeadline <= 0 {
		deps.ReadDeadline = laneProbeBound
	}
	if deps.RetryDeadline <= 0 {
		deps.RetryDeadline = laneProbeRetryBound
	}
	table, _, err := deps.CardTable(ctx, p.cfg)
	if err == nil && len(table) == 0 {
		err = errors.New("it listed no card")
	}
	if err != nil {
		// A read that ended because the CALLER went away (a client that disconnected mid-probe) says nothing about the
		// table, and the line is the only trace of a real nvidia-smi fault: it is not written for a cancelled caller. Nor is it
		// written again for a fault it just reported: a table that stays unreadable fails every question the same way, and a
		// ticket scan asks once per waiting ticket every 10 s, so the line is written for the first occurrence and then once a
		// minute per probe and cause while the fault lasts (laneFaultLog).
		if ctx.Err() == nil && p.laneFaults.due(kind+"|"+err.Error(), p.now()) {
			log.Printf("media lane probe (%s): the card table could not be read (%v); the lane is reported as not judged, so the router decides without a reading of this machine's lane and the call reads the table itself", kind, err)
		}
		return nil, err
	}
	return table, nil
}

// laneFaultEvery is how long the line for a card-table fault stays unwritten after it was written. It is CHOSEN, not measured: a
// ticket scan runs every 10 s, so a minute turns the lines of six scans of one fault into one and still says, once a minute, that
// the fault lasts.
const laneFaultEvery = time.Minute

// laneFaultLog remembers when the line for each fault of the lane probe's card-table read was last written. The zero value is
// ready. It belongs to a Pipeline (the fleet-serve process has one), so a test that builds its own starts with nothing written.
type laneFaultLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// due reports whether the line for key (the probe and the text of the fault) is to be written at now, and notes that it was. The
// first occurrence is always due, the same one again inside laneFaultEvery is not, and another fault is another key and is due at
// once. Entries older than the window are dropped here, so a fault whose text keeps changing cannot grow the map.
func (l *laneFaultLog) due(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, at := range l.last {
		if now.Sub(at) >= laneFaultEvery {
			delete(l.last, k)
		}
	}
	if _, quiet := l.last[key]; quiet {
		return false
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	l.last[key] = now
	return true
}

// resolveLane resolves an image request against this box. why is not empty when the box cannot judge the request
// (a task it does not model, a binding that is not a ComfyUI one, a lease the process inherited, a lease directory or a
// card table that would not open or read): the sentence is the reason, and every caller degrades toward the way the call
// ran before the question existed. unread is true only for the reasons that are a READ that failed (the lease directory,
// the card table, a plan the table cannot make): there the lane is in question and the box could not look at it, which the
// router must not take for a free lane (core.LaneVerdict.Unjudged). The others say the lane is not in question at all.
func (p *Pipeline) resolveLane(ctx context.Context, req core.Request) (ln laneOf, why string, unread bool) {
	if req.Task != core.TaskGenerateImage {
		return ln, "only image generation is probed", false
	}
	if strings.TrimSpace(req.Input) == "" {
		return ln, "the request has no prompt", false
	}
	// The binding the call would render with, resolved the way runGenerateImage resolves it.
	cfg, _, ferr := p.cfg.ResolveImageFamily(paramStr(req.Params, "family"))
	if ferr != nil {
		return ln, ferr.Error(), false
	}
	if cfg.ImageGenEngine == "sdcpp" || cfg.ImageGenScript == "" {
		return ln, "this binding is not a ComfyUI one", false
	}
	ln.token = paramStr(req.Params, "waiter_token")
	ln.need = imageNeed(cfg, ln.token).forCanvas(req.Params)
	if ln.need.Refusal != "" {
		return ln, ln.need.Refusal, false // the admission refuses it before any lease; the lane is not in question
	}

	// A call that runs under a lease its parent holds (`gpu reserve -- local-offload ...`) has a card chosen for it.
	if inherited, ierr := ambientLeaseEnv(); ierr != nil || inherited != nil {
		return ln, "this process runs under a GPU lease it inherited, so the lane is its parent's", false
	}
	m, err := p.scopedManager()
	if err != nil {
		return ln, "the lease directory cannot be opened (" + err.Error() + "); the call reports it itself", true
	}
	ln.m = m
	// The card table, read under the probe's bounds (laneCardTable). It sizes the host-RAM need (the call declares what does
	// not fit its card) and, on a card-scoped host, fixes the plan.
	table, terr := p.laneCardTable(ctx, "image")
	if terr != nil {
		return ln, "the card table could not be read (" + terr.Error() + "); the call reads it itself", true
	}
	ln.table = table
	ln.plan = mediaPlan{whole: true}
	if m.CardScoped() {
		pl, perr := planMedia(ln.need, table, nil)
		if perr != nil {
			return ln, perr.Error(), true
		}
		ln.plan = pl
	}
	return ln, "", false
}

// MediaLaneFree implements core.LaneProber for an image generation request, and for a whole-node run_graph (medialane_graph.go,
// which answers its own question and writes nothing).
//
// Two questions, told apart by whether the request resumes a place in line (a waiter_token that can still be resumed).
// A NEW arrival asks whether a wait-0 try would be granted, which is the relaxation described above. A call that
// RESUMES a place asks whether it would be served before anyone who came after its place, so on every plan only the
// callers that arrived before the place are ahead of it (the ticket queue's question, P0 plan S2): the lease queue
// serves a resumed place by arrival time, so a caller behind it never holds the card against it.
func (p *Pipeline) MediaLaneFree(ctx context.Context, req core.Request) core.LaneVerdict {
	if req.Task == core.TaskRunGraph {
		return p.graphLaneFree(ctx, req)
	}
	ln, why, unread := p.resolveLane(ctx, req)
	if why != "" {
		return core.LaneVerdict{Free: true, Why: "not judged: " + why, Unjudged: unread}
	}
	// Every read below goes through a view that prunes nothing: the question must not change the state it asks about.
	m := ln.m.ReadOnly()
	need, plan := ln.need, ln.plan

	r := &laneReading{p: p, m: m, cards: ln.table, token: ln.token}
	r.since = r.placeSince()
	// The declaration the call makes on the cards its plan names, else the smallest it can make (its largest card): if that is
	// refused, every card's would be. The admission sizes it the same way (askRAMFor).
	r.askRAM = p.askRAMFor(ctx, need, plan, ln.table)
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
	p     *Pipeline
	m     *gpulease.Manager
	cards []gpuprobe.Card
	token string
	// since is the arrival time of the place in line the call resumes, zero for a new arrival (placeSince).
	since  time.Time
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
// orders them (a tie is not counted: the prober may only err toward "free"). A NEW arrival has no place, so every
// caller in line is ahead of it: its admission does not order by arrival on the pinned and the allocated plans, every
// other caller in line claims the cards before the call looks (gpualloc.QueuedClaims).
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
		if arrivedBefore(t.SinceMs) && conflict(t.Devices, ids) && t.HoldsItsCardsAgainst(r.askRAM > 0) {
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
	for id := range gpualloc.QueuedClaimsBefore(r.m, r.cards, r.token, r.askRAM > 0, r.since) {
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
	r.callersAhead(ids, r.since)
	if len(r.reasons) == before { // taken by something that ended while it was being read
		r.block(subjectOf(ids) + " are held, or promised to callers ahead of this one")
	}
}

// autoInput is the allocator's input for a call that names no card: the same one the admission gives it, built from the
// one card-table read the question made.
func (r *laneReading) autoInput(ctx context.Context) func() (gpulease.AllocInput, error) {
	stored := r.p.alloc
	table := r.cards
	stored.Cards = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return table, "", nil }
	return func() (gpulease.AllocInput, error) {
		return gpualloc.BuildInput(ctx, r.m, r.p.cfg, gpualloc.Need{Claimed: r.claimed(), RAMGiB: r.askRAM}, stored)
	}
}

// pickAuto asks the allocator which card the call would take, from one reading of the world (a wait of 0, so no
// polling): free says that card is free now, and when it is not, ids is the set the call would queue on.
func (r *laneReading) pickAuto(ctx context.Context) (ids []string, free bool, err error) {
	return gpualloc.PickAuto(gpualloc.Plan{Min: 1, Max: 1}, 0, r.autoInput(ctx), io.Discard, time.Sleep, time.Now)
}

// allocated reads a call that names no card: the allocator chooses, from the same input the admission gives it, and
// one reading of the world.
func (r *laneReading) allocated(ctx context.Context, need mediaNeed) {
	if mediaSlots.wholeHeld() {
		r.slots(nil)
		return
	}
	ids, free, err := r.pickAuto(ctx)
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
		r.callersAhead(ids, r.since)
		if len(r.reasons) == before {
			r.block(subjectOf(ids) + " are held, or promised to callers ahead of this one")
		}
		return
	}
	r.hostRAMFor(ctx, need, ids)
}
