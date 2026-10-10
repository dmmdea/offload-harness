package gpuactivity

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// Options select what one Snapshot reads.
type Options struct {
	LockOverride string // config gpu_lock_path
	StateDir     string // config state_dir
	Endpoint     string // llama-swap base; "" = do not read the seat
	Seat         string // the agent seat (id or alias); "" = do not read the seat
	// SeatTimeout bounds the seat read. A status call must never hang on a seat
	// that is loading (llama-swap holds /upstream/<seat>/… for the whole load):
	// seatload reports `Starting` without touching the upstream, and this bound
	// covers a slow /running. Zero = 3 s.
	SeatTimeout time.Duration
	// SampleGPU runs nvidia-smi for utilization/memory and the processes on the
	// cards. Off in tests and where the box has no NVIDIA driver.
	SampleGPU bool
	// Sampler overrides how the cards are read when SampleGPU is true. Nil (the
	// production default) means the real SampleGPUs (nvidia-smi). A caller one
	// package up from gpuactivity cannot reach the package-private smiRun seam
	// (internal/gpuactivity/smi.go) to fake specific per-card numbers, so this
	// field is the seam at the Options boundary: it lets a verdict test (held vs
	// held-idle vs held-working depends on the ACTUAL util numbers, not just
	// on/off) inject idle or busy cards without touching the real driver. See
	// internal/mcpserver's statusGPUSampler var for how offload_status wires it.
	Sampler func(ctx context.Context) ([]GPU, error)
	// ProcSampler overrides how the processes on the cards are listed when SampleGPU is true. Nil
	// (the production default) means SampleProcesses (nvidia-smi --query-compute-apps). The same
	// seam as Sampler, for the same reason: how a display card's desktop is folded
	// (View.splitProcesses) is only testable through a status call with process rows of its own.
	ProcSampler func(ctx context.Context) ([]GPUProcess, error)
	// Scope fills in the effective cards of a legacy whole-node lease (the evidence rule lives
	// in modelaffinity, which imports this package, so the caller hands it in). nil = the
	// lease's declared devices only.
	Scope func(gpulease.Info) gpulease.Info
	// OrphanGrace is how long an attended lease's owner may be gone before the lease
	// reads as orphaned (config gpu_orphan_grace_min). Zero = gpulease.DefaultOrphanGrace.
	OrphanGrace time.Duration
	// ComfyDir is the ComfyUI install, read for the activity facts of a lease that has no
	// owner and no progress contract. "" = none.
	ComfyDir string
}

// Holder describes the lease holder beyond the lease record.
type Holder struct {
	// Devices are the cards the lease sits on as seats read it (lease ids): the declared
	// devices, else the inferred ones; empty = the whole node.
	Devices         []string  `json:"devices,omitempty"`
	PID             int       `json:"pid"`
	Alive           bool      `json:"alive"`
	Class           string    `json:"class"`
	Epoch           uint64    `json:"epoch"`
	Reason          string    `json:"reason,omitempty"`
	Origin          string    `json:"origin,omitempty"`
	Command         string    `json:"command,omitempty"`
	Exclusive       bool      `json:"exclusive"`
	Draining        bool      `json:"draining"`
	AgeSec          int       `json:"age_s"`
	HeartbeatAgeSec int       `json:"heartbeat_age_s"`
	ExpiresAt       time.Time `json:"-"`

	// --- ownership and standing (plan P8): derived by gpulease.Standing, never stored. ---
	Unattended    bool   `json:"unattended,omitempty"`
	OwnerState    string `json:"owner_state,omitempty"` // alive | gone | unknown | remote
	OwnerSession  string `json:"owner_session,omitempty"`
	OwnerPID      int    `json:"owner_pid,omitempty"`
	OrphanGraceS  int    `json:"orphan_grace_s,omitempty"`
	OrphanedSince string `json:"orphaned_since,omitempty"` // RFC3339, when the owner was first seen gone
	OrphanedForS  int    `json:"orphaned_for_s,omitempty"`
	Orphaned      bool   `json:"orphaned,omitempty"`
	Overdue       bool   `json:"overdue,omitempty"`
	OverdueBySec  int    `json:"overdue_by_s,omitempty"`
	Stalled       bool   `json:"stalled,omitempty"`
	// OwnerNote says why a RECORDED owner cannot be told apart (the session was never in the
	// registry, or the registry could not be read); OrphanMarkErr says the moment the owner
	// was first seen gone could not be recorded.
	OwnerNote     string         `json:"owner_note,omitempty"`
	OrphanMarkErr string         `json:"orphan_marker_error,omitempty"`
	TreeOrphan    bool           `json:"tree_orphan,omitempty"`
	Progress      *ProgressState `json:"progress,omitempty"`
	// Facts are information about a lease that has no owner and no progress contract
	// (newest ComfyUI output, ComfyUI log): never an input to the verdict.
	Facts []string `json:"activity_facts,omitempty"`

	// Terms (plan P9), from the lease record. Expired is the label the holder's tick stamped
	// when the term ended unrenewed; ExpiredWhy is its sentence. TermSec is the renewal term,
	// RequestedSec the window asked for when that was above the cap on a term, HardEnd (RFC3339)
	// the instant after which the lease is no longer renewed.
	Expired      bool   `json:"expired,omitempty"`
	ExpiredWhy   string `json:"expired_why,omitempty"`
	TermSec      int    `json:"term_s,omitempty"`
	RequestedSec int    `json:"requested_s,omitempty"`
	HardEnd      string `json:"hard_end,omitempty"`
}

// ProgressState is the reading of a lease's progress contract.
type ProgressState struct {
	File     string `json:"file"`
	State    string `json:"state"` // advancing | stalled | unknown
	AgeSec   int    `json:"age_s"`
	StallSec int    `json:"stall_s"`
	Detail   string `json:"detail,omitempty"`
	// Problem says why an unknown state is unknown ("does not exist", "cannot be read: ...").
	Problem string `json:"problem,omitempty"`
}

// SeatState is what llama-swap and the engine say about the agent seat.
type SeatState struct {
	Name      string `json:"name"`
	Canonical string `json:"canonical,omitempty"`
	Loaded    bool   `json:"loaded"`
	Starting  bool   `json:"starting"`
	Stopping  bool   `json:"stopping,omitempty"`
	Inflight  int    `json:"inflight"`
	Source    string `json:"source,omitempty"`
	Err       string `json:"error,omitempty"`
}

// View is one point-in-time answer.
type View struct {
	At     time.Time
	Held   bool
	Stale  bool // a lease record exists but its holder is provably gone or silent past its window
	Holder *Holder
	// Leases lists every live lease when MORE THAN ONE is held (card-scoped leases), each
	// with its own standing; Holder is the one the verdict is about (the most escalated).
	Leases    []Holder
	Seat      SeatState
	Runs      []Run
	GPUs      []GPU
	Processes []GPUProcess
	GPUErr    string
	// Verdict is one word a session can branch on; Note is the sentence behind
	// it. See Assess for the vocabulary.
	Verdict string
	Note    string
	// OtherDevices are the cards every OTHER live lease sits on (lease ids): work on them is
	// not the primary lease's holder's.
	OtherDevices []string
}

// trespassOf lists the cards that are busy under a bounded lease but outside its cards, with
// nothing the harness knows of to explain them (plan P5, `device-trespass`): the job MAY be using
// a card it did not claim, so a waiter that was handed that card as free could collide with it.
// It is a possibility, never an accusation: card-scoped leases make text seats on the free cards
// legal, and the harness registers the runs of only two doors (agent_run and the delegation
// door) and watches only the planner seat's gauge, so a cascade call on a router seat, an OCR or
// speech seat, or the desktop looks the same from here. Not flagged: the display card (the
// desktop's own use), a card another live lease holds, a card a registered run is pinned to,
// anything while the seat itself is busy (its card is unknown here, so the busy card cannot be
// attributed), and a whole-node lease (it has no outside).
func trespassOf(v View) []GPU {
	if !v.Held || v.Holder == nil || len(v.Holder.Devices) == 0 {
		return nil
	}
	if v.Seat.Inflight > 0 || (v.Seat.Starting && !v.Seat.Stopping) {
		return nil
	}
	held := map[string]bool{}
	for _, d := range v.Holder.Devices {
		held[strings.ToLower(strings.TrimSpace(d))] = true
	}
	for _, d := range v.OtherDevices {
		held[strings.ToLower(strings.TrimSpace(d))] = true
	}
	display := displayCards(v)
	var out []GPU
	for _, g := range v.GPUs {
		if !g.UtilKnown || g.UtilPct < utilBusyPct || display[g.UUID] || held[strings.ToLower(g.UUID)] {
			continue
		}
		if runOnCard(v.Runs, g) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// runOnCard reports whether any registered run is pinned to the card (by index or UUID prefix).
func runOnCard(runs []Run, g GPU) bool {
	idx := strconv.Itoa(g.Index)
	uuid := strings.ToLower(g.UUID)
	for _, r := range runs {
		if r.OnPin(idx) {
			return true
		}
		for _, d := range r.Devices {
			d = strings.ToLower(strings.TrimSpace(d))
			if len(d) >= 4 && strings.HasPrefix(uuid, d) {
				return true
			}
		}
	}
	return false
}

// trespassTail is the note for a trespass: empty when there is none.
func trespassTail(v View) string {
	t := trespassOf(v)
	if len(t) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t))
	for _, g := range t {
		parts = append(parts, describeCard(g))
	}
	return "; possible device-trespass: " + strings.Join(parts, ", ") + " is busy outside this lease's cards with nothing registered to explain it; this is not attributed to the holder (a text seat, another process or the desktop could be serving on it), so treat the card as possibly in use before handing it to a waiter"
}

// Verdict vocabulary.
const (
	VerdictFree        = "free"         // no lease, seat idle or absent, cards quiet
	VerdictLoadedIdle  = "loaded-idle"  // no lease; the seat is resident with nothing in flight (unloads at its ttl)
	VerdictWorking     = "working"      // a request or a registered run is in flight on the seat
	VerdictHeldWorking = "held-working" // a lease is held and the cards are busy under it (the holder's own job)
	VerdictHeldIdle    = "held-idle"    // a lease is held and NOTHING is running: seat idle, cards quiet
	VerdictBusyOutside = "busy-outside" // no lease, seat idle, but the cards are busy — work the harness does not own
	VerdictStaleHolder = "stale-holder" // a lease record whose holder is gone; the next acquirer reclaims it
	// Standing verdicts (plan P8). Highest precedence first: stale-holder, tree-orphan,
	// held-stalled, held-orphaned, held-overdue, then held-working and held-idle.
	VerdictTreeOrphan   = "tree-orphan"   // the wrapper is gone but its job tree is alive and keeps the cards
	VerdictHeldStalled  = "held-stalled"  // a progress contract exists and its file did not move inside its window
	VerdictHeldOrphaned = "held-orphaned" // an attended lease whose owner has been gone past the grace
	VerdictHeldOverdue  = "held-overdue"  // the declared window ended and the holder still holds the cards
	// utilBusyPct is the utilization above which a card counts as working for
	// the verdict. Below it a lease is held over an idle card.
	utilBusyPct = 15
)

// Snapshot composes the readings. Every leg is best-effort and independently
// reported: an unreadable seat or an absent nvidia-smi narrows the verdict's
// evidence, named in the View, and never fails the call.
func Snapshot(ctx context.Context, opts Options) View {
	now := time.Now()
	v := View{At: now}

	leaseDir, lerr := gpulease.LeaseDir(opts.LockOverride, opts.StateDir)
	if lerr == nil {
		info, meta, reclaimable := gpulease.InspectDirDetail(leaseDir)
		if opts.Scope != nil && info.Held {
			info = opts.Scope(info)
		}
		switch {
		case info.Held:
			v.Held = true
			v.Leases, v.Holder = holdersOf(leaseDir, info, opts, now)
			if v.Holder != nil {
				for _, l := range info.Each() {
					if l.Epoch != v.Holder.Epoch {
						v.OtherDevices = append(v.OtherDevices, l.EffectiveDevices()...)
					}
				}
			}
		case meta != nil && reclaimable:
			v.Stale = true
			v.Holder = &Holder{PID: meta.Holder.PID, Alive: gpulease.PIDAlive(meta.Holder.PID), Class: string(meta.Class), Epoch: meta.Epoch, Reason: meta.Reason, Command: meta.Command}
		}
		reg := OpenAt(DirBeside(leaseDir))
		v.Runs = reg.List(now)
	}

	if opts.Endpoint != "" && opts.Seat != "" {
		to := opts.SeatTimeout
		if to <= 0 {
			to = 3 * time.Second
		}
		sctx, cancel := context.WithTimeout(ctx, to)
		rd, err := seatload.Inflight(sctx, &http.Client{Timeout: to}, opts.Endpoint, opts.Seat)
		cancel()
		v.Seat = SeatState{Name: opts.Seat, Canonical: rd.Canonical, Loaded: rd.Loaded, Starting: rd.Starting, Stopping: rd.Stopping, Inflight: rd.Inflight, Source: rd.Source}
		if err != nil {
			v.Seat.Err = err.Error()
		}
		// Runs on OTHER seats are still runs on this box's cards; keep them all,
		// but the seat block's count is what the drain would see.
	}

	if opts.SampleGPU {
		sample := opts.Sampler
		if sample == nil {
			sample = SampleGPUs
		}
		gpus, gerr := sample(ctx)
		if gerr != nil {
			v.GPUErr = "nvidia-smi: " + gerr.Error()
		} else {
			v.GPUs = gpus
			procSample := opts.ProcSampler
			if procSample == nil {
				procSample = SampleProcesses
			}
			if procs, perr := procSample(ctx); perr == nil {
				v.Processes = procs
			}
		}
	}

	v.Verdict, v.Note = Assess(v)
	return v
}

// Assess derives the verdict from a View's readings. Pure, so the vocabulary is
// testable without a lease, a seat or a driver.
func Assess(v View) (verdict, note string) {
	now := v.At
	if now.IsZero() {
		now = time.Now()
	}
	// A seat that is STOPPING is on its way out, not work: counting it made a
	// ttl unload read "WORKING — agent-pool is loading" (2026-09-23).
	seatBusy := (v.Seat.Starting && !v.Seat.Stopping) || v.Seat.Inflight > 0
	runs := len(v.Runs)

	// TWO readings, because "a card is busy" and "the HOLDER is working" are not
	// the same claim. maxUtil is every card, and answers "is anything using this
	// box" (busy-outside). workUtil skips cards the harness provably cannot run
	// on, and is the only thing allowed to say the holder is working.
	//
	// 2026-09-20, <node-b>, while the operator played a game: the verdict read
	// `held-working — 33% on card 1 (RTX 5070 Ti)` while the lease holder had
	// burned 4 SECONDS of CPU in 141 minutes and the two cards it actually
	// fenced sat at 0%. Card 1 is the display card and that 33% was the game.
	// Because held-working outranks held-idle, the verdict that exists for
	// exactly this case could never fire on a box anyone was using, and three
	// jobs queued behind a holder that was doing nothing.
	display := displayCards(v)
	// A lease that names its cards is working when THOSE cards are: a busy card outside the
	// set is a seat's, another lease's, or a trespass (below), never the holder's own job.
	var leaseCards map[string]bool
	if v.Held && v.Holder != nil && len(v.Holder.Devices) > 0 {
		leaseCards = map[string]bool{}
		for _, d := range v.Holder.Devices {
			leaseCards[strings.ToLower(strings.TrimSpace(d))] = true
		}
	}
	maxUtil, utilKnown, busyCard := -1, false, ""
	workUtil, workKnown, workCard := -1, false, ""
	for _, g := range v.GPUs {
		if !g.UtilKnown {
			continue
		}
		utilKnown = true
		if g.UtilPct > maxUtil {
			maxUtil = g.UtilPct
			busyCard = describeCard(g)
		}
		if display[g.UUID] {
			continue
		}
		if leaseCards != nil && !leaseCards[strings.ToLower(g.UUID)] {
			continue
		}
		workKnown = true
		if g.UtilPct > workUtil {
			workUtil = g.UtilPct
			workCard = describeCard(g)
		}
	}
	cardsBusy := utilKnown && maxUtil >= utilBusyPct
	workCardsBusy := workKnown && workUtil >= utilBusyPct
	// What the operator's own machine is doing, named, so a reader never has to
	// re-derive why a busy box still reads idle under the lease.
	foreignTail := ""
	if cardsBusy && !workCardsBusy {
		foreignTail = " — the cards ARE busy (" + busyCard + ") but that is the display card, and with no seat request in flight that load is the desktop's, not the holder's work"
	}

	work := describeWork(v, now)
	// A stale record never outranks live work: on 2026-09-14 <node-c> read
	// `stale-holder — nothing is running under it` while its seat was loading for
	// a delegated run. The record is reported as a tail on whatever IS running,
	// and is the verdict only when nothing else is.
	staleTail := ""
	if v.Stale && v.Holder != nil {
		staleTail = fmt.Sprintf("; a lease record left over from a holder that is gone (pid %d, reason %q) sits beside it — the next `gpu reserve` reclaims it", v.Holder.PID, v.Holder.Reason)
	}
	h := v.Holder
	progressAdvancing := v.Held && h != nil && h.Progress != nil && h.Progress.State == "advancing"
	switch {
	case v.Held && (seatBusy || runs > 0):
		if head := standingHead(v); head != "" {
			return VerdictWorking, head + work + holderTail(v)
		}
		return VerdictWorking, "the lease is held AND work is in flight on the seat: " + work + holderTail(v)
	case v.Held && h != nil && h.TreeOrphan:
		return VerdictTreeOrphan, "the lease's wrapper is gone but the job it started is still alive and holds the cards: the claim is kept until that job ends or is taken over" + takeoverHint(h) + holderTail(v)
	case v.Held && h != nil && h.Stalled:
		return VerdictHeldStalled, stalledNote(h) + takeoverHint(h) + factsTail(h) + holderTail(v)
	case v.Held && h != nil && h.Orphaned:
		return VerdictHeldOrphaned, orphanedNote(h) + takeoverHint(h) + factsTail(h) + holderTail(v)
	case v.Held && h != nil && h.Overdue:
		return VerdictHeldOverdue, overdueNote(h) + takeoverHint(h) + factsTail(h) + holderTail(v)
	case v.Held && workCardsBusy:
		return VerdictHeldWorking, "the lease is held and the cards are busy under it — " + workCard + "; the seat is idle, so this is the holder's own job" + trespassTail(v) + evidenceTail(h) + ownerCaveat(h) + factsTail(h) + holderTail(v)
	case progressAdvancing:
		return VerdictHeldWorking, "the lease is held and its progress file is advancing (" + progressSentence(h) + "); the seat is idle and the cards are quiet" + ownerCaveat(h) + factsTail(h) + holderTail(v)
	case v.Held:
		n := "the lease is held but NOTHING is running on the cards right now: no request in flight on the seat"
		if v.Seat.Name == "" {
			n = "the lease is held but nothing the harness can see is running"
		}
		if workKnown {
			n += fmt.Sprintf(", GPU utilization at most %d%% on the cards it can run on", workUtil)
		} else if utilKnown {
			n += fmt.Sprintf(", GPU utilization at most %d%%", maxUtil)
		} else if v.GPUErr != "" {
			n += " (GPU utilization unknown: " + v.GPUErr + ")"
		}
		n += foreignTail
		n += " — the holder is waiting (a drain, a queue), loading, or stalled" + trespassTail(v) + evidenceTail(h) + ownerCaveat(h) + factsTail(h) + holderTail(v)
		return VerdictHeldIdle, n
	case seatBusy || runs > 0:
		return VerdictWorking, "unreserved, and work is in flight on the seat: " + work + staleTail
	case v.Stale:
		who := ""
		if v.Holder != nil {
			who = fmt.Sprintf(" (pid %d, reason %q)", v.Holder.PID, v.Holder.Reason)
		}
		return VerdictStaleHolder, "a lease record is left over from a holder that is gone" + who + "; the next `gpu reserve` reclaims it — nothing is running under it"
	case cardsBusy:
		return VerdictBusyOutside, "no lease and the seat is idle, but the cards are busy — " + busyCard + "; that is work the harness does not own" + processTail(v) + staleTail
	case v.Seat.Stopping:
		return VerdictLoadedIdle, fmt.Sprintf("no lease; %s is unloading (its ttl ran out or an unload was asked), nothing in flight", v.Seat.Name) + staleTail
	case v.Seat.Loaded:
		return VerdictLoadedIdle, fmt.Sprintf("no lease; %s is resident with nothing in flight and unloads at its ttl", v.Seat.Name) + staleTail
	default:
		return VerdictFree, "no lease, no request in flight, cards quiet"
	}
}

// describeCard is the one phrasing for "which card, how busy".
func describeCard(g GPU) string {
	return fmt.Sprintf("%d%% on card %d (%s)", g.UtilPct, g.Index, g.Name)
}

// displayCards returns, by UUID, the cards driving a display — utilization
// there is never a lease holder's work. The rule itself lives in
// gpuprobe.DisplayCardUUIDs so the fleet node's placement figure uses the SAME
// one; this is only the adapter from a View's readings.
func displayCards(v View) map[string]bool {
	devs := make([]gpuprobe.Device, 0, len(v.GPUs))
	for _, g := range v.GPUs {
		devs = append(devs, gpuprobe.Device{UUID: g.UUID, DisplayActive: g.DisplayActive})
	}
	return gpuprobe.DisplayCardUUIDs(devs)
}

func describeWork(v View, now time.Time) string {
	var parts []string
	switch {
	case v.Seat.Stopping:
		parts = append(parts, fmt.Sprintf("%s is unloading", v.Seat.Name))
	case v.Seat.Starting:
		parts = append(parts, fmt.Sprintf("%s is loading", v.Seat.Name))
	case v.Seat.Inflight > 0:
		src := v.Seat.Source
		if src == "" {
			src = "seat"
		}
		parts = append(parts, fmt.Sprintf("%d request(s) in flight on %s (%s)", v.Seat.Inflight, v.Seat.Name, src))
	}
	if len(v.Runs) > 0 {
		runs := make([]string, 0, len(v.Runs))
		for _, r := range v.Runs {
			runs = append(runs, r.Summary(now))
		}
		parts = append(parts, fmt.Sprintf("%d registered run(s): %s", len(v.Runs), strings.Join(runs, "; ")))
	}
	return strings.Join(parts, "; ")
}

func holderTail(v View) string {
	if v.Holder == nil {
		return ""
	}
	h := v.Holder
	s := fmt.Sprintf(" [holder pid %d", h.PID)
	if !h.Alive {
		s += " NOT ALIVE"
	}
	s += fmt.Sprintf(", held %ds", h.AgeSec)
	if h.HeartbeatAgeSec > 0 {
		s += fmt.Sprintf(", heartbeat %ds ago", h.HeartbeatAgeSec)
	}
	if h.Draining {
		s += ", draining"
	}
	if h.Exclusive {
		s += ", exclusive"
	}
	if h.Command != "" {
		s += ", running: " + h.Command
	}
	return s + "]"
}

func processTail(v View) string {
	listed, folded := v.splitProcesses()
	if len(listed) == 0 && len(folded) == 0 {
		return ""
	}
	var parts []string
	if len(listed) > 0 {
		names := make([]string, 0, len(listed))
		for _, p := range listed {
			names = append(names, fmt.Sprintf("%s (pid %d)", shortName(p.Name), p.PID))
		}
		sort.Strings(names)
		const keep = 6
		more := ""
		if len(names) > keep {
			more = fmt.Sprintf(" and %d more", len(names)-keep)
			names = names[:keep]
		}
		parts = append(parts, "on the cards: "+strings.Join(names, ", ")+more)
	}
	// The desktop is counted, not named: sorted by name and cut at six, thirty window rows pushed
	// the one process that is not the desktop into "and N more" (the same noise as the JSON list).
	for _, f := range folded {
		parts = append(parts, f.Sentence())
	}
	return "; " + strings.Join(parts, "; ")
}

// DisplayCardProcs is the one summary that stands in for the processes nvidia-smi lists on a
// display card and cannot size. On Windows (WDDM) that is the whole desktop: explorer, every
// browser, the chat apps, 31 rows on the reference 3-card box, each used_known=false. Listed one
// by one they buried the single process that mattered (a python on a work card) in the lease view
// an agent reads first (F18, 2026-10-09), and they say nothing about the lease. Counted per card
// instead; the complete list is nvidia-smi's own (docs/systems/gpu-lease.md).
type DisplayCardProcs struct {
	Index   int    `json:"index"`
	GPUUUID string `json:"gpu_uuid,omitempty"`
	Name    string `json:"name"`
	Count   int    `json:"count"`
}

// Sentence is the summary as `gpu status` prints it.
func (d DisplayCardProcs) Sentence() string {
	noun := "desktop processes"
	if d.Count == 1 {
		noun = "desktop process"
	}
	return fmt.Sprintf("%d %s on the display card (card %d, %s), memory unknown (WDDM)", d.Count, noun, d.Index, d.Name)
}

// screenCards returns, lower-cased, the cards the card table marks "display": display_active OR
// display_attached (gpuprobe.ScreenCardUUIDs, the placement rule). The desktop lives on the
// monitor's card, and a sleeping screen closes none of its windows; but with the screen asleep
// display_active reads Disabled on every card while display_attached stays Yes on the monitor's
// (display.go, measured 2026-10-03), so the load-attribution rule (displayCards: display_active
// alone) would fold nothing exactly when an unattended session reads the lease. Empty on a box
// with no non-display card: its only card is its work card (the single-card guard every display
// rule carries).
func screenCards(v View) map[string]bool {
	devs := make([]gpuprobe.Device, 0, len(v.GPUs))
	for _, g := range v.GPUs {
		devs = append(devs, gpuprobe.Device{UUID: g.UUID, DisplayActive: g.DisplayActive, DisplayAttached: g.DisplayAttached})
	}
	out := map[string]bool{}
	for id := range gpuprobe.ScreenCardUUIDs(devs) {
		out[strings.ToLower(id)] = true
	}
	return out
}

// splitProcesses separates the processes worth a row of their own from the ones a display card's
// summary stands for. A row is folded only when BOTH hold: nvidia-smi could not size it
// (UsedKnown false, which is WDDM's answer for every process) AND it sits on a card that drives a
// screen. A row with a known size, a row on any other card (the python that holds a work card is
// unsized too) and a row that names no card stay listed: absent evidence is never "it is only the
// desktop". Counts are distinct pids per card. With no display card the processes come back as
// they are (nil stays nil, so a sample-less view keeps omitting the key); with one, listed is
// never nil.
func (v View) splitProcesses() (listed []GPUProcess, folded []DisplayCardProcs) {
	screen := screenCards(v)
	if len(screen) == 0 {
		return v.Processes, nil
	}
	cards := make(map[string]GPU, len(screen))
	for _, g := range v.GPUs {
		cards[strings.ToLower(g.UUID)] = g
	}
	pids := map[string]map[int]bool{}
	listed = make([]GPUProcess, 0, len(v.Processes))
	for _, p := range v.Processes {
		id := strings.ToLower(p.GPUUUID)
		if p.UsedKnown || !screen[id] {
			listed = append(listed, p)
			continue
		}
		if pids[id] == nil {
			pids[id] = map[int]bool{}
		}
		pids[id][p.PID] = true
	}
	for id, on := range pids {
		g := cards[id]
		folded = append(folded, DisplayCardProcs{Index: g.Index, GPUUUID: g.UUID, Name: g.Name, Count: len(on)})
	}
	sort.Slice(folded, func(i, j int) bool { return folded[i].Index < folded[j].Index })
	return listed, folded
}

func shortName(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Map is the JSON shape shared by `gpu status --json` and offload_status.
func (v View) Map() map[string]any {
	m := map[string]any{
		"verdict": v.Verdict,
		"note":    v.Note,
		"seat":    v.Seat,
		"runs":    v.Runs,
	}
	if v.Runs == nil {
		m["runs"] = []Run{}
	}
	if v.Holder != nil {
		m["holder"] = v.Holder
	}
	if len(v.Leases) > 1 {
		m["leases"] = v.Leases
	}
	if v.Stale {
		m["stale_lease"] = true
	}
	if v.GPUs != nil {
		m["gpus"] = v.GPUs
	}
	if v.Processes != nil {
		// A display card's unsizable processes are one count per card, not one row per window
		// (DisplayCardProcs); everything else is listed as it always was.
		listed, folded := v.splitProcesses()
		m["gpu_processes"] = listed
		if len(folded) > 0 {
			m["display_card_processes_unknown"] = folded
		}
	}
	if v.GPUErr != "" {
		m["gpu_error"] = v.GPUErr
	}
	if t := trespassOf(v); len(t) > 0 {
		m["device_trespass"] = t
	}
	return m
}

// Lines is the CLI rendering (`gpu status`).
func (v View) Lines() []string {
	out := []string{fmt.Sprintf("activity: %s — %s", strings.ToUpper(v.Verdict), v.Note)}
	if v.Seat.Name != "" {
		s := fmt.Sprintf("seat %s:", v.Seat.Name)
		switch {
		case v.Seat.Err != "":
			s += " unreadable (" + v.Seat.Err + ")"
		case v.Seat.Stopping:
			s += " unloading"
		case v.Seat.Starting:
			s += " loading"
		case !v.Seat.Loaded:
			s += " not loaded"
		default:
			s += fmt.Sprintf(" loaded, %d in flight (%s)", v.Seat.Inflight, v.Seat.Source)
		}
		out = append(out, s)
	}
	if len(v.GPUs) > 0 {
		cards := make([]string, 0, len(v.GPUs))
		for _, g := range v.GPUs {
			u := "util ?"
			if g.UtilKnown {
				u = fmt.Sprintf("%d%%", g.UtilPct)
			}
			cards = append(cards, fmt.Sprintf("%d %s %s %.1f/%.1f GiB", g.Index, g.Name, u, float64(g.MemUsedMiB)/1024, float64(g.MemTotalMiB)/1024))
		}
		out = append(out, "cards: "+strings.Join(cards, " | "))
	} else if v.GPUErr != "" {
		out = append(out, "cards: "+v.GPUErr)
	}
	return out
}
