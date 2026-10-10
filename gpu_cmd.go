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
	"os"
	"os/exec"
	"os/signal"
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
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

func runGPU(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: local-offload gpu <status|cards|reserve|release|doctor|owner-flags> [flags]")
	}
	switch args[0] {
	case "status":
		return runGPUStatus(args[1:])
	case "reserve":
		return runGPUReserve(args[1:])
	case "release":
		return runGPURelease(args[1:])
	case "hold":
		return runGPUHold(args[1:])
	case "cards":
		return runGPUCards(args[1:])
	case "doctor":
		return runGPUDoctor(args[1:])
	case "owner-flags":
		return runGPUOwnerFlags(args[1:])
	default:
		return fmt.Errorf("unknown gpu subcommand %q (want status, cards, reserve, release, doctor or owner-flags)", args[0])
	}
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
	cards, cardsNote, cardsErr := cardTable(context.Background(), loadCfg(fs))
	if cardsErr != nil {
		cards, cardsNote = nil, fmt.Sprintf("no card table (%v)", cardsErr)
	}
	leases := modelaffinity.ScopeLeases(m.Dir(), m.Leases())
	// The host's memory and what the live leases declared of it (internal/gpulease/hostram.go): the
	// verdict OK / NEAR / OVER, OVER being committed memory above physical RAM, a box that is paging.
	hostMem, hostOK := gpuprobe.ReadHostMemory()
	host := gpucards.NewHostView(hostMem, hostOK, loadCfg(fs).GPUHostRAMHeadroom(), gpulease.DeclaredHostRAMGiB(leases), m.HostRAMPending())
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
		for k, v := range statusLeaseSection(m, cards, cardsNote, waiters) {
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
		printCardTable(cards, cardsNote, cardsErr, leases)
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
			if l.HostRAMGiB > 0 {
				declared = fmt.Sprintf(", host RAM %.1f GiB", l.HostRAMGiB)
			}
			fmt.Printf("  lease epoch %d: %s pid %d, %s%s%s\n", l.Epoch, l.Class, l.PID, scope, group, declared)
		}
	}
	printCardTable(cards, cardsNote, cardsErr, leases)
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

func runGPUReserve(args []string) error {
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
	cardsFlag := fs.String("cards", "", "hold N cards (or MIN..MAX) chosen by the allocator: not claimed, not the display card, no foreign compute process, VRAM and host RAM that fit; queues when none qualify; the command is pinned to them with CUDA_VISIBLE_DEVICES unless it pins itself")
	wholeNode := fs.Bool("whole-node", false, "hold the whole node (the default when the command names no card)")
	groupFlag := fs.String("group", "", "label for leases taken together for one job (shown in `gpu status`)")
	vramFlag := fs.Float64("vram", 0, "with --cards: GiB of VRAM the job needs free on EACH card (0 = not declared)")
	ramFlag := fs.Float64("ram", 0, "GiB of host RAM the job will load, declared on the lease and admitted against committed memory (0 = needs no host RAM). Unset, it is estimated: from the model files of a render helper call whose weights do not fit the card (render/comfy-generate.mjs, comfy-edit, comfy-video, comfy-inpaint, comfy-render), else for a media lease the largest render family this box binds, and 0 for a text lease. A grant waits, in the same queue, while committed memory + this + what running leases have yet to load would exceed physical RAM less gpu_host_ram_headroom_gib")
	owner := addOwnershipFlags(fs)
	releaseAtExpiry := fs.Bool("release-at-expiry", false, "with --detach: the hidden holder releases the card at --for, as it did before leases had terms (default: it renews its term while the owner vouches for the job, else labels the lease expired and keeps holding; nothing is ever released by a deadline)")
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
	forGiven, ramGiven := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "for":
			forGiven = true
		case "ram":
			ramGiven = true
		}
	})
	if *ramFlag < 0 {
		return errors.New("--ram must be 0 (needs no host RAM) or a number of GiB")
	}
	if *detach && !forGiven {
		return errors.New("--detach requires an explicit --for: the window is the term the lease is judged by, and the 45-minute default would label a long job overdue from its 45th minute. Declare the real window (e.g. --for 8h), or use the wrapper form `gpu reserve ... -- <command>`, which holds the lease exactly as long as the command runs")
	}
	if *releaseAtExpiry && !*detach {
		return errors.New("--release-at-expiry is for --detach: a detached holder releases the card at its --for deadline with it, instead of renewing or labelling the lease expired; the wrapper form holds the lease exactly as long as its command runs, so it has no deadline to release at")
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
		WrapperVersion: version, Group: strings.TrimSpace(*groupFlag)}
	if err := owner.apply(&opts, os.Getenv); err != nil {
		return err
	}
	if w := progressFileWarning(opts.ProgressFile); w != "" {
		fmt.Fprintln(os.Stderr, w)
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
	cmdEnv := os.Getenv
	if *detach {
		cmdEnv = func(string) string { return "" } // a detached holder runs no command to read cards from
	}
	plan, perr := planReserveDevices(devFlags, cmdArgs, cmdEnv, m.CardScoped(), func() ([]gpuprobe.Card, string, error) {
		return cardTablePatient(context.Background(), reserveCfg)
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
	// allocated cards, the whole node, the detached holder), because the grant admits it against
	// committed memory wherever the cards came from.
	need := resolveReserveHostRAM(ramGiven, *ramFlag, gpulease.Class(*class), cmdArgs, plan.IDs, reserveCfg, m.CardScoped(), os.Stderr)
	opts.HostRAMGiB = need.GiB
	devFlags.ramGiB = need.GiB // the card allocator's pre-filter reads the same figure
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
			if err := detachMaintain(m, epoch, loadCfg(fs), *drain, drainDeadline(*drainTimeout, queuedAt, *wait), *unload, exclusiveAfter); err != nil {
				return detachedMaintainError(m, epoch, err)
			}
		}
		return nil
	}

	// The job's PAIR card: queued now, in the lease queue and through the
	// drain; running once the command starts; closed with its exit.
	cfg := loadCfg(fs)
	card := newLeaseCard(cfg, cmdArgs, *origin)
	var lease *gpulease.Lease
	if plan.Auto {
		lease, err = acquireAutoCards(m, gpulease.Class(*class), opts, plan, *wait, buildAlloc, os.Stderr, time.Sleep, time.Now)
		if err == nil {
			// A lease lost during the drain is taken again on the cards this one held.
			opts.Devices = lease.Devices()
			fmt.Fprintf(os.Stderr, "gpu reserve: holding cards %s (%s); the other cards stay free\n", strings.Join(lease.Devices(), ", "), plan.Source)
		}
	} else {
		lease, err = acquireQueued(m, gpulease.Class(*class), opts, *wait)
	}
	if err != nil {
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
	// exactly the warm.
	finish := func() {
		// The instances kept under this lease go first: they hold VRAM the seat's warm-back and
		// the next holder both want, and they live no longer than the lease (gpu_instances.go).
		stopInstancesOfLease(cfg, lease, os.Stderr)
		if *unload {
			warmBackGuarded(cfg, leaseWarmGuard(m, lease), os.Stderr)
		}
		_ = lease.Release()
	}
	defer finish()
	if *drain || *unload {
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
			merr := maintainSeatScoped(mctx, cfg, lease.Restamp, *drain, drainDeadline(*drainTimeout, queuedAt, *wait), *unload, exclusiveAfter, markWarmOwed(m), lease.Devices())
			stopRenew()
			cancelMaintain()
			if merr == nil {
				break
			}
			lost := lease.Check()
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
			fmt.Fprintf(os.Stderr, "gpu reserve: the lease was lost during the drain (%v); queueing for the card again, the drain starts over under the new lease\n", lost)
			_ = lease.Release() // gone or another holder's: Release only ever drops its own epoch
			next, aerr := acquireQueued(m, gpulease.Class(*class), opts, requeueWait(queuedAt, *wait))
			if aerr != nil {
				err := fmt.Errorf("the lease was lost during the drain and the card could not be taken again: %w", aerr)
				card.finish(err)
				return err
			}
			lease = next
		}
	}
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	defer signal.Stop(sigc)

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
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
	)
	// The models the render lane's unload may take under a card lease (plan P5): the roster minus
	// the memory stack minus the seats pinned to cards this lease does not hold. A whole-node
	// lease hands over nothing, and the lane keeps its own rule.
	if e := wrapperUnloadEnv(context.Background(), cfg, lease.Devices()); e != "" {
		cmd.Env = append(cmd.Env, e)
	}
	// A lease holds cards; it does not confine the command. A reservation that named or
	// allocated its cards pins the command to them unless the command pins itself (see
	// confineWrapped), and the wrapper says so when a pin of its own falls outside them.
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
	if err := cmd.Start(); err != nil {
		err = fmt.Errorf("starting wrapped command: %w", err)
		card.finish(err)
		return err
	}
	card.running()
	why := "" // what cut the command short, for the card
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Heartbeat while the command runs. The reclaim rule needs BOTH a stale heartbeat
	// and an expired window, so a missed tick inside the declared window is harmless.
	tick := time.NewTicker(wrapperTickEvery)
	defer tick.Stop()
	terms := newTermTicker(lease)
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
		case <-sigc:
			why = " (interrupted)"
			_ = cmd.Process.Kill()
		case <-tick.C:
			// LOSING THE LEASE MUST BE LOUD. Discarding this error left the wrapped
			// benchmark running with no reservation while a render tore its tier down
			// — the failure the wrapper exists to prevent, now invisible. Renew()
			// checks the epoch first, so this fires on an operator `gpu release` or on
			// being fenced out.
			if err := lease.Renew(); err != nil {
				fmt.Fprintf(os.Stderr,
					"gpu reserve: LEASE LOST (%v) — the GPU is no longer reserved for this command; killing it\n", err)
				why = " (lease lost)"
				_ = cmd.Process.Kill()
			} else {
				// The heartbeat is the proof the lease is still ours; only then is its term asked
				// about. A term that ends is renewed or labelled, never a reason to stop the command.
				terms.tick()
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
	// The cards can be free while the HOST is what is short: committed memory plus this lease's
	// declared RAM plus what the running leases have yet to load would pass physical RAM less the
	// headroom. The request keeps its place in the same line and says so once (never per poll: a line
	// per second is a notification per second in the session that wrapped this).
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
		if wait <= 0 {
			return fmt.Errorf("%w; pass --wait <duration> (default %s) to queue until the host has the room, or --ram <GiB> if the estimate is wrong (0 = needs no host RAM)", err, defaultReserveWait)
		}
		return fmt.Errorf("%w; the host did not have the room within --wait %s — pass a longer --wait to keep waiting, or --ram <GiB> if the estimate is wrong", err, wait)
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
			if asJSON {
				out := map[string]any{
					"held": true, "class": info.Class, "epoch": info.Epoch,
					"pid": info.PID, "expires_at": info.ExpiresAt.Format(time.RFC3339),
				}
				if len(info.Devices) > 0 {
					out["devices"] = info.Devices
				}
				b, _ := json.Marshal(out)
				fmt.Println(string(b))
			} else {
				cardsText := ""
				if len(info.Devices) > 0 {
					cardsText = ", cards " + strings.Join(info.Devices, ", ")
				}
				fmt.Printf("reserved: %s epoch %d (pid %d%s) until %s\n  release with: local-offload gpu release --epoch %d\n",
					info.Class, info.Epoch, info.PID, cardsText, info.ExpiresAt.Format(time.Kitchen), info.Epoch)
			}
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
func runGPUHold(args []string) error {
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
	groupFlag := fs.String("group", "", "label for leases taken together for one job")
	releaseAtExpiry := fs.Bool("release-at-expiry", false, "release the lease at --for (the pre-terms behaviour; the parent passes its --release-at-expiry)")
	owner := addOwnershipFlags(fs)
	_ = fs.Parse(args)

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
		HostRAMGiB: max(*ramFlag, 0),
		// This process's stderr is what the parent reads back (childReason), so the wait is told there once.
		OnHostRAMWait: func(e *gpulease.ErrHostRAM) { fmt.Fprintf(os.Stderr, "gpu hold: %s\n", e.Error()) },
	}
	// The parent resolved the owner and the contract and passed them as flags: this
	// process's own parent is about to exit and says nothing about who asked.
	if err := owner.apply(&holdOpts, os.Getenv); err != nil {
		return err
	}
	var lease *gpulease.Lease
	if strings.TrimSpace(*cardsFlag) != "" {
		min, max, perr := parseCardCount(*cardsFlag)
		if perr != nil {
			return perr
		}
		holdCfg := loadCfg(fs)
		holdFlags := reserveDeviceFlags{cards: *cardsFlag, vramGiB: *vramFlag, ramGiB: *ramFlag}
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
		_ = lease.Release()
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
	for {
		if *releaseAtExpiry && !time.Now().Before(deadline) {
			return nil // the old ending: the deferred Release lets the card go
		}
		time.Sleep(holdPollEvery)
		if err := lease.Check(); err != nil {
			return nil // released or fenced out — exit quietly, the lease is not ours
		}
		if time.Since(lastRenew) >= holdRenewEvery {
			_ = lease.Renew()
			lastRenew = time.Now()
			if !*releaseAtExpiry {
				terms.tick()
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
	released, err := m.ReleaseByEpoch(*epoch)
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
	}
}

// drainFloor is the least a drain waits, whatever the queue budget says: a
// caller that asked to fail fast on the LEASE (`--wait 0`) still gets a real
// window for in-flight work — the old fixed default, kept as the floor.
const drainFloor = 2 * time.Minute

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
	return gpuactivity.Options{LockOverride: cfg.GPULockPath, StateDir: cfg.StateDir, Endpoint: cfg.Endpoint, Seat: cfg.AgentPlannerModel(""), SampleGPU: true, Scope: modelaffinity.ScopeFunc(cfg.GPULockPath, cfg.StateDir), OrphanGrace: cfg.GPUOrphanGrace(), ComfyDir: cfg.ComfyDir}
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
				if cerr := l.Check(); cerr != nil {
					fmt.Fprintf(os.Stderr, "gpu reserve: LEASE LOST while draining (%v)\n", cerr)
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
	if info := m.Inspect(); info.Held && info.HoldsEpoch(epoch) {
		return fmt.Errorf("%w (the lease is still held; `gpu release` frees it)", err)
	}
	return fmt.Errorf("%w (the lease is no longer this reservation's: it was released, or another holder took the card; reserve again to queue for it)", err)
}
