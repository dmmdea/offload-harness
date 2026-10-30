// GPU plumbing for the media doors: gen/lock env, the whole-node lease, busy defers and footprint sampling.

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/mediaops"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

// genEnv builds the extra env for a GPU-gen child: COMFY_DIR + MEMORY_STACK
// (invariant 1: the models freeLlamaSwap must never unload, sourced from
// config rather than a buried const).
//
// It does NOT carry the lease. Callers append the env returned by acquireMediaLease,
// which is the only thing that makes a runner willing to touch the GPU — keeping the
// two separate is what makes "did this call site take a lease?" answerable by reading
// the call site. There is no longer a GPU_LOCK_WAIT_MS: queueing moved to the Go side
// with the acquisition, and the runners never read that variable.
func (p *Pipeline) genEnv() []string {
	env := []string{"COMFY_DIR=" + p.cfg.ComfyDir}
	if len(p.cfg.MemoryStack) > 0 {
		env = append(env, "MEMORY_STACK="+strings.Join(p.cfg.MemoryStack, ","))
	}
	// FFMPEG_PATH (F-35 regression follow-up, 2026-09-23; F-38 fix, 2026-09-24): the
	// music route's post-render QA gate (render/comfy-music.mjs / audio-qa.mjs —
	// over-render/trim, silence/true-peak measurement, loudness normalization)
	// shells out to ffmpeg the same way audioio.go already does for transcribe.
	// Threading the SAME configured binary here keeps both call sites honoring one
	// per-machine ffmpeg_path instead of the script guessing its own default.
	//
	// F-38: config.Default's FFmpegPath is the bare name "ffmpeg" (a genuinely
	// unconfigured box never sets one), so the old `!= ""` check was ALWAYS true
	// and every child got FFMPEG_PATH=ffmpeg — a bare name, not a path.
	// audio-qa.mjs's resolveFfmpeg() treats a set FFMPEG_PATH as an exact file via
	// existsSync(), which cannot see PATH resolution, so it read "ffmpeg" as
	// missing and silently skipped the entire QA gate on every fleet node that
	// never set an explicit ffmpeg_path (reproduced identically on <node-c> and
	// <node-a>, 2026-09-23/24). mediaops.ResolveBinary resolves it here in Go —
	// the same PATH-aware lookup doctor's media route already uses — so the child
	// always receives either a real absolute path (existsSync succeeds directly)
	// or nothing at all. Omitted (not "ffmpeg") when it cannot be resolved on this
	// box at all: the script's own resolveFfmpeg() then does its own PATH probe,
	// which will reach the identical answer, and main() now fails loudly rather
	// than silently skipping the gate when neither side can find it.
	if resolved, ok := mediaops.ResolveBinary(p.cfg.FFmpegPath); ok {
		env = append(env, "FFMPEG_PATH="+resolved)
	}
	return env
}

// gpuWait is how long a GPU task may queue behind the current holder before deferring.
// One ceiling for every task: per-task overrides bought nothing at 90s and would have
// silently restored the old 20-minute video wait from every existing config file.
// Zero means a single try.
func (p *Pipeline) gpuWait() time.Duration {
	if p.cfg.GPUWaitMs <= 0 {
		return 0
	}
	return time.Duration(p.cfg.GPUWaitMs) * time.Millisecond
}

// lockEnv threads a configured GPU-lock override to a render runner as the
// GPU_LOCK env, so the Go-side vision gate (LO-1) and the Node runners always
// contend on the SAME lock path. Empty when gpu_lock_path is unset — both
// sides then resolve the identical default on their own via gpulease.LeaseDir.
func (p *Pipeline) lockEnv() []string {
	if p.cfg.GPULockPath != "" {
		return []string{"GPU_LOCK=" + p.cfg.GPULockPath}
	}
	return nil
}

// ambientLeaseEnv reports the lease this process is ALREADY running under, if any.
//
// `local-offload gpu reserve --class media -- local-offload …` is a documented
// standalone flow: the reserve verb takes the lease and runs the harness as its CHILD,
// threading GPU_LEASE_DIR/EPOCH/CLASS down. Acquiring again inside that child is a
// self-deadlock — the parent holds the only slot, so the child queues behind itself
// until its window lapses and then defers. Inheriting is also the right semantic: the
// operator asked for ONE reservation to cover the whole command.
//
// (nil, nil) means no ambient lease — acquire normally. A lease that is PRESENT but no
// longer current is an error, not an inheritance: the parent was fenced out, so nothing
// is protecting the card and a silent fresh acquisition would hide that.
func ambientLeaseEnv() ([]string, error) {
	dir := strings.TrimSpace(os.Getenv("GPU_LEASE_DIR"))
	rawEpoch := strings.TrimSpace(os.Getenv("GPU_LEASE_EPOCH"))
	class := strings.TrimSpace(os.Getenv("GPU_LEASE_CLASS"))
	if dir == "" && rawEpoch == "" && class == "" {
		return nil, nil
	}
	if dir == "" || rawEpoch == "" || class == "" {
		return nil, fmt.Errorf("inherited gpu lease is incomplete (GPU_LEASE_DIR=%q EPOCH=%q CLASS=%q); "+
			"refusing to render unarbitrated", dir, rawEpoch, class)
	}
	epoch, perr := strconv.ParseUint(rawEpoch, 10, 64)
	if perr != nil || epoch == 0 {
		return nil, fmt.Errorf("inherited gpu lease epoch %q is not a fencing token; refusing to render unarbitrated", rawEpoch)
	}
	if !gpulease.Class(class).Valid() {
		return nil, fmt.Errorf("inherited gpu lease class %q is unknown; refusing to render unarbitrated", class)
	}
	// The fence, applied at the boundary: an epoch that is no longer current means the
	// card was handed to somebody else while our parent was suspended.
	// Per epoch: with card-scoped leases several are live at once, and comparing against
	// the lowest epoch would fence out every other lease's child.
	if !gpulease.EpochIsCurrent(dir, epoch) {
		return nil, fmt.Errorf("inherited gpu lease (epoch %d) is no longer current at %s; "+
			"the card was handed to another holder", epoch, dir)
	}
	return []string{
		"GPU_LEASE_DIR=" + dir,
		"GPU_LEASE_EPOCH=" + rawEpoch,
		"GPU_LEASE_CLASS=" + class,
	}, nil
}

// The in-process media slots (one per card) live in mediaslots.go.

// errGPUBusy reports that the card is legitimately held by someone else. Callers turn
// it into a clean defer rather than a failure — the work is not broken, it waited its
// window behind a holder.
//
// detail is set when the holder is another job in THIS process, where there is no lease
// record to describe — the machine-wide Info would name our own lease and read as
// nonsense.
type errGPUBusy struct {
	info   gpulease.Info
	detail string
	// hostRAM is set when the cards were free and the host's memory was what the call waited for
	// (gpulease/hostram.go): the caller turns it into a place in line that says so.
	hostRAM *gpulease.ErrHostRAM
}

func (e *errGPUBusy) Error() string {
	if e.detail != "" {
		return "gpu busy: " + e.detail
	}
	// The holder is a LEASE of a class ("held by a text-class lease"), never the bare class: "text holds
	// the lease" read as a text seat (gpulease.Class.LeasePhrase, the phrase `gpu status` leads with).
	return fmt.Sprintf("gpu busy: held by %s (%ds, reason %q)",
		e.info.Class.LeasePhrase(), int(e.info.Age/time.Second), e.info.Reason)
}

// IsGPUBusy reports whether err means the card was legitimately held by someone else
// for the whole wait window — the work is intact and was queued, not broken.
//
// It is exported because errGPUBusy is unexported and RunImageBatch returns an error
// rather than a core.Result: with no predicate the CLI turned a busy card into a hard
// failure and exited non-zero, which is precisely what a defer exists to avoid.
func IsGPUBusy(err error) bool {
	var busy *errGPUBusy
	var queued *errGPUQueued
	return errors.As(err, &busy) || errors.As(err, &queued)
}

// deferForLease turns a lease-acquisition failure into the same clean defer the render
// runner used to produce on a busy card — but WITHOUT spawning a process that could
// only have queued and timed out. A busy card is not a failure, it is work behind a
// holder; an unusable lease location is a configuration fault and is classed apart so
// the two are distinguishable in the ledger.
func (p *Pipeline) deferForLease(err error, task core.TaskType, meta core.Meta, inputChars int, start time.Time) core.Result {
	meta.LatencyMs = time.Since(start).Milliseconds()
	// A call that waited its window with no card is not refused: it holds a place in line, and the
	// answer carries the token that resumes it (mediaadmit.go).
	var queued *errGPUQueued
	if errors.As(err, &queued) {
		meta.ErrClass = "gpu_queued"
		p.recordDefer(task, meta, inputChars, err.Error())
		res := core.Deferf(err.Error(), "", meta)
		res.DeferClass = core.DeferClassCapacity
		res.Data = queued.payload()
		return res
	}
	var busy *errGPUBusy
	if errors.As(err, &busy) {
		meta.ErrClass = "gpu_busy"
	} else {
		meta.ErrClass = core.ErrClassGPULeaseUnavailable
	}
	p.recordDefer(task, meta, inputChars, err.Error())
	return core.Deferf(err.Error(), "", meta)
}

// acquireWholeNode takes the machine-wide, WHOLE-NODE MEDIA lease for one generation job and
// returns the env that hands it down to the render runner, plus a release func. It is what every
// call did before cards could be leased, and what a call still does when it needs the whole node
// or runs on a host that cannot lease cards (acquireMediaLease, mediaadmit.go, decides).
//
// THIS IS WHERE ARBITRATION MOVED TO. Acquisition used to live only in
// render/gpu-lock.mjs, which meant the Go side could not refuse a job before spawning
// it, and — because two languages each implemented the rules — the two disagreed about
// who owned the card. The Go side now acquires and the runner INHERITS
// (GPU_LEASE_DIR/EPOCH/CLASS), so there is exactly one implementation.
//
// A busy card is QUEUED for up to `wait` and only then returns *errGPUBusy, so the
// caller defers with the holder's detail instead of spawning a process that could only
// have waited and timed out. The heartbeat keeps a long render's lease alive; the
// reclaim rule needs both a stale heartbeat and an expired window, so a missed tick
// inside the declared window is harmless.
func (p *Pipeline) acquireWholeNode(ctx context.Context, reason string, ttl, wait time.Duration, resume string) ([]string, func(), error) {
	return p.acquireWholeNodeWith(ctx, reason, ttl, wait, resume, 0, 0)
}

// acquireWholeNodeWith is acquireWholeNode for a call that declares hostRAMGiB of host RAM (and, above 0, commitGiB of stated commit,
// judged against the commit limit; 0 = the one figure stands in for the commit): the grant
// admits it against the available memory and the commit charge against physical RAM (the one figure, no commit stated;
// gpulease/hostram.go) and keeps the call in the queue while the host is short. When the host is short and a model its own
// unload takes is resident, the call is compared to the host AFTER the pipeline has unloaded it, under the lease (the F82 order;
// mediahostram.go): the memory the render frees is not counted as held against it.
func (p *Pipeline) acquireWholeNodeWith(ctx context.Context, reason string, ttl, wait time.Duration, resume string, hostRAMGiB, commitGiB float64) ([]string, func(), error) {
	noop := func() {}
	start := time.Now()

	// THE IN-PROCESS SLOT COMES FIRST, on both paths. Blocking here is free — no timer
	// in the uncontended case, no polling in the contended one — and it is the only
	// thing arbitrating two jobs that INHERIT the same lease. See slotSet (mediaslots.go).
	if !takeMediaSlot(wait) {
		return nil, noop, &errGPUBusy{detail: fmt.Sprintf(
			"another generation job in this process still holds the card after %s", wait)}
	}
	slotHeld := true
	defer func() {
		// Every failure path below must give the slot back, or the first bad lease
		// wedges every later job in this process.
		if slotHeld {
			releaseMediaSlot()
		}
	}()

	// Already inside someone's lease? Inherit it. Acquiring again would queue behind
	// ourselves until the window lapsed.
	inherited, ierr := ambientLeaseEnv()
	if ierr != nil {
		return nil, noop, ierr
	}
	if inherited != nil {
		// The holder above us owns renewal and release; doing either here would drop a
		// lease that is not ours. The slot is still ours to give back.
		slotHeld = false
		var once sync.Once
		// The card is ours from here: the call's PAIR card turns running.
		core.MarkWorking(ctx)
		return append(inherited, p.lockEnv()...), func() { once.Do(releaseMediaSlot) }, nil
	}
	m, err := gpulease.OpenAt(p.cfg.GPULockPath, p.cfg.StateDir)
	if err != nil {
		// Refuse rather than run unarbitrated. This is the fail-closed half of the
		// design: an unusable lease location means we cannot promise mutual exclusion,
		// and quietly rendering anyway is exactly the behaviour that let a job tear the
		// text tier down. The message names the fix (state_dir).
		return nil, noop, err
	}
	// A render whose own unload frees the host memory it declares is not compared to the host in the grant (the F82 order,
	// mediahostram.go): it takes the node declaring nothing, clears, and only then asks the host. Decided before the wait is
	// measured, because it reads llama-swap.
	d := p.hostRAMDeferralLoad(ctx, m, hostRAMGiB, commitGiB, nil, nil, 0)
	// Spend only what is LEFT of the window on the machine-wide lease, so the two waits
	// compose into the one budget the caller asked for rather than doubling it.
	remaining := wait - time.Since(start)
	if remaining < 0 {
		remaining = 0
	}
	// resume is a place-keeping token (gpulease/tokens.go): only a host that leases cards ever
	// leaves one, so on any other host it names nothing and the call is what it always was.
	lease, err := m.Acquire(gpulease.ClassMedia, gpulease.Options{
		Reason: reason, Origin: "pipeline", TTL: ttl, Wait: remaining, ResumeToken: resume, HostRAMGiB: d.gate(), HostRAMWaitGiB: d.later(),
		HostRAMCommitGiB: d.gateCommit(), HostRAMWaitCommitGiB: d.laterCommit(),
	})
	if err != nil {
		var held *gpulease.ErrHeld
		if errors.As(err, &held) {
			return nil, noop, &errGPUBusy{info: held.Info}
		}
		var short *gpulease.ErrHostRAM
		if errors.As(err, &short) && !short.Impossible {
			// The node is free; the host's memory is not: busy, with the reason, not broken.
			return nil, noop, &errGPUBusy{detail: err.Error(), hostRAM: short}
		}
		if errors.Is(err, gpulease.ErrStillQueued) {
			// The card is free but a waiter registered earlier is ahead (register D-1xx-3): busy, not broken.
			return nil, noop, &errGPUBusy{detail: err.Error()}
		}
		return nil, noop, err
	}

	// The heartbeat exists only while a render is actually running, and a whole-node lease
	// (this function) is held by one media job at a time per process, so this is at most ONE
	// 15s timer for the duration of GPU work that lasts minutes (a card-scoped host holds one
	// lease, and one such timer, per card in use: startHeartbeat). Nothing ticks while the
	// harness is idle.
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
	release := func() {
		once.Do(func() {
			close(stop)
			// JOIN, do not just signal. Release mutates the Lease that a Renew() already
			// in flight is reading, and it would also let that Renew re-create the
			// heartbeat file after the release swept it. Waiting costs microseconds.
			<-stopped
			_ = lease.Release()
			releaseMediaSlot()
		})
	}
	slotHeld = false
	if d.defers {
		// The unload the runner would do after launch is done here, under the lease, and only then is the host asked. The wait is
		// bounded (hostRAMWaitEnd); past it the lease is released and the call is refused with the words the grant always used.
		if aerr := p.clearThenAdmitHostRAM(ctx, lease, d, start); aerr != nil {
			release()
			var short *gpulease.ErrHostRAM
			if errors.As(aerr, &short) && !short.Impossible {
				return nil, noop, &errGPUBusy{detail: aerr.Error(), hostRAM: short}
			}
			return nil, noop, aerr
		}
	}
	// The card is ours from here: the call's PAIR card turns running.
	core.MarkWorking(ctx)
	env := []string{
		"GPU_LEASE_DIR=" + lease.Dir(),
		"GPU_LEASE_EPOCH=" + strconv.FormatUint(lease.Epoch(), 10),
		"GPU_LEASE_CLASS=" + string(lease.Class()),
	}
	return append(env, p.lockEnv()...), release, nil
}

// footprintsPath resolves the shared fleet footprint store path: a
// "footprints.json" sibling of the ledger (else cache) file — the same
// ~/.local-offload base the default config resolves those to, and
// automatically isolated in tests that point them at temp dirs. Falls back to
// ~/.local-offload/footprints.json when both are opted out; "" = no store
// (sampling stays off).
func (p *Pipeline) footprintsPath() string {
	for _, anchor := range []string{p.cfg.LedgerPath, p.cfg.CachePath} {
		if anchor != "" {
			return filepath.Join(filepath.Dir(anchor), "footprints.json")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local-offload", "footprints.json")
	}
	return ""
}

// FootprintStore returns the lazily-opened shared footprint store (nil when no
// path resolves). Exported for the fleet-serve/fleet-measure verbs: health
// advertises its Entries(), fleet-measure prints its on-disk records.
func (p *Pipeline) FootprintStore() *fleetnode.Footprints {
	p.footOnce.Do(func() {
		if path := p.footprintsPath(); path != "" {
			p.foot = fleetnode.OpenFootprints(path)
		}
	})
	return p.foot
}

// footprintSampling composes the passive per-render VRAM sampling hook for one
// GPU render: the footprint key from THIS machine's bindings, the sampler
// cfg.FleetSampler selects, and a record-into-the-shared-store callback
// (gpugen fires it on SUCCESS only, peak > 0 only). nil when no store
// resolves — gpugen then keeps its legacy exec path byte-identical.
func (p *Pipeline) footprintSampling(family, quant, task string) *gpugen.Sampling {
	store := p.FootprintStore()
	if store == nil {
		return nil
	}
	return &gpugen.Sampling{
		Footprint:   &gpugen.FootprintKey{Family: family, Quant: quant, Task: task},
		SampleFunc:  p.footprintSampleFunc(),
		OnFootprint: func(peakGiB float64) { store.Record(family, quant, task, peakGiB) },
		// The host side of the same measurement (G3 of the P0 plan): what the render's process tree held of
		// the host's memory at its peak, recorded beside the VRAM peak under the same key. The media
		// admission raises a declaration to it once enough runs exist (calibratedRAM).
		HostSampleFunc:  p.hostSampleFunc(),
		OnHostFootprint: func(privateGiB, residentGiB float64) { store.RecordHost(family, quant, task, privateGiB, residentGiB) },
	}
}

// hostSampleFunc is the host-memory sample of a render: the private and the resident memory of the process tree
// rooted at the runner, root included (gpulease.TreeMemory). p.hostSample overrides it in tests. A ComfyUI
// instance kept from an earlier lease and reused by this runner is not under it, so a run can read LOW; the one
// consumer (calibratedRAM) only ever raises a declaration with it, and the store keeps the last twenty runs of a key and
// answers with their 90th percentile (fleetnode.HostPeaks), so one run that read high or low does not decide it.
func (p *Pipeline) hostSampleFunc() func(childPid int) (float64, float64, error) {
	if p.hostSample != nil {
		return p.hostSample
	}
	return gpulease.TreeMemory
}

// footprintSampleFunc selects the per-render VRAM source per cfg.FleetSampler:
// "pdh-shared" (J3, UMA iGPUs — the amd-rdna3 seed sets it) → the PDH tree
// summing Dedicated+Shared, because on unified memory allocations land in
// SHARED and Dedicated reads ~0 (footprints would silently never record);
// Windows + not-"global" → the PDH Dedicated tree (measures OUR job's cost,
// uncontaminated by the desktop/other apps); otherwise an nvidia-smi
// global-delta closure. p.fleetSample overrides in tests.
func (p *Pipeline) footprintSampleFunc() func(childPid int) (float64, error) {
	if p.fleetSample != nil {
		return p.fleetSample
	}
	if runtime.GOOS == "windows" && p.cfg.FleetSampler == "pdh-shared" {
		return fleetnode.TreeDedicatedPlusSharedGiB
	}
	if runtime.GOOS == "windows" && p.cfg.FleetSampler != "global" {
		return fleetnode.TreeDedicatedGiB
	}
	return globalDeltaSampleFunc(runNvidiaSmiMemory)
}

// globalDeltaSampleFunc builds the fallback sampler: global VRAM used minus a
// baseline captured by the closure on its first call — which gpugen makes
// immediately at child start, before the render loads anything. Called only
// from gpugen's single sampler goroutine, so the baseline needs no lock.
func globalDeltaSampleFunc(run func() (string, error)) func(childPid int) (float64, error) {
	baseline := -1.0
	return func(int) (float64, error) {
		out, err := run()
		if err != nil {
			return 0, err
		}
		_, used, err := fleetnode.ParseSmiMemory(out)
		if err != nil {
			return 0, err
		}
		if baseline < 0 {
			baseline = used
		}
		d := used - baseline
		if d < 0 {
			d = 0
		}
		return d, nil
	}
}

// runNvidiaSmiMemory shells the global VRAM query the global-delta sampler
// parses (the same query fleet-serve's 2s health sampler runs).
func runNvidiaSmiMemory() (string, error) {
	out, err := winexec.Command("nvidia-smi", "--query-gpu=memory.total,memory.used", "--format=csv,noheader,nounits").Output()
	return string(out), err
}

// deferGen records a deferred gen result with latency stamped, keeping the four gen
// runners' defer paths uniform (defer-not-crash, invariant 4).
func (p *Pipeline) deferGen(req core.Request, meta core.Meta, start time.Time, inputChars int, reason string) core.Result {
	meta.LatencyMs = time.Since(start).Milliseconds()
	p.recordDefer(req.Task, meta, inputChars, reason)
	return core.Deferf(reason, "", meta)
}
