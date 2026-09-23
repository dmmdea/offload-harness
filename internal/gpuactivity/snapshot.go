package gpuactivity

import (
	"context"
	"fmt"
	"net/http"
	"sort"
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
}

// Holder describes the lease holder beyond the lease record.
type Holder struct {
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
	At        time.Time
	Held      bool
	Stale     bool // a lease record exists but its holder is provably gone or silent past its window
	Holder    *Holder
	Seat      SeatState
	Runs      []Run
	GPUs      []GPU
	Processes []GPUProcess
	GPUErr    string
	// Verdict is one word a session can branch on; Note is the sentence behind
	// it. See Assess for the vocabulary.
	Verdict string
	Note    string
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
		switch {
		case info.Held:
			v.Held = true
			v.Holder = &Holder{
				PID: info.PID, Alive: gpulease.PIDAlive(info.PID), Class: string(info.Class), Epoch: info.Epoch,
				Reason: info.Reason, Origin: info.Origin, Command: info.Command,
				Exclusive: info.Exclusive, Draining: info.Draining,
				AgeSec: int(info.Age.Seconds()), ExpiresAt: info.ExpiresAt,
			}
			if !info.HeartbeatAt.IsZero() {
				v.Holder.HeartbeatAgeSec = int(now.Sub(info.HeartbeatAt).Seconds())
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
			if procs, perr := SampleProcesses(ctx); perr == nil {
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
	// 2026-09-20, the Qube, while the operator played a game: the verdict read
	// `held-working — 33% on card 1 (RTX 5070 Ti)` while the lease holder had
	// burned 4 SECONDS of CPU in 141 minutes and the two cards it actually
	// fenced sat at 0%. Card 1 is the display card and that 33% was the game.
	// Because held-working outranks held-idle, the verdict that exists for
	// exactly this case could never fire on a box anyone was using, and three
	// jobs queued behind a holder that was doing nothing.
	display := displayCards(v)
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
	// A stale record never outranks live work: on 2026-09-14 the Lenovo read
	// `stale-holder — nothing is running under it` while its seat was loading for
	// a delegated run. The record is reported as a tail on whatever IS running,
	// and is the verdict only when nothing else is.
	staleTail := ""
	if v.Stale && v.Holder != nil {
		staleTail = fmt.Sprintf("; a lease record left over from a holder that is gone (pid %d, reason %q) sits beside it — the next `gpu reserve` reclaims it", v.Holder.PID, v.Holder.Reason)
	}
	switch {
	case v.Held && (seatBusy || runs > 0):
		return VerdictWorking, "the lease is held AND work is in flight on the seat: " + work + holderTail(v)
	case v.Held && workCardsBusy:
		return VerdictHeldWorking, "the lease is held and the cards are busy under it — " + workCard + "; the seat is idle, so this is the holder's own job" + holderTail(v)
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
		n += " — the holder is waiting (a drain, a queue), loading, or stalled" + holderTail(v)
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
	if len(v.Processes) == 0 {
		return ""
	}
	names := make([]string, 0, len(v.Processes))
	for _, p := range v.Processes {
		names = append(names, fmt.Sprintf("%s (pid %d)", shortName(p.Name), p.PID))
	}
	sort.Strings(names)
	const keep = 6
	more := ""
	if len(names) > keep {
		more = fmt.Sprintf(" and %d more", len(names)-keep)
		names = names[:keep]
	}
	return "; on the cards: " + strings.Join(names, ", ") + more
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
	if v.Stale {
		m["stale_lease"] = true
	}
	if v.GPUs != nil {
		m["gpus"] = v.GPUs
	}
	if v.Processes != nil {
		m["gpu_processes"] = v.Processes
	}
	if v.GPUErr != "" {
		m["gpu_error"] = v.GPUErr
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
