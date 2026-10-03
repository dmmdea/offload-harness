// gpuwait.go — the half of this gate that answers to the MACHINE rather than to
// this process: an admission that would make llama-swap CHANGE what it holds
// resident waits while a MEDIA render owns the card.
//
// THE DEFECT. internal/gpulease is machine-wide and fenced, and its ClassMedia
// holder unloads llama-swap once per lease so the card is clear for the render.
// Ordinary interactive text was deliberately left outside that lease — "thousands
// per day at ~46ms, and leasing them is untenable" — so nothing stopped the very
// next text call from making llama-swap pull a multi-GB model straight back into
// the VRAM the render had just been given. Observed as the box becoming unusable
// under a render: the media job and the text tier both resident, both thrashing.
// A lease that one side reads and the other ignores is not mutual exclusion, which
// is the same sentence gpulease was written to stop being true.
//
// WHY THIS DOES NOT REOPEN THE COST gpulease REFUSED. That carve-out is about
// ACQUIRING a lease — an epoch bump, a claim file, a heartbeat, a release, all on
// the write path, per request. This never acquires and never writes. It READS the
// lease with gpulease.InspectDir, the one inspection path (the vision gate and
// delegate.LocalBusy read it the same way), and only on the admissions that can
// change residency — never on a request joining an in-flight batch of the model
// already loaded. On an idle box that is one ReadFile of a path that does not
// exist, tens of microseconds against a call the harness budgets 46ms for.
//
// WAITING, NOT REFUSING. A held card is congestion, not a fault: an image render
// clears in tens of seconds and the caller then gets the answer it asked for. So a
// blocked admission polls until the card frees, bounded by the caller's OWN budget
// (its http.Client.Timeout) and by ctx — the same two bounds the in-process park in
// affinity.go uses, for the same reason. One admission gets ONE such deadline for
// all its waiting, so a caller that waits, parks, and is promoted onto a card that
// has been taken again cannot spend that budget twice. It is deliberately NOT bounded by the
// holder's declared TTL: gpulease stamps DefaultTTL = 1h on essentially every
// media lease as a reservation, not an estimate, so treating it as an ETA would
// turn every wait into an instant refusal.
package modelaffinity

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// leasePollInterval is how often a blocked admission re-reads the lease. It
// matches gpulease.acquirePollInterval: fast enough that a waiter starts within a
// second of a render finishing, cheap enough at one small file read to be free.
const leasePollInterval = time.Second

var (
	leaseMu  sync.RWMutex
	leaseDir string // "" = gate not armed; see SetGPULease
)

// SetGPULease arms the machine-wide half of this gate at the lease directory the
// operator's gpu_lock_path/state_dir resolve to, and is called from config.Load.
//
// ARMED FROM CONFIG LOAD, ON PURPOSE — the same wiring, for the same reason, as
// netguard.SetTailnetSuffix beside it. The lease location is a property of the
// MACHINE, not of any one client, and it is decided by two config fields. Passing
// it down instead would mean threading it through llamaclient.New and
// agent.NewLLMClient at 60-odd construction sites, where the ONE site that forgot
// would be an ungated text lane with nothing to report it. Arming it in the same
// act that resolves the overrides means a second resolution order cannot exist —
// which is the whole of gpulease.LeaseDir's doctrine, applied one layer up.
//
// A resolution failure DISARMS rather than degrades to a guess: gpulease.LeaseDir
// refuses a cloud-synced root, and inventing a different directory here is exactly
// the silent lease split it refuses it to prevent. Disarming is safe rather than
// merely tolerable — the same refusal reaches gpulease.OpenAt on the media path,
// so no media lease can be taken on such a box and there is nothing to protect.
// The error is returned so the caller can say so out loud.
func SetGPULease(lockOverride, stateDir string) error {
	dir, err := gpulease.LeaseDir(lockOverride, stateDir)
	leaseMu.Lock()
	defer leaseMu.Unlock()
	if err != nil {
		leaseDir = ""
		return err
	}
	leaseDir = dir
	return nil
}

// GPULeaseDir reports the lease directory this gate is armed at, or "" when it is
// not armed. It exists so the wiring is OBSERVABLE: the gate is armed process-wide
// from config.Load, and without a way to read it back "we armed it" would be an
// unfalsifiable claim rather than something a test can pin.
func GPULeaseDir() string { return gpuLeaseDir() }

// gpuLeaseDir reports the armed lease directory ("" when unarmed).
func gpuLeaseDir() string {
	leaseMu.RLock()
	defer leaseMu.RUnlock()
	return leaseDir
}

// awaitCard blocks while a media render owns the GPU, returning nil the moment the
// card is free (or was never held) and a *LeaseError when the wait exhausts.
//
// deadline is the wall-clock end of ALL lease waiting for one admission, computed
// once in Admit and shared with the post-promotion wait — so an admission that waits,
// gets in, parks and is promoted onto a card that has been taken again cannot spend
// the caller's budget twice over. ctx bounds the wait too, and in production is
// usually the tighter of the two. A deadline already past still performs ONE
// inspection, so a spent caller is told the card is free rather than refused on
// arithmetic.
func awaitCard(ctx context.Context, base, model string, deadline time.Time) error {
	return awaitLease(ctx, base, model, deadline, blocksLoad)
}

// AwaitRunSlot is awaitCard for the START of an agent run (0.117.0, register
// D-93): it also waits out a text holder that is DRAINING the seat. The two
// predicates differ on purpose — see BlocksNewRun. A launcher calls this once,
// before admission, with its admission budget as the deadline, so a run
// refused at the cordon defers with the holder named and never spends its
// wall waiting.
func AwaitRunSlot(ctx context.Context, base, model string, deadline time.Time) error {
	return awaitLease(ctx, base, model, deadline, BlocksNewRun)
}

func awaitLease(ctx context.Context, base, model string, deadline time.Time, blocks func(gpulease.Info) bool) error {
	dir := gpuLeaseDir()
	if dir == "" {
		// Not armed: no config.Load ran in this process. Inert by construction —
		// see SetGPULease. Every production entry point loads config, and
		// TestLoadArmsTheGPULoadGate in internal/config pins that.
		return nil
	}
	// The leases that sit on THIS seat's cards (seatscope.go): a render on another card is
	// not a reason to wait.
	info := ScopeToModel(InspectLease(dir), model)
	if !blocks(info) {
		return nil
	}
	start := time.Now()
	// The reported bound is what was LEFT when this wait began, not the caller's whole
	// budget: after a park the two differ, and reporting the budget would overstate how
	// long this caller actually gave the render.
	bound := deadline.Sub(start)
	if bound < 0 {
		bound = 0
	}
	// FAIR TURN (register D-1xx-2, 2026-09-23; R2: "seat starvation behind
	// chained media leases"). Only registered once we are actually about to
	// block, never on the fast uncontested path above — a blocked admission
	// takes a place in gpulease's own waiters queue (the SAME one a lease
	// Acquire registers in) so a fresh media Acquire that reaches this box's
	// card once the lease frees must queue behind us exactly as it would
	// behind a real lease waiter, instead of a chained re-acquire winning the
	// gap before this admission's own poll ever notices the lease is free.
	// See gpulease.RegisterSeatWaiter's doc for why this cannot deadlock or
	// starve the lease itself. refresh must run every poll tick or the
	// record goes heartbeat-stale and stops protecting our place in line.
	refresh, unregister := gpulease.RegisterSeatWaiter(dir, "load "+model+" on "+base)
	defer unregister()
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return explained(dir, leaseError(base, model, blockingLease(info, blocks), time.Since(start), bound, context.DeadlineExceeded))
		}
		if remain > leasePollInterval {
			remain = leasePollInterval
		}
		select {
		case <-ctx.Done():
			return explained(dir, leaseError(base, model, blockingLease(info, blocks), time.Since(start), bound, ctx.Err()))
		case <-time.After(remain):
		}
		refresh()
		if info = ScopeToModel(InspectLease(dir), model); !blocks(info) {
			return nil
		}
	}
}

// blocksLoad decides whether info describes a card this process must not pull a
// model onto.
//
// ClassMedia, or a ClassText lease stamped EXCLUSIVE. A plain text reservation is
// held by a benchmark whose holder unloaded nothing, so a switch underneath it
// costs a MEASUREMENT, not the machine — and blocking every interactive text call
// for the length of an eval would be a larger regression than the one it prevents.
// That reasoning stopped covering the case where the holder DID clear the cards:
// `gpu reserve --drain --unload-seat` empties llama-swap for a measurement, and
// the next interactive text call pulled a multi-GB model straight back onto the
// card mid-run — the residency switch that voided two 5070 Ti runs and taught
// sessions to refuse GPU work outright rather than reserve it (0.115.2). Such a
// holder stamps Exclusive, and its loads are gated exactly like a render's: a
// call with a cascade remote lane rides the lane, one without waits its own
// budget and is told who holds the card. Media stays the class that clears the
// card and needs it kept clear regardless of any flag.
//
// A DRAINING hold of either class does not block a load (2026-09-22). The holder
// has not touched the cards yet — it is waiting for the runs in flight to finish
// — and blocking their requests is the 2026-09-14 deadlock: the drain waits for
// runs that wait for the drain. That was fixed for text holds and stayed open
// for `gpu reserve --class media --drain`, because the lease record dropped the
// draining stamp on a media lease and the media class then blocked from acquire.
// The fence starts when maintainSeat clears the stamp, exactly as for text.
//
// With card-scoped leases several are live at once and Info describes only the lowest.
// The question is asked of EACH live lease (Info.Each) and the exemption for "this process
// runs under the lease" applies per lease: a child of lease A is not blocked by A and is
// blocked by any other lease that blocks. blocksLoad is the whole-node reading (every card):
// BlocksLoadFor narrows it to the cards one seat sits on.
func blocksLoad(info gpulease.Info) bool {
	for _, l := range info.Each() {
		if blocksLoadOne(l) {
			return true
		}
	}
	return false
}

// blocksLoadOne is blocksLoad for ONE lease.
func blocksLoadOne(l gpulease.Info) bool {
	if !l.Held || insideLease(l) || (l.Draining && !l.Exclusive) {
		return false
	}
	return l.Class == gpulease.ClassMedia || (l.Class == gpulease.ClassText && l.Exclusive)
}

// BlocksNewRun decides whether info describes a card no NEW agent run may start
// on: everything blocksLoad refuses, plus a holder that is DRAINING the seat
// (0.117.0, register D-93; either class since 2026-09-22). The distinction is the whole fix: a drain must
// stop new work from landing (or it never converges under K sessions) while
// letting the runs already in flight finish their remaining steps (or it
// blocks the very work it is waiting for — the 2026-09-14 deadlock, resolved
// only by the run's 600 s wall). Requests of a registered run pass blocksLoad
// under a draining lease; a run's first admission passes through here.
func BlocksNewRun(info gpulease.Info) bool {
	for _, l := range info.Each() {
		if blocksLoadOne(l) || (l.Held && !insideLease(l) && l.Draining) {
			return true
		}
	}
	return false
}

// insideLease reports whether this process is running UNDER the very lease that
// holds the card, in which case waiting for it is waiting for ourselves.
//
// The marker is the inherited GPU_LEASE_EPOCH that pipeline.ambientLeaseEnv reads:
// `local-offload gpu reserve --class media -- local-offload …` takes one lease and
// runs the harness as its child, and a text call in that child must not queue
// behind its own parent. The epoch is compared, not merely presence-checked, so a
// stale variable left over from a lease that has since been handed on cannot
// exempt anything.
//
// THE HOLDER'S OWN PROCESS IS DELIBERATELY NOT EXEMPT. Exempting by pid would read
// as obviously safe and would silently reopen the incident in the deployment where
// it actually happened: fleet-serve (and the MCP server) run a render and serve
// text tool calls in ONE process, so a pid exemption would let exactly the calls
// that trampled the render skip the gate. There is no in-process text call to
// deadlock: the one text step on the media path — the image prompt refiner — is
// hoisted ABOVE acquireMediaLease on both the single and batch routes precisely so
// "the text call never contends with our own render", and runPipelineJob takes
// only the in-process mediaSlot, never the machine-wide lease.
//
// info is ONE lease (callers walk Info.Each). The comparison is against that lease's own
// epoch, never "any live epoch": a child of lease A is not inside lease B, and exempting
// it from B's fence is the permissive direction.
func insideLease(info gpulease.Info) bool {
	raw := strings.TrimSpace(os.Getenv("GPU_LEASE_EPOCH"))
	if raw == "" {
		return false
	}
	epoch, err := strconv.ParseUint(raw, 10, 64)
	return err == nil && epoch != 0 && epoch == info.Epoch
}

// blockingLease names the lease a refusal is about: the first live lease that blocks
// under pred, else info itself.
func blockingLease(info gpulease.Info, pred func(gpulease.Info) bool) gpulease.Info {
	for _, l := range info.Each() {
		if pred(l) {
			return l
		}
	}
	return info
}

// LeaseError is the outcome of an exhausted wait for the GPU. Like WaitError it is
// a distinct type carrying WHO held the card, because "timeout" alone sends the
// reader to the model and the endpoint when the answer is a render.
type LeaseError struct {
	Base    string         // the resolved llama-swap base the admission was for
	Want    string         // the model this request named
	Class   gpulease.Class // the holder's lease class (media, or an exclusive text hold)
	PID     int            // the holder's pid
	Reason  string         // the holder's declared reason
	Origin  string         // the holder's declared origin
	JobID   string         // the holder's job id, when it declared one
	HeldFor time.Duration  // how long the holder has owned the card
	Waited  time.Duration
	Bound   time.Duration
	// Draining: the holder is a text lease draining the seat (BlocksNewRun) — the
	// refusal is of a NEW run, not of a load; running work was never touched.
	Draining bool
	// ExpiresAt is the end the holder DECLARED when it acquired the lease; zero
	// when it declared none. Rendered by window() — see there for why a number
	// this package refuses to wait on is still worth reporting.
	ExpiresAt time.Time
	// Epoch is the holder's lease epoch. Explain, when non-empty, says what is wrong
	// with a holder that is held but not healthy (its owner gone, its progress stalled,
	// its window long past) and the command that frees it; it is appended to Error() and
	// is empty for a lease that is fine.
	Epoch   uint64
	Explain string
	cause   error // context.DeadlineExceeded (our bound) or the caller's ctx.Err()
}

// window renders the holder's declared end for the refusal message (register
// D-110): ", declared until 11:40PM (~37m0s left)".
//
// The package header says this wait is deliberately NOT bounded by the holder's
// TTL, and that stands — a DefaultTTL of an hour is a reservation, not an
// estimate, and waiting on it would turn every media wait into an instant
// refusal. Reporting it is the opposite operation: the wait already ended on its
// own bound, and the caller now has to choose between retrying, routing
// elsewhere, and coming back later. "a text job holds the GPU" answers none of
// those; the declared window answers the third. gpulease.ErrHeld has rendered the
// same field for the same reason since 0.113.14 — this is that idiom, on the
// refusal a delegation actually reads (agenttask.go files it as the capacity
// defer's reason).
//
// The remaining time is computed at RENDER time, not at build time, so a message
// formatted after a long unwind still states what is actually left.
func (e *LeaseError) window() string {
	if e.ExpiresAt.IsZero() {
		return ""
	}
	until := e.ExpiresAt.Local().Format(time.Kitchen)
	left := time.Until(e.ExpiresAt).Round(time.Second)
	if left <= 0 {
		// Past its declared end and still held: the holder either renewed or
		// overran. Say so rather than printing a negative duration — "retry now"
		// is the honest reading either way.
		return ", declared until " + until + " (that window has already elapsed)"
	}
	return fmt.Sprintf(", declared until %s (~%s left)", until, left)
}

// Error names the render, not the model. It contains the word "timeout" because
// pipeline.classifyErr buckets infra errors by substring and a held card is
// congestion, which that classifier spells "timeout"; the wording is pinned by
// test so a reword cannot silently reclassify it in the ledger.
func (e *LeaseError) Error() string {
	if e.Draining {
		return fmt.Sprintf(
			"gpu-lease timeout after %s (bound %s): a %s holder is draining the seat (pid %d, held %s, reason %q)%s: "+
				"no new run starts on %s at %s until the work already in flight finishes; running work is not interrupted",
			e.Waited.Round(time.Millisecond), e.Bound, e.Class, e.PID, e.HeldFor.Round(time.Second), e.Reason, e.window(), e.Want, e.Base) + e.explainTail()
	}
	return fmt.Sprintf(
		"gpu-lease timeout after %s (bound %s): a %s job holds the GPU (pid %d, held %s, reason %q)%s, "+
			"and admitting model %q on %s would load it into VRAM that render is using",
		e.Waited.Round(time.Millisecond), e.Bound, e.Class, e.PID, e.HeldFor.Round(time.Second), e.Reason, e.window(), e.Want, e.Base) + e.explainTail()
}

// explainTail is the standing of an unhealthy holder, after the pinned wording.
func (e *LeaseError) explainTail() string {
	if e.Explain == "" {
		return ""
	}
	return " — " + e.Explain
}

// Unwrap exposes the cause so errors.Is(err, context.DeadlineExceeded) and
// errors.Is(err, context.Canceled) keep working for callers that branch on them.
func (e *LeaseError) Unwrap() error { return e.cause }

// leaseError builds the report from the LAST inspection, so it describes the
// holder the caller actually waited on rather than a re-read that may have moved.
func leaseError(base, model string, info gpulease.Info, waited, bound time.Duration, cause error) error {
	return &LeaseError{
		Base:      base,
		Want:      model,
		Class:     info.Class,
		PID:       info.PID,
		Reason:    info.Reason,
		Origin:    info.Origin,
		JobID:     info.JobID,
		HeldFor:   info.Age,
		Waited:    waited,
		Draining:  info.Draining && !info.Exclusive,
		ExpiresAt: info.ExpiresAt,
		Epoch:     info.Epoch,
		Bound:     bound,
		cause:     cause,
	}
}

// explained attaches the standing of the lease a refusal is about. The lease is read
// again at the directory the gate is armed at, so the sentence reflects the lease the
// caller waited on; a refusal that is not a *LeaseError passes through untouched.
func explained(dir string, err error) error {
	le, ok := err.(*LeaseError)
	if !ok || le.Epoch == 0 {
		return err
	}
	for _, l := range gpulease.InspectLeases(dir) {
		if l.Epoch == le.Epoch {
			le.Explain = gpulease.ExplainHeld(dir, l, 0)
			break
		}
	}
	return le
}
