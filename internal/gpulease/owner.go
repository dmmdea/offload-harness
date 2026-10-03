package gpulease

// owner.go — who a lease belongs to, whether that owner is still there, and whether the
// lease is making the progress it promised (plan P8, ADR 0070).
//
// THE GAP THIS CLOSES. A lease recorded who HELD it (a wrapper pid and a heartbeat the
// wrapper writes about itself) and nothing about who ASKED for it. A wrapper that is
// alive and heartbeating reads as a healthy holder whether the session that launched the
// job is still at the desk or died 20 hours ago, and a heartbeat the wrapper writes about
// itself proves only that the wrapper is alive. So "running" could not be told from
// "abandoned": the operator's own question, and the incident that raised it, a film render
// whose launching session was long gone, held a card for hours behind a green label.
//
// WHAT IS RECORDED, all of it additive and omitempty so a pre-P8 record parses as an
// UNKNOWN owner:
//
//   - Owner: the session that asked (a session id), a process (pid + start identity) and
//     Remote for a lease asked for from another host. Tracked says whether, at the moment
//     the lease was taken, the session was in the session registry (see below).
//   - Unattended: nobody is expected at the desk. Remote implies it.
//   - Progress: a file the job appends to and the window in which it must move.
//
// WHAT IS DERIVED, never stored as a verdict: owner state (alive, gone, unknown, remote),
// the moment an owner was first seen gone (the orphan marker), whether the progress file
// moved inside its window, whether the declared window has ended. Words like "orphaned"
// are computed from those facts by Standing, the same way for every consumer.
//
// THE SESSION REGISTRY. Whether a Claude Code session is alive cannot be read from its id.
// Each session's MCP server writes <state root>/owners/<session>.<pid>.json at start and
// removes it at exit, with no timer. A session is alive while any registered process for
// its id is alive by pid AND start time, so a resumed or compacted session, which starts a
// new process under the SAME id, does not flap: its old entry dies, the new one lives.
// One file per process, not one per session, because the second server of a resumed
// session and the first server's exit are two writers of one record, and a
// read-modify-write between them loses an entry. Nothing here is verified against a live
// Claude Code build beyond the environment variable the ledger already relies on.
//
// NOTHING HERE KILLS OR RELEASES ANYTHING. Orphaned and overdue are surfaced, never acted
// on: taking a lease from a living holder is a separate, explicit command (P11).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Owner names who asked for a lease. The zero Owner is "unknown".
type Owner struct {
	// Session is the session id (LOCAL_OFFLOAD_ORIGIN or CLAUDE_CODE_SESSION_ID).
	Session string `json:"session,omitempty"`
	// PID and StartMs identify one process by pid and start identity (the same pair a
	// lease holder is judged by), so a recycled pid reads as gone.
	PID     int   `json:"pid,omitempty"`
	StartMs int64 `json:"start_ms,omitempty"`
	// Remote marks a lease asked for from another host. Its owner cannot be probed from
	// here, so it is treated as unattended (its progress contract is what judges it).
	Remote bool `json:"remote,omitempty"`
	// Tracked records that the session was in the registry when the lease was taken. A
	// session that was tracked and now has no live registered process is GONE; one that
	// never was (an MCP server that predates the registry) is UNKNOWN, never orphaned.
	Tracked bool `json:"tracked,omitempty"`
}

// IsZero reports whether o names nobody.
func (o Owner) IsZero() bool { return o == Owner{} }

// Progress is a lease's progress contract: the file the job appends to as it works and
// how long it may go without that file moving before the lease reads as stalled.
type Progress struct {
	File    string `json:"file"`
	StallMs int64  `json:"stall_ms"`
}

// OwnerState is what the registry and the recorded process say about an owner.
type OwnerState string

const (
	OwnerAlive   OwnerState = "alive"
	OwnerGone    OwnerState = "gone"
	OwnerUnknown OwnerState = "unknown"
	OwnerRemote  OwnerState = "remote"
)

// DefaultOrphanGrace is how long an owner may be gone before an attended lease reads as
// orphaned (config gpu_orphan_grace_min). A session that compacts or resumes is briefly
// between processes; the grace is what keeps that from reading as abandonment.
const DefaultOrphanGrace = 15 * time.Minute

// ErrUnattendedContract is returned by an acquisition that asks to be unattended without
// the bounded-claim contract: a declared window and a progress file with a stall window.
// An unattended job has nobody to notice it going wrong, so its lease must carry the
// terms it is judged by; "its owner is gone" is not an escape.
var ErrUnattendedContract = errors.New("gpulease: an unattended lease needs a declared window (--for) and a progress contract (--progress-file and --stall)")

// MaxOnYieldBytes bounds a recorded --on-yield command. The takeover runs the command
// verbatim, so it is stored verbatim or not at all; a command that does not fit is
// refused (put the logic in a script and name the script) rather than cut short.
const MaxOnYieldBytes = 4096

// validateOwnership checks the ownership options an acquisition carries.
func (o Options) validateOwnership() error {
	if (o.ProgressFile == "") != (o.Stall <= 0) {
		return errors.New("gpulease: a progress contract needs both a progress file and a stall window")
	}
	if o.Unattended && (o.TTL <= 0 || o.ProgressFile == "") {
		return ErrUnattendedContract
	}
	if o.ProgressFile != "" && !filepath.IsAbs(o.ProgressFile) {
		// A relative path is read against the READER's working directory, and every reader
		// that is not the job's own shell (the MCP server, the fleet node, another session's
		// `gpu status`) has a different one: it would not find the file and report progress
		// unknown for a job that stalled hours ago. Refuse it rather than record a contract
		// that is silently inert; the CLI resolves a relative flag before it gets here.
		return fmt.Errorf("gpulease: the progress file %q must be an absolute path: a reader in another directory cannot find a relative one", o.ProgressFile)
	}
	if len(o.OnYield) > MaxOnYieldBytes {
		return fmt.Errorf("gpulease: --on-yield is %d bytes; the most a lease records verbatim is %d (put the logic in a script and name the script)", len(o.OnYield), MaxOnYieldBytes)
	}
	return nil
}

// stampOwnership writes the ownership options onto a record.
func (m *Manager) stampOwnership(meta *Meta, opts Options) {
	if !opts.Owner.IsZero() {
		o := opts.Owner
		if o.PID > 0 && o.StartMs == 0 {
			if st, ok := m.procStart(o.PID); ok {
				o.StartMs = st
			}
		}
		if o.Session != "" && !o.Remote && !o.Tracked {
			o.Tracked = m.sessionRegistered(o.Session)
		}
		meta.Owner = &o
	}
	meta.Unattended = opts.Unattended || (meta.Owner != nil && meta.Owner.Remote)
	if opts.ProgressFile != "" {
		meta.Progress = &Progress{File: opts.ProgressFile, StallMs: opts.Stall.Milliseconds()}
	}
	if opts.YieldGrace > 0 {
		meta.YieldGraceMs = opts.YieldGrace.Milliseconds()
	}
	// Verbatim: the takeover runs this command, and one cut short for display is a corrupted
	// command. validateOwnership has already refused one too long to record. The directory
	// goes with it, so a relative command still names the same file when it runs elsewhere.
	if oy := strings.TrimSpace(opts.OnYield); oy != "" {
		meta.OnYield = oy
		if wd, err := os.Getwd(); err == nil {
			meta.OnYieldDir = wd
		}
	}
}

// ---------------------------------------------------------------------------
// Resolving an owner
// ---------------------------------------------------------------------------

// ResolveOwner builds the owner of a lease from what a caller said and what the
// environment says. Flags win: an explicit session or process is the caller naming its
// owner (a launcher that detaches before the lease is taken must, or the owner is lost).
// The environment supplies only a SESSION (envSession, resolved by the caller through the
// ledger's process origin), never a pid: the process a wrapper happens to be run from is
// usually a short-lived shell, and recording it would read every lease as abandoned the
// moment the shell exits. With neither, the owner is unknown.
func ResolveOwner(session string, pid int, startMs int64, remote bool, envSession string) Owner {
	o := Owner{Session: strings.TrimSpace(session), PID: pid, StartMs: startMs, Remote: remote}
	if o.PID <= 0 {
		o.PID, o.StartMs = 0, 0
	}
	if o.Session == "" && !remote {
		o.Session = strings.TrimSpace(envSession)
	}
	return o
}

// ---------------------------------------------------------------------------
// The session registry
// ---------------------------------------------------------------------------

// OwnersDirFor is the session registry beside a lease directory (<state root>/owners for
// <state root>/gpu/lease). Derived from the lease directory, not from the config, so the
// writer (an MCP server) and every reader resolve the same directory the same way, the
// doctrine of LeaseDir.
func OwnersDirFor(leaseDir string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(leaseDir)), "owners")
}

func (m *Manager) ownersDir() string { return OwnersDirFor(m.leaseDir()) }

// registration is one process of one session, the content of an owners/ file.
type registration struct {
	Session string `json:"session"`
	PID     int    `json:"pid"`
	StartMs int64  `json:"start_ms"`
}

// ownerFileStem makes a session id safe to use in a file name: anything outside
// [A-Za-z0-9_-] (a '.', which separates the pid, and every path character included) is
// written as ~XX. Distinct ids stay distinct and none can name a path.
func ownerFileStem(session string) string {
	var b strings.Builder
	for i := 0; i < len(session); i++ {
		c := session[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "~%02x", c)
		}
	}
	return b.String()
}

// RegisterOwner records this process as carrying session, in the registry beside
// leaseDir, and returns the function that removes the record. A second process under the
// same session (a resume, a compaction) simply adds its own file. Entries of processes
// that are no longer alive are swept on the way, so the directory cannot grow without
// bound. An empty session registers nothing.
func RegisterOwner(leaseDir, session string, pid int) (unregister func(), err error) {
	return registerOwner(leaseDir, session, pid, processStart)
}

func registerOwner(leaseDir, session string, pid int, procStart func(int) (int64, bool)) (func(), error) {
	session = strings.TrimSpace(session)
	if session == "" || pid <= 0 {
		return func() {}, nil
	}
	dir := OwnersDirFor(leaseDir)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return func() {}, fmt.Errorf("gpulease: session registry %s: %w", dir, err)
	}
	sweepDeadOwners(dir, procStart)
	start, _ := procStart(pid)
	b, err := json.Marshal(registration{Session: session, PID: pid, StartMs: start})
	if err != nil {
		return func() {}, err
	}
	path := filepath.Join(dir, ownerFileStem(session)+"."+strconv.Itoa(pid)+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return func() {}, fmt.Errorf("gpulease: session registry: %w", err)
	}
	if err := renameReplacing(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if werr := os.WriteFile(path, b, 0o666); werr != nil {
			return func() {}, fmt.Errorf("gpulease: session registry: %w", werr)
		}
	}
	return func() { _ = removeClaim(path) }, nil
}

// readRegistryDir and readRegistryFile are the registry's only filesystem reads, package-level
// so a test can make them fail the way a permission error or a wrong mount does (a
// regular file where the directory belongs reads as "not found" on Windows, so it cannot).
var (
	readRegistryDir  = os.ReadDir
	readRegistryFile = os.ReadFile
)

// readRegistrations lists the registry entries of one session (every entry when session
// is empty). A directory that does not exist is an empty registry (nothing ever
// registered, or everything exited); any OTHER failure to list it or to read an entry (a
// permission error, a wrong mount, a file where the directory belongs) is returned, because
// "the registry could not be read" and "nobody is in it" are different answers and the
// first must never be read as the second.
func readRegistrations(dir, session string) ([]registration, error) {
	entries, err := readRegistryDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	prefix := ""
	if session != "" {
		prefix = ownerFileStem(session) + "."
	}
	var out []registration
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, prefix) {
			continue
		}
		b, rerr := readRegistryFile(filepath.Join(dir, name))
		if rerr != nil {
			// An entry removed between the listing and the read is a process that just exited.
			if !errors.Is(rerr, fs.ErrNotExist) && firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		var r registration
		if json.Unmarshal(b, &r) != nil || r.PID <= 0 {
			continue
		}
		if session != "" && r.Session != session {
			continue // a stem collision cannot name another session's process
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, firstErr
}

// sweepDeadOwners removes the entries of processes that are gone.
func sweepDeadOwners(dir string, procStart func(int) (int64, bool)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		var r registration
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		if !processIs(r.PID, r.StartMs, procStart) {
			_ = removeClaim(filepath.Join(dir, name))
		}
	}
}

// processIs reports whether the process recorded as pid + start is alive and is the SAME
// process: a live pid whose start identity differs is a recycled pid, and reads as gone,
// the rule Reclaimable applies to a lease holder. A start identity that cannot be read is
// not evidence of recycling.
func processIs(pid int, startMs int64, procStart func(int) (int64, bool)) bool {
	if pid <= 0 || !pidAlive(pid) {
		return false
	}
	if startMs != 0 {
		if st, ok := procStart(pid); ok && st != startMs {
			return false
		}
	}
	return true
}

// sessionRegistered reports whether the session has a live registered process. A registry
// that cannot be read says nothing: at stamp time that is "not registered", the
// conservative answer (an untracked owner is never judged gone).
func (m *Manager) sessionRegistered(session string) bool {
	regs, _ := readRegistrations(m.ownersDir(), session)
	for _, r := range regs {
		if processIs(r.PID, r.StartMs, m.procStart) {
			return true
		}
	}
	return false
}

// OwnerState judges an owner. Alive when any live registered process carries the
// session, or the recorded process is alive; gone when the owner could be tracked and
// nothing of it is alive; unknown when it could not be tracked (no entry ever, no pid) or
// the registry could not be read; remote for a lease asked for from another host.
func (m *Manager) OwnerState(o *Owner) OwnerState {
	state, _ := m.ownerStateWhy(o)
	return state
}

// ownerStateWhy is OwnerState plus, for an owner that IS recorded but cannot be told apart,
// the reason in a sentence (empty otherwise): the session was never in the registry, or the
// registry could not be read. A recorded process that is alive is positive evidence and
// stands whatever the registry says; absence of evidence from an unreadable registry is not
// evidence of absence (a resumed session lives under another pid), so it reads unknown.
func (m *Manager) ownerStateWhy(o *Owner) (OwnerState, string) {
	if o == nil || o.IsZero() {
		return OwnerUnknown, ""
	}
	if o.Remote {
		return OwnerRemote, ""
	}
	traceable := o.Tracked || o.PID > 0
	var regErr error
	if o.Session != "" {
		var regs []registration
		regs, regErr = readRegistrations(m.ownersDir(), o.Session)
		if len(regs) > 0 {
			traceable = true
		}
		for _, r := range regs {
			if processIs(r.PID, r.StartMs, m.procStart) {
				return OwnerAlive, ""
			}
		}
	}
	if o.PID > 0 && processIs(o.PID, o.StartMs, m.procStart) {
		return OwnerAlive, ""
	}
	if regErr != nil {
		return OwnerUnknown, "the session registry could not be read (" + regErr.Error() + "), so whether the owner is still there cannot be told"
	}
	if !traceable {
		return OwnerUnknown, "the session was not in the session registry when the lease was taken, so whether it is still there cannot be told"
	}
	return OwnerGone, ""
}

// ---------------------------------------------------------------------------
// The orphan marker
// ---------------------------------------------------------------------------

// orphanMarkPrefix names the sidecar orphan.<epoch>: the moment an owner was first seen
// gone. It is a sidecar like hb.<epoch>, removed with the lease.
const orphanMarkPrefix = "orphan."

type orphanMark struct {
	SinceMs int64 `json:"since_ms"`
	ByPID   int   `json:"by_pid"`
}

func (m *Manager) orphanMarkPath(epoch uint64) string {
	return filepath.Join(m.leaseDir(), orphanMarkPrefix+strconv.FormatUint(epoch, 10))
}

func (m *Manager) readOrphanMark(epoch uint64) (time.Time, bool) {
	b, err := os.ReadFile(m.orphanMarkPath(epoch))
	if err != nil {
		return time.Time{}, false
	}
	var mk orphanMark
	if json.Unmarshal(b, &mk) != nil || mk.SinceMs <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(mk.SinceMs), true
}

// ObserveOwner judges the owner of one live lease and keeps its orphan marker honest.
// The FIRST observer to see the owner gone stamps the marker, under the epoch lock with a
// second look inside it, so concurrent readers write one marker and agree on one moment;
// the marker is what makes "gone for 23 minutes" a fact any later reader shares rather
// than a clock each reader starts for itself. An owner seen alive again clears the marker,
// so a session that came back (a resume that took a minute) starts its grace afresh if it
// goes again. A reader that cannot take the lock reports the owner's state and an unstamped
// moment (now): it never fails a status call (see Standing.OrphanMarkErr for how it says so).
//
// This is one of the writers in the read path, and it writes a sidecar only. WHO calls it
// matters: only the status surfaces (`gpu status`, offload_status, the fleet health) stamp.
// The plain inspectors the text gate polls every blocked second, and the sentences a waiter
// or a refusal reads (ExplainHeld, ErrHeld.Error), use peekOwner and stay read-only.
func (m *Manager) ObserveOwner(info Info) (OwnerState, time.Time) {
	state, _, since, _ := m.observeOwner(info, true)
	return state, since
}

// peekOwner is ObserveOwner without the write: the owner's state and, for a gone owner, the
// moment a status call recorded (the marker), else "first seen now" with nothing written, so
// a lease nobody has looked at yet reads as inside the grace until a status surface sees it.
func (m *Manager) peekOwner(info Info) (OwnerState, time.Time) {
	state, _, since, _ := m.observeOwner(info, false)
	return state, since
}

// observeOwner is the one implementation. stamp says whether this reader may write the
// marker. It returns the owner's state, why an unreadable owner is unknown, the moment the
// owner was first seen gone (zero unless it is gone), and the error, if any, of a marker
// write this call had to make: a write that fails is reported, never swallowed, because a
// reader that cannot record the moment would otherwise report "gone for 0s" on every call
// and never reach orphaned, with no hint why.
func (m *Manager) observeOwner(info Info, stamp bool) (OwnerState, string, time.Time, error) {
	state, note := m.ownerStateWhy(info.Owner)
	if info.Epoch == 0 {
		return state, note, time.Time{}, nil
	}
	since, marked := m.readOrphanMark(info.Epoch)
	switch {
	case state == OwnerGone && marked:
		return state, note, since, nil
	case state == OwnerGone:
		now := m.now()
		if !stamp {
			return state, note, now, nil
		}
		return state, note, now, m.stampOrphanMark(info, &now)
	case state == OwnerAlive && marked && stamp:
		// Only positive evidence of presence clears it: an owner that merely cannot be
		// told (an unreadable registry) must not erase the moment it was seen gone.
		return state, note, time.Time{}, m.clearOrphanMark(info)
	}
	return state, note, time.Time{}, nil
}

// stampOrphanMark records the moment under the epoch lock, adopting a marker another
// observer wrote first (*now is set to it). The lease directory is probed for writability
// BEFORE the lock is taken: an unwritable directory fails the lock's create with
// ErrPermission, which the lock cannot tell from contention, so each such call would spin
// its full wait (about two seconds) for nothing.
func (m *Manager) stampOrphanMark(info Info, now *time.Time) error {
	if err := m.canWrite(); err != nil {
		return fmt.Errorf("orphan marker could not be recorded: the lease directory is not writable from here: %w", err)
	}
	var werr error
	lerr := m.withEpochLock(func() error {
		if s, ok := m.readOrphanMark(info.Epoch); ok {
			*now = s
			return nil
		}
		// A lease that is no longer live must not collect a marker for itself.
		if !m.leaseStillLive(info.Epoch) {
			return nil
		}
		b, _ := json.Marshal(orphanMark{SinceMs: now.UnixMilli(), ByPID: m.pid})
		tmp := m.orphanMarkPath(info.Epoch) + ".tmp"
		if err := os.WriteFile(tmp, b, 0o666); err != nil {
			werr = err
			return nil
		}
		if err := renameReplacing(tmp, m.orphanMarkPath(info.Epoch)); err != nil {
			_ = os.Remove(tmp)
			werr = err
		}
		return nil
	})
	switch {
	case lerr != nil:
		return fmt.Errorf("orphan marker could not be recorded: %w", lerr)
	case werr != nil:
		return fmt.Errorf("orphan marker could not be recorded: %w", werr)
	}
	return nil
}

// clearOrphanMark removes the marker of an owner that is back, looking again inside the
// lock: only a still-present owner clears it.
func (m *Manager) clearOrphanMark(info Info) error {
	if err := m.canWrite(); err != nil {
		return fmt.Errorf("orphan marker could not be cleared: the lease directory is not writable from here: %w", err)
	}
	var rerr error
	lerr := m.withEpochLock(func() error {
		if m.OwnerState(info.Owner) == OwnerAlive {
			rerr = removeClaim(m.orphanMarkPath(info.Epoch))
		}
		return nil
	})
	switch {
	case lerr != nil:
		return fmt.Errorf("orphan marker could not be cleared: %w", lerr)
	case rerr != nil:
		return fmt.Errorf("orphan marker could not be cleared: %w", rerr)
	}
	return nil
}

// canWrite proves the lease directory is writable by this process right now.
func (m *Manager) canWrite() error {
	if m.writeProbe != nil {
		return m.writeProbe(m.leaseDir())
	}
	f, err := os.CreateTemp(m.leaseDir(), ".writeprobe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = removeClaim(name)
	return nil
}

// leaseStillLive reports whether the epoch is still a live lease, so a marker is never
// written for a lease that released between the caller's read and the lock.
func (m *Manager) leaseStillLive(epoch uint64) bool {
	for _, l := range m.reader().live() {
		if l.Meta.Epoch == epoch {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Standing: what a lease is doing, derived from the facts above
// ---------------------------------------------------------------------------

// Progress states.
const (
	ProgressAdvancing = "advancing"
	ProgressStalled   = "stalled"
	ProgressUnknown   = "unknown"
)

// ProgressView is the reading of a lease's progress contract.
type ProgressView struct {
	// Declared: the lease carries a progress contract.
	Declared bool
	File     string
	// State is advancing, stalled or unknown (the file is missing or unreadable). A missing
	// file is UNKNOWN, never stalled: the job may not have written its first line yet, and
	// a status call must not invent a stall.
	State string
	// Age is how long since the last sign of life: the later of the file's modification
	// and the lease's acquisition. Counting from the lease's start at the earliest means a
	// log left over from an earlier run does not make a fresh lease read as stalled, and
	// it is why the surfaces say "last activity", not "last moved".
	Age    time.Duration
	Stall  time.Duration
	Detail string
	// Problem says why State is unknown: "does not exist", "is a directory, not a file" or
	// "cannot be read: <error>" (a permission error and the rest are not "not written yet").
	Problem string
	// PastStall: the file is still unknown (missing or unreadable) after the lease has outlived
	// its stall window. The verdict does not change (unknown is never stalled); the reading
	// says so, because by then a wrong path is as likely as a slow job.
	PastStall bool
}

// Standing is the derived reading of one live lease. Verdict words are chosen from it by
// one function (gpuactivity.Assess), so every consumer reads the same lease the same way.
type Standing struct {
	Epoch        uint64
	Owner        OwnerState
	OwnerSession string
	// OrphanedSince is when the owner was first seen gone (zero when it is not gone).
	OrphanedSince time.Time
	// Orphaned: an ATTENDED lease whose owner has been gone for the grace. An unattended
	// lease, a remote one and an unknown owner are never orphaned: the first two are
	// judged by their progress contract and window, the last by its window alone.
	Orphaned   bool
	Unattended bool
	// Overdue: the declared window has ended and the lease is still held. Informational
	// about the holder, never a release: a wrapper that is alive and heartbeating keeps its
	// claim past its window.
	Overdue   bool
	OverdueBy time.Duration
	Progress  ProgressView
	// Stalled: a progress contract exists and its file did not move inside its window.
	Stalled bool
	// OwnerNote says why an owner that IS recorded cannot be told apart (the session was not
	// in the registry when the lease was taken, or the registry could not be read).
	OwnerNote string
	// OrphanMarkErr is set when the orphan marker could not be recorded or cleared.
	OrphanMarkErr string
}

// ObserverAt returns a Manager bound to an explicit lease directory, for a verdict path
// that has the directory and not a Manager (gpuactivity, the fleet node). It does not
// create or probe anything; its Standing stamps and clears the orphan marker.
func ObserverAt(leaseDir string) *Manager { return managerAt(leaseDir) }

// Standing derives the standing of one live lease. It stamps or clears the orphan marker
// (ObserveOwner), so call it from a status surface (`gpu status`, offload_status, the fleet
// health), never from a poll or a message that must stay read-only (StandingReadOnly).
// grace <= 0 means the installed grace, else DefaultOrphanGrace.
func (m *Manager) Standing(info Info, grace time.Duration) Standing {
	return m.standing(info, grace, true)
}

// StandingReadOnly is Standing for a caller that must not write: it reads the orphan marker
// a status surface recorded and never stamps or clears one, and it never takes the epoch
// lock. A gone owner nobody has recorded yet reads as first seen now, inside its grace.
func (m *Manager) StandingReadOnly(info Info, grace time.Duration) Standing {
	return m.standing(info, grace, false)
}

func (m *Manager) standing(info Info, grace time.Duration, stamp bool) Standing {
	grace = orphanGraceOrDefault(grace)
	now := m.now()
	st := Standing{Epoch: info.Epoch, Unattended: info.Unattended}
	state, note, since, merr := m.observeOwner(info, stamp)
	if merr != nil {
		st.OrphanMarkErr = merr.Error()
	}
	st.OwnerNote = note
	st.Owner = state
	if info.Owner != nil {
		st.OwnerSession = info.Owner.Session
	}
	if state == OwnerRemote {
		st.Unattended = true
	}
	if state == OwnerGone && !since.IsZero() {
		st.OrphanedSince = since
		st.Orphaned = !st.Unattended && now.Sub(since) >= grace
	}
	if end := info.ExpiresAt; !end.IsZero() && end.UnixMilli() > 0 && now.After(end) {
		st.Overdue = true
		st.OverdueBy = now.Sub(end)
	}
	st.Progress = m.progressOf(info, now)
	st.Stalled = st.Progress.State == ProgressStalled
	return st
}

// StandingAll is the standing of everything an Info describes. With card-scoped leases
// several are live at once and Info is the lowest epoch's: a node that published only that
// one would hide a stalled lease behind a healthy sibling. The result carries the flags of
// every lease (orphaned, overdue and stalled are each true if true of ANY of them) and the
// detail of the most escalated one.
func (m *Manager) StandingAll(info Info, grace time.Duration) Standing {
	var out Standing
	rank := func(st Standing) int {
		switch st.Escalation() {
		case "held-stalled":
			return 3
		case "held-orphaned":
			return 2
		case "held-overdue":
			return 1
		}
		return 0
	}
	for i, l := range info.Each() {
		st := m.Standing(l, grace)
		orphaned, overdue, stalled := out.Orphaned || st.Orphaned, out.Overdue || st.Overdue, out.Stalled || st.Stalled
		if i == 0 || rank(st) > rank(out) {
			out = st
		}
		out.Orphaned, out.Overdue, out.Stalled = orphaned, overdue, stalled
	}
	return out
}

// progressOf reads the progress contract against the clock.
func (m *Manager) progressOf(info Info, now time.Time) ProgressView {
	p := info.Progress
	if p == nil || p.File == "" {
		return ProgressView{}
	}
	v := ProgressView{Declared: true, File: p.File, Stall: time.Duration(p.StallMs) * time.Millisecond, State: ProgressUnknown}
	fi, err := statProgress(p.File)
	switch {
	case err == nil && fi.IsDir():
		v.Problem = "is a directory, not a file"
	case err == nil:
		// fall through to the reading below
	case errors.Is(err, fs.ErrNotExist):
		v.Problem = "does not exist"
	default:
		v.Problem = "cannot be read: " + err.Error()
	}
	if v.Problem != "" {
		// Unknown, never stalled: the job may not have written its first line yet. But the
		// lease's age bounds how long the file has been missing, and past the stall window a
		// wrong path is as likely as a slow job, so the reading says so (the verdict is
		// unchanged).
		if !info.AcquiredAt.IsZero() {
			if v.Age = now.Sub(info.AcquiredAt); v.Age < 0 {
				v.Age = 0
			}
		}
		v.PastStall = v.Stall > 0 && v.Age > v.Stall
		return v
	}
	ref := fi.ModTime()
	if !info.AcquiredAt.IsZero() && info.AcquiredAt.After(ref) {
		ref = info.AcquiredAt
	}
	v.Age = now.Sub(ref)
	if v.Age < 0 {
		v.Age = 0
	}
	v.State = ProgressAdvancing
	if v.Stall > 0 && v.Age > v.Stall {
		v.State = ProgressStalled
	}
	v.Detail = progressDetail(p.File)
	return v
}

// progressDetailBytes bounds how much of a progress file is read for its last line.
const progressDetailBytes = 4096

// progressDetail is the last line the job wrote, for a sentence like "clip 4 of 17". A
// JSON line contributes its "detail" string, else "done" of "total"; anything else is the
// raw line, clipped.
func progressDetail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := fi.Size() - progressDetailBytes
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) > 0 {
		return ""
	}
	last := ""
	for _, ln := range strings.Split(strings.ReplaceAll(string(buf), "\r\n", "\n"), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			last = s
		}
	}
	if last == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(last), &obj) == nil {
		if d, ok := obj["detail"].(string); ok && strings.TrimSpace(d) != "" {
			return clipDetail(d)
		}
		done, dok := obj["done"].(float64)
		total, tok := obj["total"].(float64)
		if dok && tok && total > 0 {
			return fmt.Sprintf("%d of %d", int(done), int(total))
		}
	}
	return clipDetail(last)
}

func clipDetail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return s
}

// statProgress is the progress file stat, a seam so a test can make it fail the way a
// permission error does.
var statProgress = os.Stat
