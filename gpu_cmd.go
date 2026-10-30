package main

// `local-offload gpu` — the operator-facing half of the machine-wide GPU lease.
//
// This is the verb that makes the lease usable by work the harness does not own. The
// incident it prevents: a benchmark was running against llama-swap when a media job
// arrived through a different ingress, unloaded every GPU-resident model, and killed
// the measurement. Text work never took the lock, so nothing could have stopped it.
// `gpu reserve --class text` gives that work a way to hold the card, and a render then
// WAITS instead of tearing the tier down.
//
//	local-offload gpu status [--json]
//	local-offload gpu cards [--json]
//	local-offload gpu reserve --class text --for 45m --reason "kv bench" -- <command...>
//	local-offload gpu reserve --class text --for 45m --reason "kv bench" --detach
//	local-offload gpu reserve --class media --devices 0,2 --for 2h -- <command...>
//	local-offload gpu reserve --class media --cards 2 --for 2h -- <command...>
//	local-offload gpu release [--epoch N]
//	local-offload gpu doctor [--scan dir] [--write-audit] [--json]
//	local-offload gpu owner-flags [--pid N]
//
// The WRAPPER form is the one to prefer: the lease lives exactly as long as the
// command and cannot be leaked by forgetting to release, the way `timeout` or `nice`
// compose. --detach exists for an interactive session that wants the card held across
// several commands, and it is the weaker form by design.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpucards"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/proctree"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

// gpuVerbs are the subcommands runGPU dispatches, in the order the usage lists them. `hold` is the hidden
// holder a --detach reserve spawns and is not documented.
var gpuVerbs = []string{"status", "cards", "faults", "reserve", "release", "doctor", "owner-flags", "hold"}

func runGPU(args []string) error {
	if len(args) == 0 {
		return errors.New(gpuUsageLine)
	}
	// A --config in front of the subcommand belongs to the subcommand, as it does in front of any other
	// verb (main.go hoistGlobalConfig); without this `gpu --config x status` dispatched on "--config" and
	// failed as an unknown subcommand (F35).
	sub, rest, ok := hoistGlobalConfig(args)
	if !ok {
		return errors.New(gpuUsageLine)
	}
	switch sub {
	case "-h", "-help", "--help", "help":
		fmt.Print(gpuUsage())
		return nil
	case "status":
		return runGPUStatus(rest)
	case "reserve":
		return runGPUReserve(rest)
	case "release":
		return runGPURelease(rest)
	case "hold":
		return runGPUHold(rest)
	case "cards":
		return runGPUCards(rest)
	case "faults":
		return runGPUFaults(rest)
	case "doctor":
		return runGPUDoctor(rest)
	case "owner-flags":
		return runGPUOwnerFlags(rest)
	default:
		return fmt.Errorf("unknown gpu subcommand %q (want status, cards, faults, reserve, release, doctor or owner-flags; `local-offload gpu --help` lists them)", sub)
	}
}

// gpuUsageLine is the one-line usage a bare `gpu` (or a `gpu --config x` with no subcommand) is refused with.
const gpuUsageLine = "usage: local-offload gpu [--config <path>] <status|cards|faults|reserve|release|doctor|owner-flags> [flags] (local-offload gpu --help lists them)"

// gpuUsage is what `gpu --help` prints on stdout: every documented subcommand once, and where the flags of
// each are (the sub-verbs' own flag sets print them on --help).
func gpuUsage() string {
	return `local-offload gpu: the machine-wide GPU lease (who holds the cards, and a place in line for everyone else)

Usage:
  local-offload gpu [--config <path>] <subcommand> [flags]

Subcommands:
  gpu status [--json]                    who holds the cards, the queue behind them, and what the holder is doing (verdict and note)
  gpu cards [--json]                     the card table, with each card's holder
  gpu faults [--days N] [--jobs] [--json]
                                         the driver's fault history (up to 30 days, every card) as incidents, with known-error
                                         classes and NVIDIA's action; --jobs adds the ledger rows that ran here during each
  gpu reserve [flags] -- <command...>    hold the cards for exactly as long as <command> runs, queueing behind a current holder
  gpu reserve --detach [flags]           hold the cards in a background process until it is released (needs an explicit --for)
  gpu release [--epoch N] [--warm-seat]  release the lease held (or only epoch N, if it is still current); --warm-seat loads the agent seat back
  gpu doctor [--scan dir] [--write-audit] [--json]
                                         audit the lease readers on this host (harness binaries and gpu-lock.mjs copies)
  gpu owner-flags [--pid N]              print the owner flags for the calling process tree, to splice into a launcher that detaches

--config <path> may come before the subcommand or after it. "local-offload gpu <subcommand> --help" lists that subcommand's flags.
`
}

// openLease resolves the state root the same way every consumer does, so the CLI can
// never end up arbitrating a different lease than the render path.
func openLease(fs *flag.FlagSet) (*gpulease.Manager, error) {
	cfg := loadCfg(fs)
	// gpu_lock_path MUST be honoured here. Ignoring it while the render path and the
	// vision gate obeyed it put the reservation verb on a different directory from the
	// renders it is supposed to arbitrate against — they never contended, which is the
	// original incident with no warning.
	m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		return nil, err
	}
	// The per-host switch for card-scoped leases (config gpu_card_scoped_leases, default
	// off). Reading a card-scoped directory is never gated; only writing one is, and the
	// config key alone does not enable it: the host also needs a green reader audit
	// (see gpulease.ApplyCardScopedConfig). A host that asked and has none keeps the
	// writer off and is told why, once per command.
	if err := m.ApplyCardScopedConfig(cfg.GPUCardScopedLeases); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	return m, nil
}

// Seams for the live reads behind `gpu status`, so a test prints a synthetic host instead of
// calling nvidia-smi and the Windows performance counters.
var (
	statusActivityFn = gpuactivity.Snapshot
	statusForeignFn  = foreignGPUHolders
)

func runGPUStatus(args []string) error {
	fs := flag.NewFlagSet("gpu status", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	m, err := openLease(fs)
	if err != nil {
		return err
	}
	// The readings below begin here: a marker stamped after this instant belongs to a lease
	// they cannot speak for (see the stale-marker clear).
	readsBegan := time.Now()
	// With the scope the seat gates read (plan P4): a legacy whole-node lease arrives with the
	// cards the evidence rule scoped it to, or with the reason it stayed whole-node.
	info := modelaffinity.ScopeInfo(m.Dir(), m.Inspect())
	// What the cards are DOING (0.117.0, register D-93): the seat's in-flight
	// count, the registered runs, a utilization sample and one verdict.
	act := statusActivityFn(context.Background(), activityOptions(loadCfg(fs)))
	// Who is queued (register D-124): the line behind the holder, and whether
	// the agent seat is owed a warm-back by the last of them.
	waiters := m.Waiters()
	warmOwed := m.SeatWarmOwed()
	// A marker the readings prove moot is removed, and said. The readings above took as long as
	// the whole activity snapshot, so the clear itself checks what they cannot: that the marker
	// is older than they are, and that no lease is live now. When it declines (a lease stamped
	// a fresh marker meanwhile, or another status run got there first) the live value is
	// reported, not the one read before.
	clearedStale := ""
	if warmOwedIsStale(info, act, warmOwed) {
		if m.ClearSeatWarmOwedIfStale(warmOwed, readsBegan) {
			clearedStale, warmOwed = warmOwed, ""
		} else {
			warmOwed = m.SeatWarmOwed()
		}
	}
	// Non-harness processes holding significant VRAM right now (register
	// D-1xx-4, 2026-09-23): visible here too, not only at acquire, because a
	// session reading `gpu status` mid-investigation deserves the same
	// evidence a fresh `gpu reserve` would have printed.
	foreign := statusForeignFn(context.Background(), loadCfg(fs))
	// The per-card table (plan P3): which card each lease holds, and what is free.
	// Read-only, and never a reason for status to fail: no nvidia-smi prints the leases
	// and says there is no table.
	// cardCfg is the config the table is read with and the display cap is judged by (gpucards.RowsWithCap).
	cardCfg := loadCfg(fs)
	cards, cardsNote, cardsErr := cardTable(context.Background(), cardCfg)
	if cardsErr != nil {
		cards, cardsNote = nil, fmt.Sprintf("no card table (%v)", cardsErr)
	}
	leases := modelaffinity.ScopeLeases(m.Dir(), m.Leases())
	// The host's memory and what the live leases declared of it (internal/gpulease/hostram.go): the
	// verdict OK / NEAR / OVER, OVER being no memory available, the commit charge above physical RAM (the line a commit nobody
	// stated is held to), or the commit charge reaching an enforced commit limit.
	hostMem, hostOK := gpuprobe.ReadHostMemory()
	host := gpucards.NewHostView(hostMem, hostOK, loadCfg(fs).GPUHostRAMHeadroom(), gpulease.DeclaredHostRAMLoad(leases), m.HostRAMPendingLoad())
	if *asJSON {
		queued := gpucards.QueueRows(waiters)
		foreignJSON := make([]map[string]any, 0, len(foreign))
		for _, h := range foreign {
			foreignJSON = append(foreignJSON, map[string]any{"name": h.Name, "pid": h.PID, "mib": h.MiB})
		}
		// With several live leases (card-scoped) `info` is the LOWEST epoch while the verdict
		// and activity.holder are about the most escalated one: the top-level lease fields
		// describe that lease, from its own record, so one JSON never holds one lease's owner
		// beside another's standing. epochs[] lists every live lease.
		hl := gpuactivity.HeadlineOf(info, act.Holder)
		out := map[string]any{
			"held": hl.Held, "class": hl.Class, "epoch": hl.Epoch, "pid": hl.PID,
			"age_s": int(hl.Age.Seconds()), "reason": hl.Reason, "origin": hl.Origin,
			"job_id": hl.JobID, "expires_at": hl.ExpiresAt.Format(time.RFC3339),
			"exclusive": hl.Exclusive, "draining": hl.Draining, "command": hl.Command, "state_root": m.Root(),
			"queued": queued, "seat_warm_owed": warmOwed,
			"foreign_gpu_holders": foreignJSON,
			"verdict":             act.Verdict, "activity": act.Map(),
			// The next step, spelled out: a session reading "held" used to conclude
			// "refuse the work"; the honest answer is "queue behind it".
			"queue_with": queueHint,
		}
		// The seat whose stale warm-back marker this run removed (the key is only present when
		// one was), so a reader of the JSON can tell "nothing was owed" from "it was just cleared".
		if clearedStale != "" {
			out["seat_warm_owed_cleared"] = clearedStale
		}
		// Card-scoped leases (P2): the cards a lease holds and every live epoch. Only
		// present when they matter, so a whole-node host's output is unchanged.
		if len(hl.Devices) > 0 {
			out["devices"] = hl.Devices
		}
		// Ownership (P8): who asked for the lease and the contract it is judged by. The
		// derived standing (owner state, orphaned-since, progress age) is under
		// activity.holder, from the same reading as the verdict, and about the same lease.
		if hl.Owner != nil {
			out["owner"] = hl.Owner
		}
		if hl.Unattended {
			out["unattended"] = true
		}
		if hl.Progress != nil {
			out["progress"] = hl.Progress
		}
		if len(info.Epochs) > 1 || len(hl.Devices) > 0 {
			out["epochs"] = info.Epochs
		}
		// What the seat gates read (plan P4). seat_scope is declared, inferred or whole-node;
		// scope_why is the evidence, or the exact reason a legacy lease stayed whole-node.
		if info.Held {
			out["seat_scope"] = string(info.ScopeKind())
			if len(info.Inferred) > 0 && len(info.Devices) == 0 {
				out["inferred_devices"] = info.Inferred
			}
			if info.ScopeWidened {
				out["scope_widened"] = true
			}
			if info.ScopeWhy != "" {
				out["scope_why"] = info.ScopeWhy
			}
		}
		// The per-card table, the per-lease list and the card-scoped switch (P3). Keys
		// are only ever added: `queued` keeps its four keys and gains devices/scope.
		for k, v := range statusLeaseSection(m, cardCfg, cards, cardsNote, waiters) {
			out[k] = v
		}
		out["card_scoped_leases"] = m.CardScoped()
		out["host_memory"] = host.Map()
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	// The line and the owed warm are printed held or free: a warm can be owed
	// while the card is free (a fenced-out holder pays nothing; the marker
	// waits for the next last holder).
	queueLines := func() {
		if len(waiters) > 0 {
			parts := make([]string, 0, len(waiters))
			for _, w := range waiters {
				scope := ""
				if len(w.Devices) > 0 {
					scope = ", cards " + strings.Join(w.Devices, ",")
				}
				if w.WaitingFor == gpulease.WaitHostRAM {
					// The cards would admit it; the host's memory does not yet.
					scope += fmt.Sprintf(", waiting for host RAM (needs %.1f GiB)", w.HostRAMGiB)
				}
				parts = append(parts, fmt.Sprintf("pid %d (%s%s, %s in line)", w.PID, w.Class, scope, time.Since(w.Since()).Round(time.Second)))
			}
			fmt.Printf("  queued: %d — %s\n", len(waiters), strings.Join(parts, ", "))
		}
		if clearedStale != "" {
			fmt.Printf("  cleared the stale warm-back marker for %s: the seat is loaded and no lease holds the card\n", clearedStale)
		}
		if warmOwed != "" {
			fmt.Printf("  seat warm-back owed: %s (paid by the last holder to release)\n", warmOwed)
		}
		if w := formatForeignWarning(foreign); w != "" {
			fmt.Printf("  %s\n", w)
		}
	}
	if !info.Held {
		// Say the card is UNRESERVED explicitly. The lease only binds code paths that
		// take it, so an unreserved card is exactly when a bench is exposed — that
		// should be visible, not inferred from silence.
		fmt.Printf("GPU: free (unreserved)  state root: %s\n", m.Root())
		printCardTable(cardCfg, cards, cardsNote, cardsErr, leases) // a free host
		fmt.Println(host.Line())
		queueLines()
		printActivity(act)
		return nil
	}
	excl := ""
	if info.Exclusive {
		excl = "  (exclusive: text loads wait or route elsewhere)"
	}
	if info.Draining {
		excl += "  (draining: new runs wait, in-flight runs complete)"
	}
	if !info.AsksForHostRAM() && !info.HolderGaveUp.IsZero() {
		excl += "  (" + holderGaveUpNote(info.HolderGaveUp, info.HolderGaveUpWhy, info.Epoch, m.SeatWarmOwed() != "") + ")"
	}
	if info.AsksForHostRAM() {
		excl += "  (" + hostRAMAskedStatus(info.HostRAMWaitGiB, info.HostRAMWaitCommitGiB, info.Age, info.HostRAMHoldUntil, info.HostRAMHoldPhase, time.Now(), true, info.Epoch, info.Yields == gpulease.YieldsWrapper, m.SeatWarmOwed() != "", info.HolderGaveUp, info.HolderGaveUpWhy) + ")"
	}
	if info.Command != "" {
		excl += "\n  running: " + info.Command
	}
	// "held by a text-class lease", never "held by text": the bare class read as a text SEAT holding
	// the card (2026-10-07, a bench's reservation taken for a model). The class labels a reservation.
	fmt.Printf("GPU: held by %s  pid %d  epoch %d  for %s  expires %s%s\n  reason: %s\n  queue behind it: %s\n",
		info.Class.LeasePhrase(), info.PID, info.Epoch, info.Age.Round(time.Second),
		info.ExpiresAt.Format(time.Kitchen), excl, info.Reason, queueHint)
	if len(info.Devices) > 0 {
		fmt.Printf("  cards: %s (a card-scoped lease; other cards are not fenced by it)\n", strings.Join(info.Devices, ", "))
	}
	if len(info.Devices) == 0 && info.ScopeWhy != "" {
		// A legacy whole-node record: what the text seats treat it as, and why.
		if len(info.Inferred) > 0 {
			widened := ""
			if info.ScopeWidened {
				widened = " [scope-widened]"
			}
			fmt.Printf("  seats: fenced on cards %s only (inferred%s); %s\n", strings.Join(info.Inferred, ", "), widened, info.ScopeWhy)
		} else {
			fmt.Printf("  seats: the whole node (%s)\n", info.ScopeWhy)
		}
	}
	for _, ln := range ownershipStatusLines(act.Holder, info) {
		fmt.Println("  " + ln)
	}
	if len(info.Epochs) > 1 {
		fmt.Printf("  live leases: epochs %v (this line shows the lowest; `gpu status --json` lists them all)\n", info.Epochs)
	}
	if len(leases) > 1 || len(info.Devices) > 0 {
		for _, l := range leases {
			scope := "whole node"
			if len(l.Devices) > 0 {
				scope = "cards " + strings.Join(l.Devices, ", ")
			}
			group := ""
			if l.Group != "" {
				group = " group " + l.Group
			}
			declared := ""
			if l.DeclaresHostRAM() {
				declared = fmt.Sprintf(", host RAM %.1f GiB touched, %.1f GiB commit", l.HostRAMGiB, l.HostRAMCommitGiB)
			}
			if !l.AsksForHostRAM() && !l.HolderGaveUp.IsZero() {
				declared += ", " + holderGaveUpNote(l.HolderGaveUp, l.HolderGaveUpWhy, l.Epoch, m.SeatWarmOwed() != "")
			}
			if l.AsksForHostRAM() {
				declared = ", " + hostRAMAskedStatus(l.HostRAMWaitGiB, l.HostRAMWaitCommitGiB, l.Age, l.HostRAMHoldUntil, l.HostRAMHoldPhase, time.Now(), false, l.Epoch, l.Yields == gpulease.YieldsWrapper, m.SeatWarmOwed() != "", l.HolderGaveUp, l.HolderGaveUpWhy)
			}
			fmt.Printf("  lease epoch %d: %s pid %d, %s%s%s\n", l.Epoch, l.Class, l.PID, scope, group, declared)
		}
	}
	printCardTable(cardCfg, cards, cardsNote, cardsErr, leases) // a held host
	fmt.Println(host.Line())
	queueLines()
	printActivity(act)
	return nil
}

// queueHint: one sentence, shared with offload_status (gpulease.QueueHint) so the
// two surfaces a session reads cannot disagree about what to do next.
const queueHint = gpulease.QueueHint

// defaultReserveWait is how long `gpu reserve` queues behind a holder when the caller
// says nothing. Eight hours, not 90 s: the media pipeline's ceiling protects ONE tool
// call from hanging, but a reservation is taken by a session that has a job to run and
// would otherwise write the job off. The whole window is waited out (Options.WaitOut):
// a text holder's declared window is printed, not trusted, because holders release
// before it as a rule.
const defaultReserveWait = 8 * time.Hour

// ramCommitHelp is the usage text of `gpu reserve --ram-commit` (a constant so a test reads it: the flag set exits the
// process on --help).
const ramCommitHelp = "GiB of commit the job will be charged, STATED on the lease beside --ram and admitted against the OS commit limit less gpu_host_ram_headroom_gib (the limit is always enforced on Windows; on Linux with vm.overcommit_memory=2, and also when that mode cannot be read, which counts as strict; in mode 0 or 1 a stated commit is not judged). Commit is memory the OS has promised the job, touched or not, so it can be much more than the working set --ram states (a process tree measured at 27.8 GiB touched was charged 58.1 GiB). State it from a measurement. Unset, no commit is stated: the --ram figure, estimated or given, stands in for it and is admitted against physical RAM less that headroom (or against an enforced commit limit less it, when that sits below physical RAM), as a job that declares one figure always was, because the commit limit usually lies further out than physical RAM and a job that commits more than it touches would be admitted past it. A commit stated equal to --ram is still stated. A job whose commit is smaller than its working set (mapped files count in the working set and not in the commit) cannot say so by leaving the flag out; --ram-commit below --ram is taken as stated. Needs a host-RAM declaration above 0"

func runGPUReserve(args []string) (retErr error) {
	fs := flag.NewFlagSet("gpu reserve", flag.ExitOnError)
	fs.String("config", "", "config file path")
	class := fs.String("class", "text", "lease class: text (a measurement/bench) or media (a generation job)")
	dur := fs.Duration("for", 45*time.Minute, "how long the card is needed; stamped as the lease's declared window")
	reason := fs.String("reason", "", "why the card is held (shown to whoever is waiting)")
	origin := fs.String("origin", "", "who asked for it (a label: session/host); gpu status shows it in the owner line when no session or pid owner is recorded")
	detach := fs.Bool("detach", false, "hold the lease in a hidden background process instead of wrapping a command")
	drain := fs.Bool("drain", false, "after taking the lease, wait until the agent seat is IDLE — no request running or waiting on the engine, no load in progress, no registered agent run — before continuing; the lease is stamped DRAINING meanwhile (new runs wait, in-flight runs complete) and exclusive only once idle; a drain that misses its deadline releases the lease and exits non-zero")
	drainTimeout := fs.Duration("drain-timeout", 0, "how long --drain waits for in-flight work; 0 (the default) = the rest of the --wait queue budget, never under 2m — one 27B step runs 3-4 min, which is why a fixed 2m window failed twice on 2026-09-14")
	unload := fs.Bool("unload-seat", false, "after the drain, unload the agent seat through llama-swap so the cards are free; the wrapper form warms it back when the command ends (detach: use `gpu release --warm-seat`); implies --exclusive")
	wait := fs.Duration("wait", defaultReserveWait, "how long to QUEUE behind a current holder before giving up (0 = fail fast); the holder's declared window is reported, not trusted — the wait runs its full length")
	exclusive := fs.Bool("exclusive", false, "stamp a text lease exclusive: the harness's text-load gate then keeps models off these cards for the lease's length (loads ride a cascade remote lane or wait their own budget); implied by --unload-seat")
	asJSON := fs.Bool("json", false, "emit JSON")
	devicesFlag := fs.String("devices", "", "hold only these cards (nvidia-smi indices or GPU UUID prefixes, comma-separated) instead of the whole node; needs card-scoped leases on this host (config gpu_card_scoped_leases and a green `gpu doctor --write-audit`); the command is pinned to them with CUDA_VISIBLE_DEVICES unless it pins itself")
	cardsFlag := fs.String("cards", "", "hold N cards (or MIN..MAX) chosen by the allocator: not claimed, the display card only within gpu_display_cap_pct of its VRAM (default 60, 0 closes it) and, below a cap of 100, only with --vram declared (at 100 with no display_floor_gib the whole card is open and nothing is held to a footprint), no foreign compute process, VRAM and host RAM that fit; queues when none qualify; the command is pinned to them with CUDA_VISIBLE_DEVICES unless it pins itself")
	wholeNode := fs.Bool("whole-node", false, "hold the whole node (the default when the command names no card)")
	groupFlag := fs.String("group", "", "label for leases taken together for one job (shown in `gpu status`)")
	vramFlag := fs.Float64("vram", 0, "with --cards: GiB of VRAM the job needs free on EACH card (0 = not declared)")
	ramFlag := fs.Float64("ram", 0, "GiB of host RAM the job will TOUCH (its working set), declared on the lease and admitted against the RAM that is available (0 = needs no host RAM). Unset, it is estimated: from the model files of a render helper call whose weights do not fit the card (render/comfy-generate.mjs, comfy-edit, comfy-video, comfy-inpaint, comfy-render), else for a media lease the largest ComfyUI render family this box binds (an sd.cpp binding is not counted: a node that binds only sd.cpp declares 0), and 0 for a text lease. A grant waits, in the same queue, while the available RAM less this and what running leases have yet to touch would fall under gpu_host_ram_headroom_gib, or while the commit charge + the job's commit + what running leases have yet to touch would exceed physical RAM less that headroom (the OS commit limit instead, with what running leases have yet to be charged, when --ram-commit states the commit). With --unload-seat the host is asked AFTER the seat is unloaded, while the cards are held, because the memory it needs may be the seat's own")
	ramCommitFlag := fs.Float64("ram-commit", 0, ramCommitHelp)
	holdMaxFlag := fs.Duration("ram-hold-max", 0, "with --unload-seat: the most the lease may hold its cards waiting for the host to have the RAM it declared, counted from the unload; past it the lease is released (the seat warmed back), the command never runs and the reserve exits non-zero. Unset = config gpu_hostram_hold_max_sec (default 600 s); 0 = no bound, --wait alone ends the wait")
	owner := addOwnershipFlags(fs)
	releaseAtExpiry := fs.Bool("release-at-expiry", false, "with --detach: the hidden holder releases the card at --for, as it did before leases had terms (default: it renews its term while the owner vouches for the job, else labels the lease expired and keeps holding; nothing is ever released by a deadline)")
	dieWithWrapper := fs.Bool("die-with-wrapper", false, "wrapper form: end the command's whole process tree when this wrapper exits or dies (Windows: the job object is created kill-on-close). Off by default, because it makes the wrapper a single point of failure for a long render; without it a dead wrapper's job keeps running and its lease keeps the cards as a tree-orphan until the job ends. Refused where the platform has no handle whose closing ends a tree, and when the command cannot be put in the job. A ComfyUI instance another lease took over is spared and the guarantee stops at it (the job stops killing on close once something is spared)")
	_ = fs.Parse(args)
	if *unload {
		*exclusive = true // a cleared card that the next text call refills is not cleared
	}
	// A DETACHED holder used to exit at --for and release, whether or not the work was
	// still running. With the 45-minute default that silently freed the card mid-job: the
	// next render then claimed it and unloaded the seat on top of the running work, and the
	// node was left advertising a seat that was not there (2026-09-07 audit). It no longer
	// releases at the deadline (it renews its term while its owner vouches for the job, else
	// labels the lease expired and holds on; --release-at-expiry restores the old behaviour),
	// but its window is still the term the lease is judged by, so the requirement stays: the
	// default would read a long job as overdue from the 45th minute. The wrapper form ties the
	// hold to a process and needs no window, so the requirement lands only on --detach.
	forGiven, ramGiven, ramCommitGiven, holdMaxGiven := false, false, false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "for":
			forGiven = true
		case "ram":
			ramGiven = true
		case "ram-commit":
			ramCommitGiven = true
		case "ram-hold-max":
			holdMaxGiven = true
		}
	})
	if math.IsNaN(*ramFlag) || math.IsInf(*ramFlag, 0) {
		return errors.New("--ram must be a finite number of GiB (0 = needs no host RAM): NaN and Inf compare against nothing, so a lease declaring one would never be refused or admitted by the numbers")
	}
	if *ramFlag < 0 {
		return errors.New("--ram must be 0 (needs no host RAM) or a number of GiB")
	}
	if ramCommitGiven && (math.IsNaN(*ramCommitFlag) || math.IsInf(*ramCommitFlag, 0)) {
		return errors.New("--ram-commit must be a finite number of GiB above 0: NaN and Inf compare against nothing, so a lease declaring one would never be refused or admitted by the numbers")
	}
	if ramCommitGiven && *ramCommitFlag <= 0 {
		return errors.New("--ram-commit must be a number of GiB above 0 (unset = the --ram figure); a job that is charged no commit declares no host RAM at all, which is --ram 0")
	}
	if *holdMaxFlag < 0 {
		return errors.New("--ram-hold-max must be 0 (no bound: the wait after the unload runs the whole --wait) or a duration such as 10m")
	}
	if *holdMaxFlag > time.Duration(config.MaxGPUHostRAMHoldMaxSec)*time.Second {
		return fmt.Errorf("--ram-hold-max %s is more than 7 days, longer than any lease or --wait in use, so it is a typo; use 0 for no bound (the wait after the unload then runs the whole --wait)", *holdMaxFlag)
	}
	if *detach && !forGiven {
		return errors.New("--detach requires an explicit --for: the window is the term the lease is judged by, and the 45-minute default would label a long job overdue from its 45th minute. Declare the real window (e.g. --for 8h), or use the wrapper form `gpu reserve ... -- <command>`, which holds the lease exactly as long as the command runs")
	}
	if *releaseAtExpiry && !*detach {
		return errors.New("--release-at-expiry is for --detach: a detached holder releases the card at its --for deadline with it, instead of renewing or labelling the lease expired; the wrapper form holds the lease exactly as long as its command runs, so it has no deadline to release at")
	}
	if *dieWithWrapper && *detach {
		return errors.New("--die-with-wrapper is for the wrapper form: a detached holder runs no command whose tree could end with it")
	}
	if *dieWithWrapper && !proctree.SupportsDieWithWrapper() {
		return errors.New("--die-with-wrapper is not supported on this platform: there is no handle whose closing ends a whole process tree. Without it the command's tree keeps running if this wrapper dies, and its lease keeps the cards as a tree-orphan until the job ends")
	}
	if *unload && !*drain {
		return errors.New("--unload-seat requires --drain: never unload a seat with a request in flight")
	}
	// The bounded-claim contract for an unattended job is checked before anything is
	// acquired: a lease nobody is watching must carry the terms it is judged by.
	if err := owner.validate(forGiven); err != nil {
		return err
	}

	cmdArgs := fs.Args() // everything after `--`
	if len(cmdArgs) == 0 && !*detach {
		return errors.New("give a command to wrap (`gpu reserve ... -- <command>`) or pass --detach")
	}
	if len(cmdArgs) > 0 && *detach {
		return errors.New("--detach and a wrapped command are mutually exclusive")
	}
	if *detach && owner.wantsMintedProgress() {
		return errors.New("--progress-file auto mints a file for the command the wrapper form runs; a --detach hold wraps none and runs several over its life, so name the file the jobs append to (--progress-file <path> --stall <window>)")
	}

	// THE SIGNAL HANDLER COMES FIRST (0.180 final check). It used to be registered after the card was opened, the
	// queue wait and the drain, so a console close during `--wait 8h` or a long drain found no handler and Go ended the
	// process on the spot: its PAIR card stayed open, its waiter record stayed in the line. Every stage below now
	// returns on sw.Done(): the plan (a checkpoint), the queue wait and the card choice (opts.Cancel, sw.Sleep), the
	// drain (its context), the command (the loop's select). The detached form wraps no command and keeps nil.
	var sw *signalWatch
	if !*detach {
		sw = watchWrapperSignals()
		defer sw.Stop()
	}

	m, err := openLease(fs)
	if err != nil {
		return err
	}
	// Exclusive is stamped AT ACQUIRE only when no drain is requested. With
	// --drain (and --unload-seat, which implies exclusive) the record is stamped
	// DRAINING first — new runs wait, requests of runs already in flight keep
	// flowing — and turned exclusive by maintainSeat once the seat is idle. The
	// old order (exclusive at acquire) made the drain block the very run it
	// was waiting for (2026-09-14, register D-93).
	exclusiveAfter := (*exclusive || *unload) && *drain
	opts := gpulease.Options{Reason: *reason, Origin: *origin, TTL: *dur,
		Exclusive: *exclusive && !*drain, Draining: *drain, Command: strings.Join(cmdArgs, " "),
		WrapperVersion: version, Group: strings.TrimSpace(*groupFlag), Cancel: sw.Done()}
	if err := owner.apply(&opts, os.Getenv); err != nil {
		return err
	}
	// This wrapper ticks the yield request below (gpu_yield.go); the record says so, so that no
	// surface reads a holder of an older build as one that would answer. A --detach parent never acquires
	// (its hidden holder stamps its own form), so the value is unused there.
	opts.Yields = gpulease.YieldsWrapper
	if w := progressFileWarning(opts.ProgressFile); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	// A wrapped command that runs a render helper which heartbeats (or that asked for the minted file with
	// --progress-file auto) gets the contract the caller did not declare (F8): without one the lease has
	// nothing to renew on and is labelled expired at the end of its first term while the render is
	// healthy. Only the wrapper form: a detached hold wraps no command to hand a file to.
	autoProgress, autoWhat := "", ""
	if !*detach {
		dir := gpulease.ProgressDir(m.Root())
		asked := owner.wantsMintedProgress()
		if p, what, ok := autoProgressFile(opts, cmdArgs, dir, os.Getpid(), time.Now(), asked); ok {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				// Said, never fatal: the render runs under the lease as it always did, unjudged by a contract.
				fmt.Fprintf(os.Stderr, "gpu reserve: could not create %s for a progress contract (%v); the lease will not renew on progress\n", dir, err)
				opts.Stall = 0 // a window with no file is refused by the lease; --progress-file auto --stall <d> must not turn a full disk into a refusal
			} else {
				gpulease.SweepMintedProgress(dir, mintedProgressMaxAge, time.Now())
				stall := autoProgressStall
				if opts.Stall > 0 { // --progress-file auto --stall <d>: the caller's window
					stall = opts.Stall
				}
				opts.ProgressFile, opts.Stall = p, stall
				autoProgress, autoWhat = p, what
				fmt.Fprintln(os.Stderr, autoProgressSentence(p, what, stall, asked))
			}
		}
	}
	// A window above the cap on a term is accepted whole and never shortened; say so once, here,
	// where it is asked for (the hidden holder of --detach would print into a log nobody reads).
	if w := reserveTermWarning(*dur, opts); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	queuedAt := time.Now()

	// Which cards (plan P3). A host without card-scoped leases takes none of this path:
	// no card table is read and the lease is whole-node, as before.
	devFlags := reserveDeviceFlags{devices: *devicesFlag, cards: *cardsFlag, wholeNode: *wholeNode, vramGiB: *vramFlag, ramGiB: *ramFlag}
	reserveCfg := loadCfg(fs)
	// The most the lease may hold its cards waiting for the host's RAM after the unload (F82 hold bound): the flag when given
	// (0 is a value: no bound), else the config key (unset = 600 s).
	holdMax := reserveCfg.GPUHostRAMHoldMax()
	if holdMaxGiven {
		holdMax = *holdMaxFlag
	} else if sec := reserveCfg.GPUHostRAMHoldMaxSec; sec != nil && *sec > config.MaxGPUHostRAMHoldMaxSec {
		// Load refuses this by name, but it returns at the FIRST validator that fails, so with another key invalid ahead of this one
		// the refusal never comes and the accessor's clamp would be silent. Said once, where the clamped figure is used.
		fmt.Fprintf(os.Stderr, "gpu reserve: warning: config gpu_hostram_hold_max_sec is %d s, above the %d s (7 day) ceiling; using the ceiling\n", *sec, config.MaxGPUHostRAMHoldMaxSec)
	}
	cmdEnv := os.Getenv
	if *detach {
		cmdEnv = func(string) string { return "" } // a detached holder runs no command to read cards from
	}
	var planCards []gpuprobe.Card // the table the plan read, kept so the host-RAM estimate below reuses it
	plan, perr := planReserveDevices(devFlags, cmdArgs, cmdEnv, m.CardScoped(), func() ([]gpuprobe.Card, string, error) {
		cards, note, err := cardTablePatient(context.Background(), reserveCfg)
		if err == nil {
			planCards = cards
		}
		return cards, note, err
	})
	if perr != nil {
		return perr
	}
	if plan.Note != "" {
		fmt.Fprintf(os.Stderr, "gpu reserve: %s\n", plan.Note)
	}
	// A `--cards` request is allocated AND claimed in one loop (acquireAutoCards, below):
	// choosing the cards here and claiming them later lets two simultaneous reserves pick
	// the same one. A detached holder takes the request to the holder process, which runs
	// that loop itself, so its lease is taken by the pid the parent reports.
	opts.Devices = plan.IDs
	if len(plan.IDs) > 0 {
		fmt.Fprintf(os.Stderr, "gpu reserve: holding cards %s (%s); the other cards stay free\n", strings.Join(plan.IDs, ", "), plan.Source)
	}
	// The host RAM this lease declares (internal/hostneed): the operator's --ram, else an estimate
	// from the render helper it wraps, else the class default. Declared on EVERY path (named cards,
	// allocated cards, the whole node, the detached holder), because the grant admits it against the
	// available memory (and its commit against the commit limit if stated, else against physical RAM) wherever the cards came from.
	need := resolveReserveHostRAMWith(ramGiven, *ramFlag, gpulease.Class(*class), cmdArgs, plan.IDs, reserveCfg, m.CardScoped(), planCards, os.Stderr)
	// The commit it is charged, beside the working set: the operator's --ram-commit (stated), else the same figure (not stated).
	declared, cerr := resolveReserveHostLoad(ramCommitGiven, *ramCommitFlag, need, os.Stderr)
	if cerr != nil {
		return cerr
	}
	// F82: a lease that unloads the seat itself is compared to the host AFTER the unload, not in the grant: the memory
	// it would wait for is held by the seat its own unload frees (gpu_hostram_await.go). The grant, the card
	// allocator's host term and the queue's pass rule see a lease that declares nothing; the figure travels as
	// HostRAMWaitGiB and AwaitHostRAM admits it once the seat is cleared. A need no state of the box could ever
	// admit is refused here, before anything is taken or unloaded, as it always was.
	defers := *unload && need.GiB > 0
	if defers && !unloadReachesLease(context.Background(), reserveCfg, plan.IDs, planCards) {
		fmt.Fprintf(os.Stderr, "gpu reserve: --unload-seat reaches no model on the leased cards (%s), so nothing it frees can make room for this lease: its %.1f GiB of host RAM is compared in the grant, as for a lease that unloads nothing\n", strings.Join(plan.IDs, ", "), need.GiB)
		defers = false
	}
	gate, later := hostRAMLoadSplit(defers, declared)
	gate, later, herr := screenDeferredHostRAM(m, gate, later, *wait, os.Stderr)
	if herr != nil {
		return herr
	}
	gateRAM, awaitRAM := gate.TouchGiB, later.TouchGiB
	opts.HostRAMGiB, opts.HostRAMWaitGiB = gateRAM, awaitRAM
	opts.HostRAMCommitGiB, opts.HostRAMWaitCommitGiB = commitDeclared(gate), commitDeclared(later)
	devFlags.ramGiB, devFlags.ramCommitGiB = gateRAM, commitDeclared(gate) // the card allocator's pre-filter reads the same figures
	buildAlloc := func() (gpulease.AllocInput, error) {
		return buildAllocInput(context.Background(), m, reserveCfg, devFlags)
	}

	if *detach {
		var auto *reserveDeviceFlags
		if plan.Auto {
			auto = &devFlags
		}
		epoch, err := detachHolderFn(fs, *class, *dur, *wait, opts, auto, *asJSON, m)
		if err != nil {
			return err
		}
		printForeignGPUWarning(os.Stderr, loadCfg(fs))
		// The lease is held by the hidden child FIRST (so no new work is placed
		// here), then the seat is drained and unloaded. A failed drain leaves the
		// lease held on purpose — the card stays reserved, work keeps routing
		// elsewhere — and the exit code tells the caller not to start. When the
		// lease itself is what was lost (an operator release, a reclaim, or with
		// --release-at-expiry the child letting go at --for), nothing is held and
		// the error says so.
		if *drain || *unload {
			var freed unloadOutcome
			heldSince := time.Now() // the hidden holder took the cards just now
			drainDL := drainDeadline(*drainTimeout, queuedAt, *wait)
			// The outer bound (F82, review rounds 3 and 4): until the wait begins nothing says when the lease asking for the host
			// should end, and a reserve killed during the drain or the unload would leave the holder with no stamp to act on. The
			// drain's own deadline is --wait away (8 h by default), which is no bound at all, so the LIVE reserve keeps a moment
			// of now + the hold bound + a slack on the record, re-stamped every few seconds while the drain and the unload run (the
			// unload is covered by the same ticker). A reserve that dies stops re-stamping and the holder releases the lease within
			// about the bound plus the slack; the wait replaces it with its real end when it begins.
			outerB := startOuterBound(m, epoch, awaitRAM > 0, holdMax, hostRAMDeadline(queuedAt, *wait), os.Stderr)
			merr := detachMaintainReport(m, epoch, loadCfg(fs), *drain, drainDL, *unload, exclusiveAfter, &freed)
			outer := outerB.Stop() // the last moment on the record, for the message if the wait's own stamp fails
			if merr != nil {
				return detachedMaintainErrorAfter(m, epoch, merr, outer)
			}
			// The seat is cleared and the cards are held: now the host is asked (F82). The hidden holder keeps the
			// heartbeat meanwhile. A wait that ends without the host's admission, whether the hold bound or --wait ended it,
			// releases the lease (releaseAfterRAMWait): cards held idle behind a figure the host cannot meet are the incident
			// the bound exists to end, and a --wait shorter than the bound must not bring it back. If this process dies first,
			// the hidden holder releases the lease itself from the stamp the wait left on the record.
			if awaitRAM > 0 {
				await := func(o gpulease.AwaitHostRAMOptions) error { return m.AwaitHostRAM(epoch, o) }
				hold := hostRAMHold{max: holdMax, heldSince: heldSince, freed: freed, classDefault: need.Source == hostneed.SourceClassDefault, failClosed: true, outer: outer}
				if err := awaitHostRAMBounded(context.Background(), await, hostRAMDeadline(queuedAt, *wait), *wait, hold, os.Stderr); err != nil {
					return releaseAfterRAMWait(m, epoch, loadCfg(fs), err)
				}
			}
		}
		return nil
	}

	if wrapperBeforeCard != nil {
		wrapperBeforeCard(sw)
	}
	// A signal that came in while the plan was made (the card table is read, patiently, above) ends the reserve before
	// anything is opened: no card to close, no place in line to leave, nothing held.
	if sig := sw.Signal(); sig != nil {
		return endedBySignal(sig, stageBeforeCard)
	}
	// The job's PAIR card: queued now, in the lease queue and through the
	// drain; running once the command starts; closed with its exit.
	cfg := loadCfg(fs)
	card := newLeaseCard(cfg, cmdArgs, *origin)
	var lease *gpulease.Lease
	if plan.Auto {
		lease, err = acquireAutoCards(m, gpulease.Class(*class), opts, plan, *wait, buildAlloc, os.Stderr, sw.Sleep, time.Now)
		if err == nil {
			// A lease lost during the drain is taken again on the cards this one held.
			opts.Devices = lease.Devices()
			fmt.Fprintf(os.Stderr, "gpu reserve: holding cards %s (%s); the other cards stay free\n", strings.Join(lease.Devices(), ", "), plan.Source)
		}
	} else {
		lease, err = acquireQueued(m, gpulease.Class(*class), opts, *wait)
	}
	if err != nil {
		err = signalEndedWait(sw, err, stageQueued)
		card.finish(err)
		return err
	}
	printForeignGPUWarning(os.Stderr, cfg)
	// Release on the way out no matter how we leave, including Ctrl-C: a leaked text
	// reservation blocks every render until it expires. When the seat was unloaded
	// for this window it is warmed back BEFORE the release, so the first contract
	// placed here again finds a loaded seat — but ONLY while the card is still
	// ours and nobody is queued behind us (register D-124: an unordered warm
	// after a cut command loaded the seat onto the next lease's exclusive card,
	// and a warm ahead of a queued --unload-seat is a load bought for nothing).
	// The warm is heartbeat for its length; the lease outlives the command by
	// exactly the warm. A command ended on SIGTERM is the exception (C02): a console close, a logoff or a
	// shutdown gives the process about five seconds, and a seat reload is minutes, so the lease is released
	// without waiting for the reload (the ComfyUI instances kept under it are still stopped first, a step
	// bounded at 60 s that is usually seconds) and the warm stays owed (the marker is the debt; the next holder to release, `gpu
	// release --warm-seat` or the seat's next request pays it).
	endedOnTerm := false
	endedAt := "" // the stage a signal found the wrapper in when its command never ran ("" = the command ran), for the sentence below
	// Why the final release failed, for the error this command returns (finish sets it).
	var releaseErr error
	// closeTree lets go of the command's containment once it has started (gpu_tree.go). With
	// --die-with-wrapper that ends whatever of the tree is still running; without it, nothing.
	closeTree := func() {}
	finish := func() {
		// A progress file this wrapper minted goes with the command it was minted for, and FIRST: left in place
		// through the seat's warm-back (minutes) it would go stale and `gpu status` would read the lease
		// stalled while it is only loading the seat. Absent it reads unknown, never stalled, and the status
		// line says why (gpuactivity unknownWhy).
		if autoProgress != "" {
			_ = os.Remove(autoProgress)
		}
		// The instances kept under this lease go first: they hold VRAM the seat's warm-back and
		// the next holder both want, and they live no longer than the lease (gpu_instances.go).
		stopInstancesOfLease(cfg, lease, os.Stderr)
		closeTree()
		switch {
		case *unload && endedOnTerm && m.SeatWarmOwed() != "":
			fmt.Fprintln(os.Stderr, warmSkippedOnTerm(endedAt))
		case *unload && endedOnTerm:
			// Ended inside the grace before the seat was unloaded (a signal during the drain): there is no warm to
			// skip and none owed, so nothing is said that is not so.
		case *unload:
			warmBackGuarded(cfg, leaseWarmGuard(m, lease), os.Stderr)
		}
		// A release that fails is said, never dropped: the failure text of a bound or a wait was built before this ran and says
		// only that the lease is being released.
		if rerr := releaseLeaseConfirmed(m, lease); rerr != nil {
			releaseErr = rerr
			fmt.Fprintln(os.Stderr, leaseNotReleasedLine("gpu reserve", lease.Epoch(), rerr, m.SeatWarmOwed() != ""))
		}
	}
	// Registered BEFORE finish, so it runs AFTER it: an error returned while the lease was being released ("the lease is being
	// released") gets the release's outcome as its last word when that release failed.
	defer func() {
		if releaseErr != nil && retErr != nil {
			retErr = releaseFailedError(retErr, lease.Epoch(), releaseErr)
		}
	}()
	defer finish()
	if *drain || *unload {
		heldSince := time.Now() // when the lease took its cards, for the hold bound's failure ("held the cards for")
		for requeues := 0; ; requeues++ {
			// The drain now runs for the queue budget (hours) and the renewal loop
			// below starts only once the wrapped command does; the reclaim rule needs
			// a stale heartbeat AND an expired --for window, so a drain longer than
			// --for with no renewal handed the card to the next acquirer mid-wait
			// (reviewer finding, 0.117.0). Heartbeat for the drain's whole length.
			//
			// A lease that is GONE ends the drain at the next heartbeat (register
			// C-59): waiting out a drain on a card that is no longer ours only moved
			// the failure to the restamp, hours later, and the command never started.
			mctx, cancelMaintain := context.WithCancel(context.Background())
			stopRenew := renewWhile(lease, drainRenewEvery, func(error) { cancelMaintain() })
			// A signal ends the drain at once (the OS gives a console close about five seconds): cancelling its
			// context makes maintainSeat stop; the goroutine ends with the context.
			go func() {
				select {
				case <-sw.Done():
					cancelMaintain()
				case <-mctx.Done():
				}
			}()
			var freed unloadOutcome
			merr := maintainSeat(mctx, cfg, maintainOpts{restamp: lease.Restamp, drain: *drain, deadline: drainDeadline(*drainTimeout, queuedAt, *wait), unload: *unload, exclusive: exclusiveAfter, owed: markWarmOwed(m), devices: lease.Devices(), out: &freed})
			// F82: the seat is cleared; the host is asked now, still holding the cards, under the same heartbeat and
			// the same cancel as the drain. A lease lost or a signal received during this wait is handled below like
			// one during the drain. The wait is bounded (hold bound): at the bound the lease is released by the deferred
			// finish, with the seat warmed back as for any release, and the command never starts.
			stage, phase := stageDraining, "the drain"
			if merr == nil && awaitRAM > 0 {
				stage, phase = stageHostRAM, "the wait for host RAM"
				hold := hostRAMHold{max: holdMax, heldSince: heldSince, freed: freed, classDefault: need.Source == hostneed.SourceClassDefault}
				merr = awaitHostRAMBounded(mctx, lease.AwaitHostRAM, hostRAMDeadline(queuedAt, *wait), *wait, hold, os.Stderr)
			}
			stopRenew()
			cancelMaintain()
			if merr == nil {
				break
			}
			var bound *errHostRAMHoldBound
			if errors.As(merr, &bound) && sw.Signal() == nil {
				// The hold bound passed: never a lease to take again. The lease is still ours (the wait only ends on
				// its own bound), the deferred finish stops the kept instances, warms the seat back and releases it,
				// and the reserve ends here with the bound's failure: nothing runs and nothing re-queues.
				card.finish(merr)
				return merr
			}
			if sig := sw.Signal(); sig != nil {
				// Decided before the deferred finish runs: it chooses between the warm-back and its skip by this.
				endedOnTerm = endsGraceBounded(sig)
				endedAt = stage
				err := endedBySignal(sig, stage)
				card.finish(err)
				return err
			}
			lost := lease.Check()
			if lost != nil {
				// Not a verdict until it is seen on every look: a record that cannot be read, or that was being replaced, does not
				// show the lease lost, and a drain that released and queued again on it would give up a lease that is ours.
				switch state, last := confirmFence(lease, lost, wrapperFenceWindow, wrapperFencePoll, nil); state {
				case fenceLost:
					lost = last
				case fenceUnreadable:
					fmt.Fprintf(os.Stderr, "gpu reserve: the lease record could not be read for %s (%v); the lease can no longer be shown to be ours, so the reserve ends here and does not start over (the release below is tried; a record that stays unreadable is handed on once this process's heartbeat goes stale)\n", wrapperFenceWindow.Round(time.Second), last)
					lost = nil
				default:
					lost = nil
				}
			}
			if lost == nil {
				card.finish(merr)
				return merr // a seat fault with the lease still ours: the deferred finish releases it (and warms back if it got that far)
			}
			// The lease is not ours any more: its record is gone (an operator
			// release, a reclaim) or another holder took the card. The restamp comes
			// before the unload, so a reserve that lost its lease during the drain
			// never reached the unload. Say so, take its place in the line again
			// inside the queue budget it already had, and start the drain over under
			// the new lease; never exit at the restamp with the command not started.
			if requeues >= maxLeaseRequeues {
				err := fmt.Errorf("the lease was lost %d times while draining, giving up (last: %v): %w", requeues+1, lost, merr)
				card.finish(err)
				return err
			}
			fmt.Fprintf(os.Stderr, "gpu reserve: the lease was lost during %s (%v); queueing for the card again, the drain starts over under the new lease\n", phase, lost)
			_ = lease.Release() // gone or another holder's: Release only ever drops its own epoch
			next, aerr := acquireQueued(m, gpulease.Class(*class), opts, requeueWait(queuedAt, *wait))
			if aerr != nil {
				if sig := sw.Signal(); sig != nil && errors.Is(aerr, gpulease.ErrCancelled) {
					// The wait ended on the signal (opts carries its cancel): say so, like the first wait. The old lease is
					// released and the seat was never unloaded under the new one, so finish() has no warm to skip.
					endedOnTerm = endsGraceBounded(sig)
					endedAt = stageQueued
					err := endedBySignal(sig, stageQueued)
					card.finish(err)
					return err
				}
				err := fmt.Errorf("the lease was lost during the drain and the card could not be taken again: %w", aerr)
				card.finish(err)
				return err
			}
			lease = next
			heldSince = time.Now()
		}
	}
	if wrapperBeforeStart != nil {
		wrapperBeforeStart(sw)
	}
	if sig := sw.Signal(); sig != nil {
		endedOnTerm = endsGraceBounded(sig)
		endedAt = stageBeforeStart
		err := endedBySignal(sig, stageBeforeStart)
		card.finish(err)
		return err
	}

	// The command the operator names is theirs: at their terminal it keeps the console it inherits (stdin,
	// stdout and stderr are the operator's own) and may be interactive or open a window they want. Started
	// by a process with no console (a service, a hook, a hidden launcher) nobody is there to see a window, so
	// it is hidden like any other child (winexec.CommandIfNoConsole).
	cmd := winexec.CommandIfNoConsole(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The child inherits the lease so any render it triggers skips its own acquire and
	// its own unload — that is what hoists freeLlamaSwap from per-job to per-lease.
	cmd.Env = append(os.Environ(),
		"GPU_LEASE_DIR="+lease.Dir(),
		fmt.Sprintf("GPU_LEASE_EPOCH=%d", lease.Epoch()),
		// The class travels with the lease so an inheriting render knows what kind of
		// hold it is running under; the unload is elected per lease inside the render
		// path (claimLeaseUnload), so exactly one job per lease tears the tier down.
		"GPU_LEASE_CLASS="+string(lease.Class()),
		// The cards the lease holds (lease ids, comma-separated; empty = the whole node).
		"GPU_LEASE_DEVICES="+strings.Join(lease.Devices(), ","),
		// Where the holder's yield request will appear if it is asked; the file exists only then, so a
		// job can poll it between its items.
		gpulease.YieldFileEnv+"="+gpulease.YieldFilePath(lease.Dir(), lease.Epoch()),
	)
	// The models the render lane's unload may take under a card lease (plan P5): the roster minus
	// the memory stack minus the seats pinned to cards this lease does not hold. A whole-node
	// lease hands over nothing, and the lane keeps its own rule.
	if e := wrapperUnloadEnv(context.Background(), cfg, lease.Devices()); e != "" {
		cmd.Env = append(cmd.Env, e)
	}
	// A lease holds cards; it does not confine the command. A reservation that named or
	// allocated its cards pins the command to them unless the command pins itself (see
	// confineWrapped); a pin of its own (COMFY_CUDA_DEVICE, COMFY_CARD_UUID) that falls outside them is
	// re-pinned onto the held card and the wrapper says so, and the command always starts (GPU-F9).
	if plan.Explicit {
		// Read through the retry: a command's own pin (a bare index it inherited) resolves to a card only with
		// the table, and without one the wrapper REPLACES a pin it could have confirmed. The UUID of a card
		// the lease holds is rebuilt without the table, so a table that is still unreadable costs nothing else.
		launchCards, _, _ := cardTablePatient(context.Background(), cfg)
		conf := confineWrapped(true, lease.Devices(), cmdArgs, os.Getenv, launchCards)
		cmd.Env = append(cmd.Env, conf.Env...)
		if conf.Note != "" {
			fmt.Fprintf(os.Stderr, "gpu reserve: %s\n", conf.Note)
		}
	}
	if silencesWrapped(cmdArgs) {
		// This lease's card stands for the command: a one-shot harness verb
		// under it reports no card of its own.
		cmd.Env = append(cmd.Env, pairworkloads.UnderLeaseEnv+"=1")
	}
	if opts.ProgressFile != "" {
		// The command is told where the lease's progress file is, whether this wrapper minted it or the
		// caller declared it (render/lease-progress.mjs appends to it).
		cmd.Env = append(cmd.Env, gpuProgressFileEnv+"="+opts.ProgressFile)
	}
	if autoProgress != "" {
		if err := startAutoProgress(autoProgress, autoWhat, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "gpu reserve: could not create the progress file %s (%v); the lease reads its progress as unknown until the helper creates it\n", autoProgress, err)
		}
	}
	// The command starts inside a containment the OS can enumerate and stop as a whole (a job
	// object, a process group), so a lost lease ends what it started and not just the command, and
	// so the tree can be recorded in the lease for a reader that finds this wrapper gone.
	tree, err := proctree.Start(cmd, proctree.Options{DieWithWrapper: *dieWithWrapper})
	if err != nil {
		err = fmt.Errorf("starting wrapped command: %w", err)
		card.finish(err)
		return err
	}
	closeTree = func() { closeWrappedTree(tree, cfg, lease.Epoch(), os.Stderr) }
	if w := tree.Warning(); w != "" {
		fmt.Fprintf(os.Stderr, "gpu reserve: warning: %s; if the lease is lost only the command and what the process table shows below it are stopped\n", w)
	}
	recorder := newTreeRecorder(lease, tree, os.Stderr)
	recorder.record()
	card.running()
	if wrapperCommandStarted != nil {
		wrapperCommandStarted()
	}
	why := "" // what cut the command short, for the card
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Heartbeat while the command runs. The reclaim rule needs BOTH a stale heartbeat
	// and an expired window, so a missed tick inside the declared window is harmless.
	tick := time.NewTicker(wrapperTickEvery)
	defer tick.Stop()
	terms := newTermTicker(lease)
	wrapperYields := newHolderYieldTicker(lease, opts.OnYield, "gpu reserve", os.Stderr)
	sigDone := sw.Done() // set to nil once handled: a closed channel would be ready for ever
	fenceSaid := false   // a failed renewal is said once per episode, not at every tick
	for {
		select {
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				card.finish(fmt.Errorf("exit status %d%s", ee.ExitCode(), why))
				finish()               // os.Exit skips defers: warm back + release explicitly
				os.Exit(ee.ExitCode()) // propagate so shell loops branch correctly
			}
			card.finish(err)
			return err
		case <-sigDone:
			sig := sw.Signal()
			why = signalWhy(sig)
			endedOnTerm = endsGraceBounded(sig)
			sigDone = nil
			stopWrappedTree(tree, cmd, cfg, lease.Epoch(), os.Stderr)
		case <-tick.C:
			// LOSING THE LEASE MUST BE LOUD. Discarding this error left the wrapped
			// benchmark running with no reservation while a render tore its tier down
			// — the failure the wrapper exists to prevent, now invisible. Renew()
			// checks the epoch first, so this fires on an operator `gpu release` or on
			// being fenced out.
			//
			// But a record that cannot be READ is not a verdict: a heartbeat that races an atomic replace of the record (a restamp, the
			// queue's or the yield's writes, another process's stamp) reads the lease as gone for a moment, and a wrapper that kills on
			// that kills a long job. It looks again, waits out a short window, and kills only on a verdict seen on every look.
			if err := lease.Renew(); err != nil {
				switch state, last := confirmFence(lease, err, wrapperFenceWindow, wrapperFencePoll, nil); state {
				case fenceLost:
					fmt.Fprintf(os.Stderr,
						"gpu reserve: LEASE LOST (%v) — the GPU is no longer reserved for this command; killing it\n", last)
					why = " (lease lost)"
					stopWrappedTree(tree, cmd, cfg, lease.Epoch(), os.Stderr)
				case fenceUnreadable:
					// Not a verdict, but not a licence either: this wrapper writes no heartbeat while it cannot read the record, and
					// another reserve is handed the cards once that heartbeat is stale. The window is shorter than the TTL that decides it,
					// so the command is stopped first.
					fmt.Fprintf(os.Stderr,
						"gpu reserve: the lease record cannot be read (%v) and has not been for %s — the lease can no longer be shown to be ours, and the cards may be handed to another reserve once this heartbeat is stale; stopping the command\n", last, wrapperFenceWindow.Round(time.Second))
					why = " (lease record unreadable)"
					stopWrappedTree(tree, cmd, cfg, lease.Epoch(), os.Stderr)
				default:
					if !fenceSaid {
						fenceSaid = true
						fmt.Fprintf(os.Stderr, "gpu reserve: could not renew the lease heartbeat (%v); the lease is still ours, retrying at every tick\n", err)
					}
				}
			} else {
				fenceSaid = false
				// The heartbeat is the proof the lease is still ours; only then is its term asked
				// about. A term that ends is renewed or labelled, never a reason to stop the command.
				terms.tick()
				// An overdue lease with somebody waiting for its cards is asked to yield: the job's own on_yield runs,
				// and nothing is stopped (internal/gpulease/yield.go).
				wrapperYields.Tick()
				// And the tree is recorded for a reader that finds this wrapper gone (only when the
				// set of processes changed: the record rewrite takes the epoch lock).
				recorder.record()
			}
		}
	}
}

// acquireQueued takes the card, QUEUEING behind a current holder for up to wait.
//
// THE DEFECT THIS REPLACES: the CLI called TryAcquire, so a held card was an error,
// and every session that hit it read the error as "the machine is busy, refuse the
// work" — while the pipeline underneath had queued renders behind each other since
// ADR 0018. A reservation is a place in line, and the line is printed ONCE on entry
// (who holds it, why, until when) and once on exit; a poll line per second would be
// a notification loop in the session that wrapped this.
//
// THE SECOND DEFECT (register D-1xx-3, 2026-10-09): the place in line was taken only
// AFTER a bare TryAcquire had failed, so a card freed in the window between a release
// and the front waiter's next poll tick went to whichever fresh reserve was launched
// in it, ahead of everyone registered. A recipe that chains reserves back to back
// (its next `gpu reserve` starts the instant the previous one returns) won that race
// every batch: on the reference 3-card box it took card 2 as epochs 1320, 1321 and
// 1322 while a text waiter registered for 1h26m sat front of queue the whole time,
// and its log never printed "queued behind". A fresh reserve now never probes bare:
// Acquire registers before its first attempt and only the front of the line claims,
// with or without --wait (--wait 0 is one gated attempt). The entry line comes from a
// read-only look at the line, not from a failed probe.
func acquireQueued(m *gpulease.Manager, class gpulease.Class, opts gpulease.Options, wait time.Duration) (*gpulease.Lease, error) {
	if wait < 0 {
		wait = 0
	}
	ahead := queueLine(m, opts)
	if ahead != "" && wait > 0 {
		fmt.Fprintf(os.Stderr, "gpu reserve: queued behind %s — waiting up to %s\n", ahead, wait)
	}
	start := time.Now()
	// WaitOut: the holder's declared window is printed above as information, never
	// treated as a verdict — holders release before it as a rule, and a waiter that
	// left the line on the declaration was the refusal this verb exists to end.
	opts.Wait, opts.WaitOut = wait, true
	// The cards can be free while the HOST is what is short: the available memory less this lease's
	// working set and what the running leases have yet to touch would fall under the headroom, or the
	// commit charge plus its commit would pass the line that commit is judged against less the headroom:
	// the commit limit for a commit it states (where the OS enforces one), with what the running leases
	// have yet to be charged, or physical RAM for one nobody stated, with what they have yet to touch.
	// The request keeps its place in the same line and says so once (never per poll: a line per
	// second is a notification per second in the session that wrapped this).
	waitedRAM := false
	if opts.OnHostRAMWait == nil && wait > 0 {
		opts.OnHostRAMWait = func(e *gpulease.ErrHostRAM) {
			waitedRAM = true
			fmt.Fprintf(os.Stderr, "gpu reserve: %s — waiting up to %s\n", e.Error(), wait)
		}
	}
	lease, err := m.Acquire(class, opts)
	if err != nil {
		return nil, heldHint(err, wait)
	}
	if ahead != "" || waitedRAM {
		fmt.Fprintf(os.Stderr, "gpu reserve: acquired after %s in the queue\n", time.Since(start).Round(time.Second))
	}
	return lease, nil
}

// queueLine describes who is ahead of a request at the moment it arrives — the current
// holder of its cards, else the oldest registered waiter that conflicts with them — or ""
// when the line is empty or the request's device ids do not parse (Acquire then refuses
// them itself, with the reason). Read-only: it never claims, and it serves the one entry
// line only; the order itself is decided inside Acquire.
func queueLine(m *gpulease.Manager, opts gpulease.Options) string {
	devs, err := gpulease.NormalizeDevices(opts.Devices)
	if err != nil {
		return ""
	}
	if info := m.InspectFor(devs); info.Held {
		return m.HeldError(info).Error()
	}
	var oldest *gpulease.Waiter
	for _, w := range m.Waiters() {
		if !gpulease.DevicesConflict(w.Devices, devs) {
			continue
		}
		if oldest == nil || w.SinceMs < oldest.SinceMs {
			w := w
			oldest = &w
		}
	}
	if oldest == nil {
		return ""
	}
	return fmt.Sprintf("pid %d (%s, reason %q), already in line for the card", oldest.PID, oldest.Class, oldest.Reason)
}

// heldHint makes a refusal actionable: the holder's declared window is already in the
// message, so the only missing fact is which flag turns the refusal into a wait.
func heldHint(err error, wait time.Duration) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gpulease.ErrStillQueued) {
		// The card is free but someone registered earlier is ahead in line (register D-1xx-3);
		// the error already says who, and whether this request waited at all. What it cannot
		// say is which flag changes the outcome: none was passed (--wait 0), or the window that
		// was passed ran out with the waiter still in front.
		if wait <= 0 {
			return fmt.Errorf("%w; pass --wait <duration> (default %s) to queue behind it instead of failing", err, defaultReserveWait)
		}
		return fmt.Errorf("%w; pass a longer --wait to keep queueing", err)
	}
	var short *gpulease.ErrHostRAM
	if errors.As(err, &short) {
		// The cards are free; the host's memory is not. An impossible need says what to change in its
		// own text (--ram, the headroom); a shortage that waiting cures says which flag waits.
		if short.Impossible {
			return err
		}
		// A commit shortage on a STATED commit is cured by a smaller one as well. One nobody stated is the --ram figure held to physical
		// RAM, and the refusal's own sentence says what a stated commit is judged against instead.
		commitHint := ""
		if short.CommitShort && short.CommitStated {
			commitHint = "; --ram-commit <GiB> states the commit (lower it if the command is charged less)"
		}
		if wait <= 0 {
			return fmt.Errorf("%w; pass --wait <duration> (default %s) to queue until the host has the room, or --ram <GiB> if the estimate is wrong (0 = needs no host RAM)%s", err, defaultReserveWait, commitHint)
		}
		return fmt.Errorf("%w; the host did not have the room within --wait %s — pass a longer --wait to keep waiting, or --ram <GiB> if the estimate is wrong%s", err, wait, commitHint)
	}
	var held *gpulease.ErrHeld
	if !errors.As(err, &held) {
		return err
	}
	if wait <= 0 {
		return fmt.Errorf("%w; pass --wait <duration> (default %s) to queue behind it instead of failing", err, defaultReserveWait)
	}
	return fmt.Errorf("%w; not free within --wait %s — pass a longer --wait to keep queueing", err, wait)
}

// foreignHolderError is what a fail-fast detached reserve (--wait 0) reports when the lease it
// finds is not its child's: another reservation won the card first. The holder is named as a LEASE
// of a class, the phrase `gpu status` leads with (gpulease.Class.LeasePhrase), not as "text".
func foreignHolderError(info gpulease.Info) error {
	return fmt.Errorf("another holder took the GPU first: %s (pid %d, reason %q)",
		info.Class.LeasePhrase(), info.PID, info.Reason)
}

// detachHolder spawns a HIDDEN child that owns the lease, so the lease's holder pid is
// a real, observable process rather than this short-lived CLI invocation. With a
// positive wait the CHILD queues (gpu hold acquires with the same --wait) and this
// parent waits for it to win the card, so "reserved" is printed only once it is true.
// detachHolderFn is detachHolder; a test substitutes an in-process holder (the real one spawns a
// hidden child process).
var detachHolderFn = detachHolder

func detachHolder(fs *flag.FlagSet, class string, dur, wait time.Duration, opts gpulease.Options, auto *reserveDeviceFlags, asJSON bool, m *gpulease.Manager) (uint64, error) {
	// With a named set the parent can tell at once that a fail-fast request has no chance;
	// an allocated one is the holder's to decide (it may find another free card).
	if auto == nil {
		if info := m.InspectFor(opts.Devices); info.Held && wait <= 0 {
			return 0, heldHint(m.HeldError(info), wait)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cfgPath := ""
	if f := fs.Lookup("config"); f != nil {
		cfgPath = f.Value.String()
	}
	// The child outlives this process, so the owner and the contract travel as flags.
	child := exec.Command(self, detachHoldArgs(fs, class, dur, wait, opts, auto, cfgPath)...)
	hideWindow(child) // no console window may ever appear; one gets closed and the hold dies

	// A hidden child with nil Stderr writes to NUL, so a holder that fails to start
	// produced ZERO diagnostics and the parent could only report "did not take the
	// lease". Capture it.
	errLog := filepath.Join(os.TempDir(), fmt.Sprintf("local-offload-gpu-hold-%d.log", os.Getpid()))
	if f, ferr := os.Create(errLog); ferr == nil {
		child.Stderr = f
		defer func() { _ = f.Close() }()
	}
	if err := child.Start(); err != nil {
		return 0, fmt.Errorf("spawning detached holder: %w", err)
	}
	childPID := child.Process.Pid
	childProc := child.Process
	// If we do not end up reporting success, the child must not survive. Both failure
	// paths below used to return while leaving it running: it would then acquire as soon
	// as the winner released and hold the card for its full --for window, while the
	// operator had been told the reservation FAILED. This file warns about exactly that
	// hazard a few lines up ("a leaked text reservation blocks every render until it
	// expires").
	reported := false
	defer func() {
		if !reported && childProc != nil {
			_ = childProc.Kill()
		}
	}()
	// NO Process.Release() HERE. Releasing invalidates the handle, so the Kill above
	// returns EINVAL and the orphan survives both failure paths — the guard read as
	// present while doing nothing. Holding the handle costs nothing: this CLI
	// invocation returns moments later and exits, at which point the OS drops the
	// handle and the child carries on as an independent process either way.

	// The child is the one that queues; if it gives up (its --wait ran out, or the
	// holder's window outlasts it) it exits non-zero and THAT is the answer, read
	// here rather than inferred from a card that is still someone else's.
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()

	// Wait for the child to actually take the lease before reporting success —
	// otherwise a failed hold reads as a successful reservation. The child's own
	// queue window bounds this; the 10 s is the spawn-and-claim allowance on top.
	deadline := time.Now().Add(10*time.Second + wait)
	queuedBehind := 0
	for time.Now().Before(deadline) {
		select {
		case werr := <-childDone:
			childProc = nil // nothing left to reap
			return 0, detachGaveUpError(childPID, werr, errLog)
		default:
		}
		// With card-scoped leases several are live at once, so "the" holder is not the
		// lowest epoch: ours is the lease whose holder pid is the child, wherever it
		// sits; otherwise the lease that conflicts with the cards we asked for. An
		// allocated request names no cards yet, so a lease on some other card is not
		// the line we are standing in: only the child's own lease, or its exit, counts.
		var info gpulease.Info
		if auto == nil {
			info = m.InspectFor(opts.Devices)
		}
		for _, l := range m.Leases() {
			if l.PID == childPID {
				info = l
				break
			}
		}
		// THE HOLDER MUST BE OURS. Without comparing pids, a parent reported someone
		// else's reservation as its own and printed a release command that would kill
		// the winner's hold. With a queue window a foreign holder is not a racer but
		// the line we are standing in: say so once, keep waiting, and let the child's
		// exit (above) be the verdict when the line does not move in time.
		if info.Held && info.PID != childPID {
			if wait <= 0 {
				return 0, foreignHolderError(info)
			}
			if queuedBehind != info.PID {
				queuedBehind = info.PID
				fmt.Fprintf(os.Stderr, "gpu reserve: queued behind %v — the holder (pid %d) takes the card when it frees; waiting up to %s\n",
					m.HeldError(info), childPID, wait)
			}
		}
		if info.Held && info.PID == childPID {
			fmt.Print(detachedReport(info, m.Dir(), asJSON))
			reported = true // the holder is ours and reported; leave it running
			return info.Epoch, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return 0, fmt.Errorf("detached holder (pid %d) did not take the lease within %s; its output is at %s", childPID, (10*time.Second + wait).Round(time.Second), errLog)
}

// detachGaveUpError is the parent's answer when the hidden holder exited before it took the
// lease: the exit status, the holder's own last words (childReason) and where the rest is.
func detachGaveUpError(childPID int, werr error, errLog string) error {
	return fmt.Errorf("detached holder (pid %d) gave up before taking the lease: %v%s; its full output is at %s", childPID, werr, childReason(errLog), errLog)
}

// childReasonMax bounds the detached holder's last words as the parent repeats them: an error
// line is a sentence or two, and the log is still named for the rest.
const childReasonMax = 600

// childReason is the detached holder's own last words, read from the stderr the parent
// captured for it: the error it exited with (main prints it as "error: ..."). It carries what
// the parent cannot know, chiefly why the line did not move: who was ahead, and the flag a
// foreground reserve would have named (`--wait` on a free card with a waiter in front). Without
// it the parent reported only "gave up ... its output is at <temp log>" and the one sentence the
// operator needed sat in a temp file. It returns "; <words>" so the caller's message reads whole,
// or "" when the log is unreadable or empty.
func childReason(errLog string) string {
	b, err := os.ReadFile(errLog)
	if err != nil {
		return ""
	}
	last := ""
	for _, line := range strings.Split(string(b), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			last = l
		}
	}
	last = strings.TrimSpace(strings.TrimPrefix(last, "error:"))
	if last == "" {
		return ""
	}
	if r := []rune(last); len(r) > childReasonMax {
		last = string(r[:childReasonMax]) + "..."
	}
	return "; " + last
}

// holdArgs is the argv of the detached holder (`gpu hold ...`). A named card set travels as
// --devices; an allocated one (auto != nil) travels as the request itself (--cards, --vram,
// --ram), so the holder allocates and claims in one loop and nothing is decided in the
// parent that another reserve can take first.
func holdArgs(class string, dur, wait time.Duration, opts gpulease.Options, auto *reserveDeviceFlags, cfgPath string) []string {
	args := []string{"gpu", "hold",
		"--class", class, "--for", dur.String(), "--wait", wait.String(), "--reason", opts.Reason, "--origin", opts.Origin}
	if opts.Exclusive {
		args = append(args, "--exclusive")
	}
	if opts.Draining {
		args = append(args, "--draining")
	}
	// The host RAM the parent resolved travels on EVERY path, 0 included: the hold child declares
	// exactly this and never re-resolves (it has no command to read, and "unset" would read as 0). An
	// allocated request carries it in its own flags (the allocator's pre-filter reads that copy, and
	// the verb sets the two equal).
	ram := opts.HostRAMGiB
	if auto != nil {
		ram = auto.ramGiB
	}
	args = append(args, "--ram", strconv.FormatFloat(ram, 'f', -1, 64))
	// The commit travels whenever one was stated, one equal to the touched figure too (the holder reads an absent flag as no commit
	// stated, which is judged against another line).
	commit := opts.HostRAMCommitGiB
	if auto != nil {
		commit = auto.ramCommitGiB
	}
	if commit > 0 {
		args = append(args, "--ram-commit", strconv.FormatFloat(commit, 'f', -1, 64))
	}
	// A lease that unloads the seat declares nothing in the grant (F82): the holder takes its cards with the figure on
	// the record as a wait, and the parent asks the host after the unload (gpu_hostram_await.go).
	if opts.HostRAMWaitGiB > 0 {
		args = append(args, "--ram-wait", strconv.FormatFloat(opts.HostRAMWaitGiB, 'f', -1, 64))
	}
	if opts.HostRAMWaitCommitGiB > 0 {
		args = append(args, "--ram-wait-commit", strconv.FormatFloat(opts.HostRAMWaitCommitGiB, 'f', -1, 64))
	}
	switch {
	case auto != nil:
		args = append(args, "--cards", auto.cards, "--vram", strconv.FormatFloat(auto.vramGiB, 'f', -1, 64))
	case len(opts.Devices) > 0:
		args = append(args, "--devices", strings.Join(opts.Devices, ","))
	}
	if opts.Group != "" {
		args = append(args, "--group", opts.Group)
	}
	if cfgPath != "" {
		args = append(args, "--config", cfgPath)
	}
	return append(args, ownerHoldArgs(opts)...)
}

// runGPUHold is the detached holder itself (internal; spawned by --detach). It holds
// the lease and exits as soon as the lease stops being ours — which is how
// `gpu release` frees it without ever killing a process by pid.
func runGPUHold(args []string) (retErr error) {
	fs := flag.NewFlagSet("gpu hold", flag.ExitOnError)
	fs.String("config", "", "config file path")
	class := fs.String("class", "text", "lease class")
	dur := fs.Duration("for", 45*time.Minute, "declared window")
	reason := fs.String("reason", "", "why")
	origin := fs.String("origin", "", "who")
	wait := fs.Duration("wait", 0, "queue behind a current holder for up to this long (the parent passes its --wait)")
	exclusive := fs.Bool("exclusive", false, "stamp the text lease exclusive (the parent passes its --exclusive)")
	draining := fs.Bool("draining", false, "stamp the text lease draining (the parent passes its --drain; it restamps exclusive when the drain completes)")
	devicesFlag := fs.String("devices", "", "the cards to hold, as lease ids (the parent resolved them; empty = the whole node)")
	cardsFlag := fs.String("cards", "", "hold N cards (or MIN..MAX) chosen by the allocator, allocating and claiming in this process (the parent passes its --cards)")
	vramFlag := fs.Float64("vram", 0, "with --cards: GiB of VRAM the job needs free on EACH card")
	ramFlag := fs.Float64("ram", 0, "GiB of host RAM the lease declares (the parent resolved it: its --ram, the estimate, or the class default)")
	ramCommitFlag := fs.Float64("ram-commit", 0, "GiB of commit the lease states beside --ram (0 = none stated; the parent passes it whenever one was stated)")
	ramWaitCommitFlag := fs.Float64("ram-wait-commit", 0, "GiB of commit the lease will state to the host after the seat is unloaded, beside --ram-wait (0 = none stated)")
	ramWaitFlag := fs.Float64("ram-wait", 0, "GiB of host RAM the lease will ask the host for AFTER the parent has unloaded the agent seat (the parent passes it for --unload-seat): stamped on the record, never compared in the grant")
	groupFlag := fs.String("group", "", "label for leases taken together for one job")
	releaseAtExpiry := fs.Bool("release-at-expiry", false, "release the lease at --for (the pre-terms behaviour; the parent passes its --release-at-expiry)")
	owner := addOwnershipFlags(fs)
	_ = fs.Parse(args)

	// The parent validated these four; this verb is also run by hand, and max(NaN, 0) is NaN: a figure that is not finite compares
	// against nothing, so a lease declaring one would be neither refused nor admitted by the numbers.
	for _, f := range []struct {
		name string
		v    float64
	}{{"--ram", *ramFlag}, {"--ram-commit", *ramCommitFlag}, {"--ram-wait", *ramWaitFlag}, {"--ram-wait-commit", *ramWaitCommitFlag}} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			return fmt.Errorf("gpu hold: %s must be a finite number of GiB: NaN and Inf compare against nothing, so a lease declaring one would never be refused or admitted by the numbers", f.name)
		}
	}
	m, err := openLease(fs)
	if err != nil {
		return err
	}
	// The queue lives HERE, in the process that will hold the card, so the lease is
	// taken by the pid the parent reports and a parent that dies mid-wait leaves
	// nothing behind but a holder that will release at its own deadline.
	var holdDevices []string
	if strings.TrimSpace(*devicesFlag) != "" {
		holdDevices = strings.Split(*devicesFlag, ",")
	}
	holdOpts := gpulease.Options{
		Reason: *reason, Origin: *origin, TTL: *dur, Wait: *wait, WaitOut: true, Exclusive: *exclusive, Draining: *draining,
		WrapperVersion: version, Devices: holdDevices, Group: strings.TrimSpace(*groupFlag),
		HostRAMGiB: max(*ramFlag, 0), HostRAMWaitGiB: max(*ramWaitFlag, 0),
		HostRAMCommitGiB: max(*ramCommitFlag, 0), HostRAMWaitCommitGiB: max(*ramWaitCommitFlag, 0),
		// This process's stderr is what the parent reads back (childReason), so the wait is told there once.
		OnHostRAMWait: func(e *gpulease.ErrHostRAM) { fmt.Fprintf(os.Stderr, "gpu hold: %s\n", e.Error()) },
	}
	// The parent resolved the owner and the contract and passed them as flags: this
	// process's own parent is about to exit and says nothing about who asked.
	if err := owner.apply(&holdOpts, os.Getenv); err != nil {
		return err
	}
	// This holder ticks the yield request in its loop below; one that releases at its deadline does not.
	holdOpts.Yields = detachedYieldsForm(*releaseAtExpiry)
	var lease *gpulease.Lease
	if strings.TrimSpace(*cardsFlag) != "" {
		min, max, perr := parseCardCount(*cardsFlag)
		if perr != nil {
			return perr
		}
		holdCfg := loadCfg(fs)
		holdFlags := reserveDeviceFlags{cards: *cardsFlag, vramGiB: *vramFlag, ramGiB: *ramFlag, ramCommitGiB: *ramCommitFlag}
		lease, err = acquireAutoCards(m, gpulease.Class(*class), holdOpts, devicePlan{Auto: true, Min: min, Max: max, Source: "--cards " + strings.TrimSpace(*cardsFlag)}, *wait,
			func() (gpulease.AllocInput, error) {
				return buildAllocInput(context.Background(), m, holdCfg, holdFlags)
			},
			os.Stderr, time.Sleep, time.Now)
	} else {
		lease, err = m.Acquire(gpulease.Class(*class), holdOpts)
		// This process's stderr is what the parent reads back when the holder gives up
		// (childReason), so the refusal carries the same hint the foreground verb prints.
		err = heldHint(err, *wait)
	}
	if err != nil {
		return err
	}
	defer func() {
		// The instances kept under this lease go with it (gpu_instances.go).
		stopInstancesOfLease(loadCfg(fs), lease, os.Stderr)
		if rerr := releaseHolderLease(m, lease); rerr != nil {
			// The holder is about to exit with its lease still held: nothing polls it any more. Say so where somebody reads (the exit
			// status, the record `gpu status` shows) and not only in the log in the temp directory.
			warmOwed := m.SeatWarmOwed() != ""
			fmt.Fprintln(os.Stderr, leaseNotReleasedLine("gpu hold", lease.Epoch(), rerr, warmOwed))
			if merr := m.MarkHolderGaveUp(lease.Epoch(), rerr.Error()); merr != nil {
				fmt.Fprintf(os.Stderr, "gpu hold: could not mark the lease record that the holder gave up (%v); `gpu status` will go on naming a holder that has gone\n", merr)
			}
			if retErr == nil {
				retErr = fmt.Errorf("gpu hold: the lease (epoch %d) could not be released after %d tries and the holder is giving up: %w; run %s", lease.Epoch(), holdReleaseTries, rerr, releaseCommand(lease.Epoch(), warmOwed))
			}
		}
	}()

	// Poll FAST but renew slowly. These are two different clocks and conflating them
	// was wrong: a single 15s ticker meant `gpu release` left the holder alive for up
	// to 15s afterwards, so `gpu status` could report the card free while a holder
	// process was still sitting there. Renewal only has to beat the heartbeat TTL
	// (120s), while noticing we have been released should feel immediate.
	//
	// A DEADLINE IS A TERM, NOT A RELEASE (plan P9). This loop used to end at --for and release
	// the card with the job still running behind it (2026-09-07). It now ends only when the
	// lease stops being ours: at the end of a term the holder's tick renews it (owner alive and
	// the job progressing or the cards working, or unattended and progressing) or labels it
	// expired and carries on holding, heartbeat included. --release-at-expiry restores the old
	// ending for a caller that wants exactly that.
	deadline := time.Now().Add(*dur)
	lastRenew := time.Now()
	terms := newTermTicker(lease)
	holdYields := newHolderYieldTicker(lease, holdOpts.OnYield, "gpu hold", os.Stderr)
	for {
		if *releaseAtExpiry && !time.Now().Before(deadline) {
			return nil // the old ending: the deferred Release lets the card go
		}
		time.Sleep(holdPollEvery)
		if err := lease.Check(); err != nil {
			if gone, last := holderLeaseGone(lease, err); gone {
				// Released from outside, fenced out, or a record that stays unreadable: the lease is not ours (or cannot be shown to
				// be). The holder says which it saw before it exits, since its stderr is a log nobody tails.
				if gpulease.IsRecordUnreadable(last) {
					fmt.Fprintf(os.Stderr, "gpu hold: letting go of lease epoch %d: its record could not be read for %s (%v), so it can no longer be shown to be ours and its cards may be handed on once this heartbeat is stale\n", lease.Epoch(), holdIORetryFor.Round(time.Second), last)
					return nil
				}
				fmt.Fprintf(os.Stderr, "gpu hold: letting go of lease epoch %d: %v\n", lease.Epoch(), last)
				return nil
			}
		}
		// The reserve that took this lease enforces the host-RAM hold bound while it lives. If it died during its wait (a tool
		// timeout, Ctrl-C, a closed terminal) nothing else would, and this process would renew the lease for ever: the incident of
		// 2026-10-14 with nobody left to end it. So the holder reads its own record: a lease that STILL ASKS for the host's RAM whose
		// stamped end of the wait has passed (by a grace that lets a live reserve release first) is released here.
		//
		// The clock is read BEFORE the record: a stamp read from it can then only be as fresh as the time it is compared with or
		// fresher. Read after, a holder starved of CPU between the two reads combined a
		// stale `until` with a much later `now` and released a lease whose reserve had just pushed the moment out.
		now := holdClock()
		if asked, until, herr := lease.HostRAMHold(); herr == nil && asked > 0 && !until.IsZero() && now.After(until.Add(holdBoundGrace)) {
			releaseHolderAtWaitEnd(loadCfg(fs), m, lease, asked, until)
			return nil // the deferred function stops the instances and releases the lease
		}
		if time.Since(lastRenew) >= holdRenewEvery {
			_ = lease.Renew()
			lastRenew = time.Now()
			if !*releaseAtExpiry {
				terms.tick()
				// Asked to yield, this holder runs the job's on_yield and goes on holding: the job is a separate
				// process, and returning here would free the card under it (the 2026-09-07 incident above). Its
				// driver ends the lease once the job has stopped.
				holdYields.Tick()
			}
		}
	}
}

// The cadences of the two holders that tick. A wrapper renews its heartbeat (and runs the term
// check) every wrapperTickEvery; a detached holder polls for a release every holdPollEvery and
// renews every holdRenewEvery. Variables so a test can watch a deadline pass in its own window.
var (
	wrapperTickEvery = 15 * time.Second
	holdPollEvery    = 1 * time.Second
	holdRenewEvery   = 15 * time.Second
)

// The detached reserve keeps the outer bound it stamped on its lease moving while the drain and the unload run: every
// outerRefreshEvery it re-stamps now + the hold bound + outerSlack. (Variables so a test need not stand in line.)
var (
	outerRefreshEvery = 30 * time.Second
	outerSlack        = 60 * time.Second
)

// outerBound keeps the moment a detached reserve's lease is released at, by its hidden holder, if the reserve dies during the drain or
// the unload: now + the hold bound + outerSlack, re-stamped every outerRefreshEvery until Stop.
type outerBound struct {
	stop chan struct{}
	done chan struct{}
	mu   sync.Mutex
	last time.Time
}

// startOuterBound stamps the outer bound at once and starts refreshing it. With the hold bound off (holdMax 0) the stamp is waitEnd,
// the --wait deadline, which the refresh keeps from falling behind now plus the slack. With on false it does nothing and returns nil (Stop of a
// nil outerBound answers the zero time). A stamp that cannot be written is said once; the refresh goes on trying.
func startOuterBound(m *gpulease.Manager, epoch uint64, on bool, holdMax time.Duration, waitEnd time.Time, warn io.Writer) *outerBound {
	if !on {
		return nil
	}
	o := &outerBound{stop: make(chan struct{}), done: make(chan struct{})}
	said := false
	stamp := func() {
		at := time.Now().Add(holdMax + outerSlack)
		if holdMax <= 0 {
			// No hold bound: --wait alone ends the wait, so it is also the backstop for a reserve that dies here. It is never behind a
			// live reserve: the deadline can pass during a long unload, and a stamp behind now plus the slack is moved ahead of it.
			at = waitEnd
			if floor := time.Now().Add(outerSlack); at.Before(floor) {
				at = floor
			}
		}
		if err := m.StampHostRAMHoldPhase(epoch, at, gpulease.HoldPhaseOuter); err != nil {
			if !said {
				said = true
				fmt.Fprintf(warn, "gpu reserve: warning: could not stamp the outer hold bound on the lease record (%v); if this process dies during the drain the hidden holder has no moment at which to release the lease\n", err)
			}
			return
		}
		o.mu.Lock()
		o.last = at
		o.mu.Unlock()
	}
	stamp()
	go func() {
		defer close(o.done)
		t := time.NewTicker(outerRefreshEvery)
		defer t.Stop()
		for {
			select {
			case <-o.stop:
				return
			case <-t.C:
				stamp()
			}
		}
	}()
	return o
}

// Stop ends the refreshing and returns the last moment this reserve put on the record (the zero time if none landed).
func (o *outerBound) Stop() time.Time {
	if o == nil {
		return time.Time{}
	}
	close(o.stop)
	<-o.done
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last
}

// holdClock is the clock the hidden holder compares a stamped end of the wait against. A variable so a test can hold the holder
// back until the reserve's push-out is on the record, and decide itself when the holder may act: a grace measured in real time
// raced the push-out under CPU starvation.
var holdClock = time.Now

// holdBoundGrace is how long past the stamped end of a host-RAM wait the hidden holder waits before it releases its own
// lease: the reserve that took the lease releases it at that moment itself, and the holder acts only when it has not. It is
// generous on purpose: it delays only a BACKSTOP (the holder acts when the reserve is gone), and a reserve stalled under paging,
// or resumed after a suspend, must not get a parallel warm-back from the holder at the instant it is late with its push-out
// (under CPU starvation the 5 s this used to be raced it). A variable so a test can shorten it.
var holdBoundGrace = 30 * time.Second

// holdConfirmPause is the pause between the looks with which the hidden holder confirms that its lease is really gone before it
// lets go of it. A variable so a test can shorten it.
var holdConfirmPause = 50 * time.Millisecond

// The hidden holder's retries of its own release (a release that fails is tried again, about a second apart, before it gives up),
// and how long it goes on retrying a fence it cannot READ before it lets go. Variables so a test need not stand in line.
var (
	holdReleaseTries = 4
	holdReleasePause = time.Second
	holdIORetryFor   = 30 * time.Second
)

// releaseHolderLease is the hidden holder's release of its own lease, tried up to holdReleaseTries times, holdReleasePause apart:
// a release can fail for a moment (a file held open by a scanner, a sharing violation), and a holder that gives up on the first
// failure leaves the cards held with nothing polling them. The first try is the lease's own release, the others go by epoch (a
// lease that reports success and is still on the record is a failure, and a second Release of the same object would do nothing).
// A lease that is not on the record any more, whatever the answers were, is released. nil on success, else the last failure.
func releaseHolderLease(m *gpulease.Manager, lease *gpulease.Lease) error {
	epoch := lease.Epoch()
	var last error
	for i := 0; i < holdReleaseTries; i++ {
		if i > 0 {
			time.Sleep(holdReleasePause)
		}
		var err error
		if i == 0 {
			err = lease.Release()
		} else {
			_, err = releaseByEpochFn(m, epoch)
		}
		// The record itself decides, not Inspect() (which reads a record it cannot read as absent): off the record AND no claim of
		// the epoch left is released whatever the answers were (a card lease removes its record first and its claims after, so a
		// failure on a claim leaves the record absent and the lease half released); on the record, unreadable, or with a claim
		// left, is not. A lease that moved on is not this caller's to release: that refusal is the one error that clears it.
		if gpulease.IsLeaseMovedOn(err) {
			return nil
		}
		if !leaseStillHeld(m, epoch) && len(m.ClaimsOf(epoch)) == 0 {
			return nil
		}
		// The first real failure is the one worth reporting: a retry by epoch of a lease whose record is already gone answers (false,
		// nil) and must not replace what the lease's own release said.
		switch {
		case err != nil:
			last = err
		case last == nil:
			last = stillHeldError(m, epoch, "the release reported success")
			if last == nil {
				last = fmt.Errorf("the release reported success, but the claim of epoch %d on %s is still there", epoch, strings.Join(m.ClaimsOf(epoch), ", "))
			}
		}
	}
	return last
}

// holderLeaseGone confirms a failed fence before the hidden holder lets go. The holder's exit releases its lease, and its fence
// reads the record: a read that lands while a restamp replaces the record fails for a moment on Windows, and one failed read is
// not a lost lease. Three further looks, holdConfirmPause apart, must all fail before the lease is taken for gone; a lease that
// really is gone (an operator release, a reclaim) costs the holder a fraction of a second more to notice.
func holderLeaseGone(lease *gpulease.Lease, first error) (bool, error) {
	state, last := confirmFence(lease, first, holdIORetryFor, holdPollEvery, func(last error) {
		fmt.Fprintf(os.Stderr, "gpu hold: cannot read the record of lease epoch %d (%v); retrying for up to %s before letting go\n", lease.Epoch(), last, holdIORetryFor.Round(time.Second))
	})
	return state != fenceOK, last
}

// fenceState is what confirmFence made of a fence that failed.
type fenceState int

const (
	// fenceOK: the fence passes again (a read that failed for a moment, or a heartbeat that could not be written while the lease is
	// still ours).
	fenceOK fenceState = iota
	// fenceLost: a VERDICT, seen on every look: the lease is gone, or fenced out by another holder.
	fenceLost
	// fenceUnreadable: the record could not be READ on any look in the window. No verdict: the lease may well be ours.
	fenceUnreadable
)

// The window in which a wrapper (its tick, its drain heartbeat, its drain loop) retries a fence it cannot read, and how often it
// looks. A wrapper never kills its command, releases its lease or queues again on a record it cannot read; after the window it says
// so once and goes on, looking again at every tick. Variables so a test need not stand in line.
var (
	wrapperFenceWindow = 10 * time.Second
	wrapperFencePoll   = 250 * time.Millisecond
)

// confirmFence is what EVERY holder does with a fence (Lease.Check, or a Renew that failed) before it acts on it: a record read that
// lands while a restamp replaces the record fails for a moment on Windows and reads as "the lease is gone". It looks again three
// times, holdConfirmPause apart; a verdict (gone, fenced out) on all of them is fenceLost. A record that could not be READ on all of
// them is retried for retryFor, polling every poll, after calling onUnreadable once (nil = say nothing); one that still cannot be
// read is fenceUnreadable, which is not a verdict, and a read that turns into a verdict inside the window is fenceLost. The last error
// seen is returned with the state.
func confirmFence(lease *gpulease.Lease, first error, retryFor, poll time.Duration, onUnreadable func(error)) (fenceState, error) {
	if first == nil {
		return fenceOK, nil
	}
	last := first
	for i := 0; i < 3; i++ {
		time.Sleep(holdConfirmPause)
		err := lease.Check()
		if err == nil {
			return fenceOK, nil
		}
		last = err
	}
	if !gpulease.IsRecordUnreadable(last) {
		return fenceLost, last // a verdict (gone, fenced out): retrying changes nothing
	}
	// A READ that keeps failing (an access error, a sharing violation) passes more often than a verdict does: the lease may well be
	// there. Retry for a while, with one line, before going on without a verdict.
	if onUnreadable != nil {
		onUnreadable(last)
	}
	deadline := time.Now().Add(retryFor)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		err := lease.Check()
		if err == nil {
			return fenceOK, nil
		}
		last = err
		if !gpulease.IsRecordUnreadable(err) {
			return fenceLost, err // it became a verdict
		}
	}
	return fenceUnreadable, last
}

// releaseHolderAtWaitEnd is the hidden holder's release of its own lease when the reserve that took it did not: the host-RAM
// wait it began has passed its stamped end and the lease still asks for the host. It says why on the holder's stderr, warms the
// seat back ONLY if the lease owes it (the wrapper form's guard: a seat nothing unloaded is never loaded by a release), and
// leaves the release itself to the holder's deferred function. The kept instances are stopped HERE as well as there: they hold
// VRAM the seat's warm-back wants, so they go first, and the deferred stop that follows the warm-back finds no marker of a
// stopped instance (StopForLease reads the markers; a stopped instance's is gone), so the second call is a no-op for every
// instance the first one stopped (one it had to leave running is named a second time: noise, not harm).
func releaseHolderAtWaitEnd(cfg config.Config, m *gpulease.Manager, lease *gpulease.Lease, asked float64, until time.Time) {
	fmt.Fprintf(os.Stderr, "gpu hold: the host-RAM wait of this lease (%.1f GiB asked) ended at %s and the lease was not released: the reserve that took it is gone, or it died before it could. Releasing the lease here (the seat is warmed back only if this lease owes it)\n", asked, until.Format(time.Kitchen))
	stopInstancesOfLease(cfg, lease, os.Stderr)
	warmBackGuarded(cfg, leaseWarmGuard(m, lease), os.Stderr)
}

func runGPURelease(args []string) error {
	fs := flag.NewFlagSet("gpu release", flag.ExitOnError)
	fs.String("config", "", "config file path")
	epoch := fs.Uint64("epoch", 0, "release only if this is still the current lease (0 = release whatever is held)")
	warm := fs.Bool("warm-seat", false, "after releasing, load the agent seat back through llama-swap (the counterpart of `gpu reserve --detach --drain --unload-seat`)")
	_ = fs.Parse(args)
	m, err := openLease(fs)
	if err != nil {
		return err
	}
	// WHICH lease is being ended is settled first, by the rule the release itself uses
	// (Manager.ReleaseTarget): a refusal (several card leases held and no --epoch to say which)
	// ends the command here, before anything destructive has run. Stopping the kept instances
	// and warming the seat both used to run ahead of that refusal, so an operator who forgot
	// --epoch killed a live job's ComfyUI and still held the lease.
	target, err := m.ReleaseTarget(*epoch)
	if err != nil {
		return err
	}
	// The instances kept under the lease being ended go with it, and BEFORE the seat is warmed
	// back (both want the VRAM) and before the release (so the next holder never finds one on its
	// card). Only a lease that is live is stopped for: nothing held, nothing to stop.
	stopped := false
	if info := m.Inspect(); target != 0 && info.HoldsEpoch(target) {
		stopKeptInstances(loadCfg(fs), target, os.Stderr)
		stopped = true
	}
	// Warm BEFORE the release so the seat is loaded by the time delegators see
	// the card free again; a failed warm-back is reported and never blocks the
	// release (a leaked lease costs every caller, a cold seat costs one load).
	// The warm runs only when the lease being released is the current one and
	// nobody is queued behind it (register D-124): a successor unloads the seat
	// again, and a warm against someone else's exclusive lease is the incident.
	if *warm {
		warmBackGuarded(loadCfg(fs), releaseWarmGuard(m, *epoch), os.Stderr)
	}
	released, err := releaseByEpochFn(m, *epoch)
	if err == nil && !released {
		// "No lease was held" is believed only if nothing is held now: the answer is the same for a lease that is gone and for one
		// whose record could not be examined, and the verb used to print it and exit 0 over a lease that was there. The record
		// itself decides (a record that cannot be read is held), and for no named target the live view still must be empty.
		if target != 0 {
			if herr := stillHeldError(m, target, "the release reported that no lease was held"); herr != nil {
				err = fmt.Errorf("%w; run it again, or look at `gpu status`", herr)
			}
		} else if info := m.Inspect(); info.Held && *epoch == 0 {
			err = fmt.Errorf("the release reported that no lease was held, but epoch %d is still held; run it again, or look at `gpu status`", info.Epoch)
		}
	}
	if err != nil {
		if stopped {
			// The stop cannot be taken back: say so, so the operator does not read the failure
			// as "nothing happened".
			err = fmt.Errorf("%w (the ComfyUI instances kept under lease epoch %d were already stopped; the lease is still held)", err, target)
		}
		return err
	}
	if !released {
		fmt.Println("no lease is held")
		return nil
	}
	fmt.Println("released")
	if !*warm {
		if seat := m.SeatWarmOwed(); seat != "" {
			fmt.Printf("note: %s is owed a warm-back (unloaded for a lease); `gpu release --warm-seat` or the next request loads it\n", seat)
		}
	}
	return nil
}

// markWarmOwed is the maintainSeat callback that stamps the warm-owed marker
// after an unload.
func markWarmOwed(m *gpulease.Manager) func(seat string) {
	return func(seat string) {
		if err := m.MarkSeatWarmOwed(seat); err != nil {
			fmt.Fprintf(os.Stderr, "gpu reserve: could not record that %s is owed a warm-back: %v\n", seat, err)
		}
	}
}

// leaseWarmGuard is the wrapper form's guard: the lease object proves and
// heartbeats ownership; waiters and the marker come from the manager.
func leaseWarmGuard(m *gpulease.Manager, l *gpulease.Lease) warmGuard {
	return warmGuard{
		held:        l.Check,
		renew:       l.Renew,
		waiters:     m.Waiters,
		owed:        m.SeatWarmOwed,
		clear:       m.ClearSeatWarmOwed,
		clearIfSeat: m.ClearSeatWarmOwedIfSeat,
		onlyIfOwed:  true,
		others:      otherLeaseOnSeat(m, l.Epoch()),
		hostRAM:     hostRAMWarmCheck(m, l.Epoch()),
	}
}

// releaseWarmGuard is `gpu release --warm-seat`'s guard: the caller does not
// hold a Lease object, so ownership is "the record is the epoch I was told to
// release" (epoch 0 = whatever is held, the operator's override — the warm
// then runs when the card is free or held by the record being released).
func releaseWarmGuard(m *gpulease.Manager, epoch uint64) warmGuard {
	held := func() error {
		info := m.Inspect()
		if epoch != 0 && info.Held && !info.HoldsEpoch(epoch) {
			return fmt.Errorf("the lease has moved on (asked to release epoch %d, current is %d)", epoch, info.Epoch)
		}
		return nil
	}
	return warmGuard{
		held: held,
		// No Lease object to heartbeat here (a detached holder's child does
		// that), but the ownership check re-runs on the same cadence for the
		// warm's whole length, so a card that moves on mid-load cancels it.
		renew:       held,
		waiters:     m.Waiters,
		owed:        m.SeatWarmOwed,
		clear:       m.ClearSeatWarmOwed,
		clearIfSeat: m.ClearSeatWarmOwedIfSeat,
		others:      otherLeaseOnSeat(m, epoch),
		hostRAM:     hostRAMWarmCheck(m, epoch),
	}
}

// drainFloor is the least a drain waits, whatever the queue budget says: a
// caller that asked to fail fast on the LEASE (`--wait 0`) still gets a real
// window for in-flight work — the old fixed default, kept as the floor. A
// variable so a test can shorten it.
var drainFloor = 2 * time.Minute

// drainDeadline: an explicit --drain-timeout wins; otherwise the drain runs
// inside the same queue budget as the lease (--wait, measured from when the
// reservation started queueing), never under drainFloor.
func drainDeadline(explicit time.Duration, queuedAt time.Time, wait time.Duration) time.Time {
	if explicit > 0 {
		return time.Now().Add(explicit)
	}
	d := queuedAt.Add(wait)
	if floor := time.Now().Add(drainFloor); d.Before(floor) {
		d = floor
	}
	return d
}

// activityOptions is the activity read the gpu verbs make: this box's lease
// root, its llama-swap and its agent seat, with a utilization sample.
func activityOptions(cfg config.Config) gpuactivity.Options {
	return gpuactivity.Options{LockOverride: cfg.GPULockPath, StateDir: cfg.StateDir, Endpoint: cfg.Endpoint, Seat: cfg.AgentPlannerModel(""), SampleGPU: true, Scope: modelaffinity.ScopeFunc(cfg.GPULockPath, cfg.StateDir), OrphanGrace: cfg.GPUOrphanGrace(), ComfyDir: cfg.ComfyDir,
		// The driver's own fault events for the leased card (C-83-B), read only while a lease is held. The 60 s cache is in
		// process memory, so it serves the long-lived MCP server; every `gpu status` is a fresh process and pays one read.
		DriverEvents: gpuactivity.ReadDriverEvents}
}

// warmOwedIsStale reports whether the owed-warm marker is provably moot: the card is free of
// EVERY lease and the agent seat the marker names is read loaded and settled. The marker means
// "the seat was cleared for a lease and nobody has loaded it back"; a failed warm-back whose
// load went through anyway, or any client's request, loads it, and nothing else ever cleared
// the marker, so the next `--unload-seat` wrapper warmed a seat that was cold when its lease
// began (the 2026-09-23 defect the was-resident check exists for).
//
// The free-card test is info.Held, which is true whenever ANY lease is live: the lease reader
// returns the lowest live epoch's record (Held set for every live record) and a zero Info only
// when nothing is live, and the legacy-scope pass leaves Held alone. Only the fields of that
// summary that describe the lowest lease (epoch, devices) are the lowest lease's; Held is not
// one of them. act.Held is the activity read's own, later, reading of the same fact.
//
// A seat that is starting or stopping has not settled, a seat that could not be read proves
// nothing, and another seat's state says nothing about this one.
func warmOwedIsStale(info gpulease.Info, act gpuactivity.View, owed string) bool {
	if owed == "" || info.Held || act.Held {
		return false
	}
	s := act.Seat
	return s.Err == "" && s.Loaded && !s.Starting && !s.Stopping && strings.EqualFold(owed, s.Name)
}

func printActivity(v gpuactivity.View) {
	for _, ln := range v.Lines() {
		fmt.Println("  " + ln)
	}
}

// drainRenewEvery is the heartbeat cadence while the wrapper form drains — the
// same 15 s the run loop uses, well inside the 120 s heartbeat TTL. A variable
// so a test can watch the heartbeat move within its own window.
var drainRenewEvery = 15 * time.Second

// renewWhile heartbeats the lease on a ticker until stop is called. The loop
// ends for good only when the lease is actually GONE (Check fails): that is
// reported once and handed to onLost, so the holder can stop working on a card
// that is not its own (the wrapper cancels its drain).
//
// A heartbeat write that fails with the record still ours is reported once and
// retried on the next tick. Ending the loop on it left a multi-hour drain
// without a heartbeat until the stale heartbeat and the expired --for window
// let the next acquirer reclaim the lease (register C-59).
//
// stop returns once the loop has exited, so no heartbeat lands after the
// caller has moved on to releasing the lease.
func renewWhile(l *gpulease.Lease, every time.Duration, onLost ...func(error)) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		warned := false
		for {
			select {
			case <-done:
				return
			case <-t.C:
				err := l.Renew()
				if err == nil {
					warned = false
					continue
				}
				// A fence that cannot be READ is not a lost lease: confirm it, and cancel the drain only on a verdict.
				state, cerr := confirmFence(l, err, wrapperFenceWindow, wrapperFencePoll, nil)
				if state == fenceLost {
					fmt.Fprintf(os.Stderr, "gpu reserve: LEASE LOST while draining (%v)\n", cerr)
					for _, fn := range onLost {
						if fn != nil {
							fn(cerr)
						}
					}
					return
				}
				if state == fenceUnreadable {
					// Fail closed: the heartbeat is not written while the record cannot be read, and the window is shorter than the TTL after
					// which another reserve is handed the cards, so the drain ends here like a lost lease, with the cause.
					fmt.Fprintf(os.Stderr, "gpu reserve: the lease record could not be read for %s (%v) while draining; the lease can no longer be shown to be ours, so the drain ends\n", wrapperFenceWindow.Round(time.Second), cerr)
					for _, fn := range onLost {
						if fn != nil {
							fn(cerr)
						}
					}
					return
				}
				if !warned {
					warned = true
					fmt.Fprintf(os.Stderr, "gpu reserve: could not write the lease heartbeat (%v); the lease is still ours, retrying every %s\n", err, every)
				}
			}
		}
	}()
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// maxLeaseRequeues bounds how often one wrapper reserve takes its place in the
// line again after losing its lease mid-drain: the queue budget bounds the
// wait, this bounds the ping-pong with a holder that keeps taking the card.
const maxLeaseRequeues = 5

// requeueWait is the queue budget a reserve that lost its lease mid-drain
// still has: what remains of --wait, measured from when the reservation began
// queueing (the same clock drainDeadline uses), never under drainFloor when a
// wait was asked for. --wait 0 stays fail-fast: one try, then the error.
func requeueWait(queuedAt time.Time, wait time.Duration) time.Duration {
	if wait <= 0 {
		return 0
	}
	left := time.Until(queuedAt.Add(wait))
	if left < drainFloor {
		left = drainFloor
	}
	return left
}

// detachedMaintainError says what became of the detached holder's lease after
// its drain or unload failed. On a seat fault the lease is still held (the card
// stays reserved, work keeps routing elsewhere) and `gpu release` frees it. When
// the record is gone or is another holder's — the hidden holder releases at
// --for whether or not the drain is done, or the lease was taken away —
// nothing of this reservation is held, and telling the caller to `gpu release`
// would release someone else's lease.
func detachedMaintainError(m *gpulease.Manager, epoch uint64, err error) error {
	return detachedMaintainErrorAfter(m, epoch, err, time.Time{})
}

// detachedMaintainErrorAfter is detachedMaintainError for a caller that knows the last outer hold bound it put on the record (the zero
// time if none): a lease that is gone with that moment in the past was, most likely, released by its own hidden holder at it.
func detachedMaintainErrorAfter(m *gpulease.Manager, epoch uint64, err error, outer time.Time) error {
	if st, _ := m.RecordState(epoch); st == gpulease.RecordAbsent && !outer.IsZero() && time.Now().After(outer) {
		// Gone, with the outer bound this reserve last put on the record already past: the hidden holder releases a lease whose
		// bound has passed and which still asks for the host, so that is the likely hand. It happens when this reserve could not keep
		// the bound ahead (its drain or unload stalled it, or the box suspended); the operator is told so instead of left to guess.
		return fmt.Errorf("%w (the lease is no longer this reservation's: the outer hold bound this reserve last set, %s, had passed and the hidden holder releases a lease whose bound has passed, so it most likely released it; reserve again to queue for it)", err, outer.Format(time.Kitchen))
	}
	if st, cause := m.RecordState(epoch); st == gpulease.RecordUnreadable {
		return fmt.Errorf("%w (the lease's record cannot be read, so it is treated as still held: %v; `gpu release --epoch %d` frees it)", err, cause, epoch)
	}
	if info := m.Inspect(); info.Held && info.HoldsEpoch(epoch) {
		// A lease that asks for the host's RAM carries an outer bound from the moment the holder has it: past it the holder releases
		// the lease whatever became of this reserve, and the operator is told so instead of "held on purpose".
		for _, l := range m.Leases() {
			if l.Epoch == epoch && l.HostRAMWaitGiB > 0 && !l.HostRAMHoldUntil.IsZero() {
				return fmt.Errorf("%w (the lease is still held and its hold bound releases it at %s if nothing does sooner; `gpu release` frees it now)", err, l.HostRAMHoldUntil.Format(time.Kitchen))
			}
		}
		return fmt.Errorf("%w (the lease is still held; `gpu release` frees it)", err)
	}
	return fmt.Errorf("%w (the lease is no longer this reservation's: it was released, or another holder took the card; reserve again to queue for it)", err)
}
