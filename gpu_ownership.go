package main

// Ownership flags for the gpu verbs (plan P8, ADR 0070).
//
//	gpu reserve ... --owner-session ID --owner-pid N [--owner-start-ms MS] [--owner-remote]
//	gpu reserve ... --unattended --for 20h --progress-file /abs/work/log.jsonl --stall 2h \
//	                [--yield-grace 5m] [--on-yield "touch /abs/work/STOP"]
//	gpu owner-flags [--pid N]       prints the owner flags for the calling tree
//
// A lease records WHO asked for it so a status call can tell a job whose owner is at the
// desk from one whose owner is long gone, and an unattended job carries the terms it is
// judged by instead (a window and a progress file that must keep moving).

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// ownershipFlags are the flags `gpu reserve` and its detached child `gpu hold` share, so
// the child can hold the lease for exactly the owner and contract its parent resolved.
type ownershipFlags struct {
	ownerSession *string
	ownerPID     *int
	ownerStartMs *int64
	ownerRemote  *bool
	unattended   *bool
	progressFile *string
	stall        *time.Duration
	yieldGrace   *time.Duration
	onYield      *string
}

func addOwnershipFlags(fs *flag.FlagSet) *ownershipFlags {
	return &ownershipFlags{
		ownerSession: fs.String("owner-session", "", "the session that asked for this lease (default: LOCAL_OFFLOAD_ORIGIN, else CLAUDE_CODE_SESSION_ID); `gpu owner-flags` prints the flags for the calling tree"),
		ownerPID:     fs.Int("owner-pid", 0, "a process whose death means the owner is gone (a launcher that detaches before taking the lease must name itself here)"),
		ownerStartMs: fs.Int64("owner-start-ms", 0, "the start identity of --owner-pid, so a recycled pid reads as gone (`gpu owner-flags` prints it)"),
		ownerRemote:  fs.Bool("owner-remote", false, "the lease is asked for from another host: its owner cannot be probed from here, so it is treated as unattended"),
		unattended:   fs.Bool("unattended", false, "nobody is expected at the desk: the lease is judged by its progress contract and its window, never by whether its owner is still around; requires an explicit --for, --progress-file and --stall"),
		progressFile: fs.String("progress-file", "", "a file the job appends to as it works; the lease reads held-stalled when it stops moving for --stall. A relative path is resolved against this command's working directory and recorded absolute (every reader looks for the same file)"),
		stall:        fs.Duration("stall", 0, "how long --progress-file may go without moving before the lease reads as stalled"),
		yieldGrace:   fs.Duration("yield-grace", 0, "how long the job needs to stop cleanly when asked to (recorded for the takeover command; nothing is ever killed inside it)"),
		onYield:      fs.String("on-yield", "", "the command that makes the job stop at a clean boundary (for example `touch work/STOP`); recorded verbatim, with this command's working directory, for the takeover command"),
	}
}

// apply resolves the owner (flags win; the environment supplies a session only) and
// copies the contract onto the options. A relative --progress-file is resolved against
// THIS process's working directory here, before it is recorded: the lease is read later by
// processes that do not share it (the MCP server, the fleet node, another session's `gpu
// status`), and a path recorded as typed would read "unknown" for all of them.
func (o *ownershipFlags) apply(opts *gpulease.Options, getenv func(string) string) error {
	env := ledger.OriginFromEnv(getenv).Session
	opts.Owner = gpulease.ResolveOwner(*o.ownerSession, *o.ownerPID, *o.ownerStartMs, *o.ownerRemote, env)
	opts.Unattended = *o.unattended
	opts.ProgressFile = ""
	if p := strings.TrimSpace(*o.progressFile); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("--progress-file %q cannot be resolved to an absolute path: %w", p, err)
		}
		opts.ProgressFile = abs
	}
	opts.Stall = *o.stall
	opts.YieldGrace = *o.yieldGrace
	opts.OnYield = strings.TrimSpace(*o.onYield)
	return nil
}

// validate enforces the bounded-claim contract before anything is acquired. forGiven says
// whether --for was passed explicitly: the 45 minute default is not a declared window.
func (o *ownershipFlags) validate(forGiven bool) error {
	file := strings.TrimSpace(*o.progressFile) != ""
	stall := *o.stall > 0
	if *o.unattended {
		var missing []string
		if !forGiven {
			missing = append(missing, "an explicit --for (the window the job declares)")
		}
		if !file {
			missing = append(missing, "--progress-file (the file the job appends to)")
		}
		if !stall {
			missing = append(missing, "--stall (how long that file may stay still)")
		}
		if len(missing) > 0 {
			return fmt.Errorf("--unattended is the bounded-claim contract: nobody is at the desk to notice it going wrong, so the lease must carry the terms it is judged by. Missing: %s", strings.Join(missing, "; "))
		}
	}
	switch {
	case file && !stall:
		return errors.New("--progress-file needs --stall: the window in which the file must move")
	case stall && !file:
		return errors.New("--stall needs --progress-file: the file that must move inside the window")
	}
	// The command is recorded verbatim for the takeover to run, so one that cannot be
	// recorded faithfully is refused here rather than cut short.
	if n := len(strings.TrimSpace(*o.onYield)); n > gpulease.MaxOnYieldBytes {
		return fmt.Errorf("--on-yield is %d bytes; the most a lease records verbatim is %d: put the logic in a script and name the script", n, gpulease.MaxOnYieldBytes)
	}
	return nil
}

// holdChildArgs is the argument list of the hidden `gpu hold` child a --detach reserve
// spawns. The child outlives its parent, so it cannot look at its own parent to find the
// owner: everything the parent resolved (owner, contract) is passed explicitly.
func holdChildArgs(class string, dur, wait time.Duration, opts gpulease.Options, cfgPath string) []string {
	return holdArgs(class, dur, wait, opts, nil, cfgPath)
}

// ownerHoldArgs are the owner and contract flags the hidden holder needs: its own parent
// is about to exit, so who asked and the bounded-claim contract travel as flags.
func ownerHoldArgs(opts gpulease.Options) []string {
	var args []string
	if o := opts.Owner; !o.IsZero() {
		if o.Session != "" {
			args = append(args, "--owner-session", o.Session)
		}
		if o.PID > 0 {
			args = append(args, "--owner-pid", strconv.Itoa(o.PID))
			if o.StartMs != 0 {
				args = append(args, "--owner-start-ms", strconv.FormatInt(o.StartMs, 10))
			}
		}
		if o.Remote {
			args = append(args, "--owner-remote")
		}
	}
	if opts.Unattended {
		args = append(args, "--unattended")
	}
	if opts.ProgressFile != "" {
		args = append(args, "--progress-file", opts.ProgressFile, "--stall", opts.Stall.String())
	}
	if opts.YieldGrace > 0 {
		args = append(args, "--yield-grace", opts.YieldGrace.String())
	}
	if opts.OnYield != "" {
		args = append(args, "--on-yield", opts.OnYield)
	}
	return args
}

// runGPUOwnerFlags prints the owner flags for the calling tree, one line, ready to splice
// into a launcher that detaches before it takes the lease (a process created through WMI
// has no parent to ask, and the session environment does not survive the hop).
func runGPUOwnerFlags(args []string) error {
	fs := flag.NewFlagSet("gpu owner-flags", flag.ExitOnError)
	pid := fs.Int("pid", 0, "the process whose death means the owner is gone (default: this command's parent, the caller's shell or launcher)")
	_ = fs.Parse(args)
	p := *pid
	if p <= 0 {
		p = os.Getppid()
	}
	fmt.Println(ownerFlagsLine(os.Getenv, p, gpulease.ProcessStart))
	return nil
}

// ownerFlagsLine builds the line: the session (when the environment names one and it can
// ride as a single token), the pid and its start identity.
func ownerFlagsLine(getenv func(string) string, pid int, procStart func(int) (int64, bool)) string {
	var parts []string
	if s := ledger.OriginFromEnv(getenv).Session; s != "" {
		if strings.ContainsAny(s, " \t\"'") {
			fmt.Fprintf(os.Stderr, "gpu owner-flags: the session id %q contains whitespace or quotes and cannot be passed as one flag; it is left out (the owner is then the process alone)\n", s)
		} else {
			parts = append(parts, "--owner-session="+s)
		}
	}
	if pid > 0 {
		parts = append(parts, "--owner-pid="+strconv.Itoa(pid))
		if st, ok := procStart(pid); ok && st != 0 {
			parts = append(parts, "--owner-start-ms="+strconv.FormatInt(st, 10))
		}
	}
	return strings.Join(parts, " ")
}

// ownershipStatusLines are the lines `gpu status` prints under a held lease: who owns it
// and whether they are there, whether it is unattended, what its progress file says, and
// (for a lease that has neither an owner nor a progress contract) the activity facts.
//
// They describe the lease the verdict is about (h, the most escalated), every one of them
// from h's own reading, so the owner beside the progress is always the same lease's. info is
// the inspection, used only to say which lease that is when several are live.
func ownershipStatusLines(h *gpuactivity.Holder, info gpulease.Info) []string {
	if h == nil {
		return nil
	}
	var out []string
	if n := len(info.Epochs); n > 1 && h.Epoch != 0 {
		out = append(out, fmt.Sprintf("standing of lease epoch %d (the most escalated of %d live leases; the header above shows the lowest, epoch %d):", h.Epoch, n, info.Epoch))
	}
	who := "unknown"
	switch {
	case h.OwnerState == "remote":
		who = "a remote host"
	case h.OwnerSession != "":
		who = "session " + h.OwnerSession
	case h.OwnerPID > 0:
		who = "process " + strconv.Itoa(h.OwnerPID)
	}
	state := ""
	switch h.OwnerState {
	case "alive":
		state = "alive"
	case "gone":
		d := (time.Duration(h.OrphanedForS) * time.Second).Round(time.Second)
		// The moment the first reader saw the owner gone, which every later reader shares.
		since := ""
		if t, err := time.Parse(time.RFC3339, h.OrphanedSince); err == nil {
			since = ", since " + t.Local().Format(time.Kitchen)
		}
		grace := (time.Duration(h.OrphanGraceS) * time.Second).Round(time.Second)
		switch {
		case h.Orphaned:
			state = fmt.Sprintf("GONE for %s%s: ORPHANED (past the %s grace)", d, since, grace)
		case h.Unattended:
			state = fmt.Sprintf("gone for %s%s (an unattended lease is judged by its progress contract, not by its owner)", d, since)
		default:
			state = fmt.Sprintf("gone for %s%s (inside the %s grace)", d, since, grace)
		}
	case "remote":
		state = "cannot be probed from here: treated as unattended"
	default:
		// UNKNOWN. "No owner recorded" is true only when none is; a recorded owner that could
		// not be tracked (its session was never in the registry, or the registry could not be
		// read) is a different statement, and the note says which.
		switch {
		case h.OwnerNote != "":
			state = h.OwnerNote + ": never orphaned, judged by its declared window only"
		default:
			state = "no owner recorded: never orphaned, judged by its declared window only"
		}
	}
	out = append(out, fmt.Sprintf("owner: %s — %s", who, state))
	if h.OrphanMarkErr != "" {
		out = append(out, "warning: "+h.OrphanMarkErr+"; how long the owner has been gone cannot be tracked from here, so this lease cannot read as orphaned from here")
	}
	if h.Unattended {
		out = append(out, "unattended: judged by its progress contract and its window")
	}
	if p := h.Progress; p != nil {
		line := fmt.Sprintf("progress: %s %s", filepath.Base(p.File), p.State)
		if p.State != "unknown" {
			line = fmt.Sprintf("progress: %s last activity %s ago", filepath.Base(p.File), (time.Duration(p.AgeSec) * time.Second).Round(time.Second))
		} else if why := p.UnknownWhy(); why != "" {
			line += " (" + why + ")"
		}
		if p.Detail != "" {
			line += " (" + p.Detail + ")"
		}
		line += fmt.Sprintf(", stall window %s", (time.Duration(p.StallSec) * time.Second).Round(time.Second))
		if h.Stalled {
			line += " — STALLED"
		}
		out = append(out, line)
	}
	if h.Overdue {
		out = append(out, fmt.Sprintf("window: past its declared end by %s (the holder is still renewing; nothing is reclaimed)", (time.Duration(h.OverdueBySec)*time.Second).Round(time.Second)))
	}
	for _, f := range h.Facts {
		out = append(out, "fact: "+f)
	}
	return out
}

// fleetLeaseStanding is the standing reader a fleet node publishes its lease with: the
// same lease directory every acquirer contends on, the operator's orphan grace, and the
// worst standing across every live lease (a stalled lease never hides behind a healthy
// sibling with a lower epoch).
func fleetLeaseStanding(cfg config.Config) func(gpulease.Info) gpulease.Standing {
	return func(info gpulease.Info) gpulease.Standing {
		dir, err := gpulease.LeaseDir(cfg.GPULockPath, cfg.StateDir)
		if err != nil {
			return gpulease.Standing{}
		}
		return gpulease.ObserverAt(dir).StandingAll(info, cfg.GPUOrphanGrace())
	}
}

// progressFileWarning is the line `gpu reserve` prints when the directory of the progress
// file does not exist: the usual cause is a typo, and a progress file that never appears
// leaves the contract inert (progress unknown) for the whole window. A warning, not a
// refusal: a job may create its own directory. "" when there is nothing to say.
func progressFileWarning(path string) string {
	if path == "" {
		return ""
	}
	parent := filepath.Dir(path)
	_, err := os.Stat(parent)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, os.ErrNotExist):
		return fmt.Sprintf("gpu reserve: warning: the directory of --progress-file (%s) does not exist; until the job creates it the lease reads its progress as unknown. Check the path if that is not what you expect", parent)
	}
	return fmt.Sprintf("gpu reserve: warning: the directory of --progress-file (%s) cannot be read (%v); the lease may read its progress as unknown", parent, err)
}
