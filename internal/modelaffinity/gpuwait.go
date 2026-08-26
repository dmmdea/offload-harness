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
	dir := gpuLeaseDir()
	if dir == "" {
		// Not armed: no config.Load ran in this process. Inert by construction —
		// see SetGPULease. Every production entry point loads config, and
		// TestLoadArmsTheGPULoadGate in internal/config pins that.
		return nil
	}
	info := gpulease.InspectDir(dir)
	if !blocksLoad(info) {
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
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return leaseError(base, model, info, time.Since(start), bound, context.DeadlineExceeded)
		}
		if remain > leasePollInterval {
			remain = leasePollInterval
		}
		select {
		case <-ctx.Done():
			return leaseError(base, model, info, time.Since(start), bound, ctx.Err())
		case <-time.After(remain):
		}
		if info = gpulease.InspectDir(dir); !blocksLoad(info) {
			return nil
		}
	}
}

// blocksLoad decides whether info describes a card this process must not pull a
// model onto.
//
// ClassMedia ONLY, and that is a choice with a reason. A ClassText reservation is
// held by a benchmark or eval; its holder unloads nothing, so a switch underneath
// it costs a MEASUREMENT, not the machine — while blocking every interactive text
// call for the length of an eval run would be a larger regression than the one it
// prevents. Media is the class that clears the card and then needs it kept clear.
func blocksLoad(info gpulease.Info) bool {
	return info.Held && info.Class == gpulease.ClassMedia && !insideLease(info)
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
func insideLease(info gpulease.Info) bool {
	raw := strings.TrimSpace(os.Getenv("GPU_LEASE_EPOCH"))
	if raw == "" {
		return false
	}
	epoch, err := strconv.ParseUint(raw, 10, 64)
	return err == nil && epoch != 0 && epoch == info.Epoch
}

// LeaseError is the outcome of an exhausted wait for the GPU. Like WaitError it is
// a distinct type carrying WHO held the card, because "timeout" alone sends the
// reader to the model and the endpoint when the answer is a render.
type LeaseError struct {
	Base    string         // the resolved llama-swap base the admission was for
	Want    string         // the model this request named
	Class   gpulease.Class // the holder's lease class (always media today)
	PID     int            // the holder's pid
	Reason  string         // the holder's declared reason
	Origin  string         // the holder's declared origin
	JobID   string         // the holder's job id, when it declared one
	HeldFor time.Duration  // how long the holder has owned the card
	Waited  time.Duration
	Bound   time.Duration
	cause   error // context.DeadlineExceeded (our bound) or the caller's ctx.Err()
}

// Error names the render, not the model. It contains the word "timeout" because
// pipeline.classifyErr buckets infra errors by substring and a held card is
// congestion, which that classifier spells "timeout"; the wording is pinned by
// test so a reword cannot silently reclassify it in the ledger.
func (e *LeaseError) Error() string {
	return fmt.Sprintf(
		"gpu-lease timeout after %s (bound %s): a %s job holds the GPU (pid %d, held %s, reason %q), "+
			"and admitting model %q on %s would load it into VRAM that render is using",
		e.Waited.Round(time.Millisecond), e.Bound, e.Class, e.PID, e.HeldFor.Round(time.Second), e.Reason, e.Want, e.Base)
}

// Unwrap exposes the cause so errors.Is(err, context.DeadlineExceeded) and
// errors.Is(err, context.Canceled) keep working for callers that branch on them.
func (e *LeaseError) Unwrap() error { return e.cause }

// leaseError builds the report from the LAST inspection, so it describes the
// holder the caller actually waited on rather than a re-read that may have moved.
func leaseError(base, model string, info gpulease.Info, waited, bound time.Duration, cause error) error {
	return &LeaseError{
		Base:    base,
		Want:    model,
		Class:   info.Class,
		PID:     info.PID,
		Reason:  info.Reason,
		Origin:  info.Origin,
		JobID:   info.JobID,
		HeldFor: info.Age,
		Waited:  waited,
		Bound:   bound,
		cause:   cause,
	}
}
