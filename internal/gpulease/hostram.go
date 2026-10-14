package gpulease

// The host-RAM term of the grant (2026-10-09, the paging incident).
//
// A lease holds cards; it never held the host's memory. Two ComfyUI lanes that stream bf16 weights
// the card cannot hold ran at once under two card-scoped leases, committed memory reached 162.9 GiB
// on a 127.7 GiB box and the page file grew. The cards were free, so every grant was correct, and
// nothing read the one resource that was not. The allocator's host term (allocator.go) was the only
// place host RAM was ever consulted, and only on the `--cards` path: an explicit `--devices` lease,
// which is what every owner wrapper takes, bypassed it, and it read FREE RAM, which says nothing
// about jobs that have been granted and have not loaded yet.
//
// So a lease now DECLARES the host RAM it will load (Options.HostRAMGiB, stamped on the record as
// Meta.HostRAMGiB), and the grant admits it by gpuprobe.HostRAMAdmits: committed memory now, plus
// the need, plus the part of the leases already granted that has not loaded, must stay under
// physical RAM less the headroom. The check runs on EVERY grant path:
//
//   - a card-scoped grant (--devices, --cards, the media admission): inside grantDevicesLocked,
//     under the epoch lock, after the cards are shown free and before the epoch is issued. The
//     record that carries the declared need is written inside that same critical section, so two
//     grants on different cards cannot both read the same headroom: the second one's check sees
//     the first one's record. The machine's readings (the host's counters, what each declaring
//     holder's processes hold resident) are taken before the lock (readHost) and only compared
//     inside it, so the walk of the process trees is not done while everyone else spins on the lock.
//   - a whole-node grant: in TryAcquire, after the cheap held-probe and before the epoch is
//     bumped (a refusal burns no epoch). No lock is needed there. A whole-node lease conflicts with
//     every other lease, so when it is not refused as held there is nothing live to account for
//     (pending is 0), and a card-scoped grant that lands between this check and the claim is
//     caught by the arbitration that already exists (verifyNoDeviceLeases withdraws the claim).
//
// A refusal that waiting can cure is *ErrHostRAM, and Acquire keeps the request in the same FIFO
// as any other waiter, marked `waiting_for: host-ram` on its record; a refusal waiting cannot cure
// (the need exceeds what this box can ever admit) is the same type with Impossible set and ends the
// request at once.
//
// A lease that unloads the agent seat for itself (`gpu reserve --unload-seat`, F82) is the one exception to
// WHEN the comparison is made, never to WHAT it compares: the memory it would wait for is held by the seat its
// own unload frees, and the unload runs only once the lease holds its cards, so a comparison in the grant waits
// for a grant it can only get by being granted. Such a lease takes its cards declaring nothing
// (Options.HostRAMWaitGiB, stamped on the record as host_ram_wait_gib and read by no grant), the CLI drains and
// unloads, and AwaitHostRAM (below) then puts the one rule to the one need under the epoch lock and, in the same
// critical section, turns the waiting figure into the declared one. Until it does the lease counts as having
// loaded and claimed nothing, which is true: no command runs before the admission.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// WaitHostRAM is Waiter.WaitingFor of a request the cards would admit but the host's memory does
// not yet.
const WaitHostRAM = "host-ram"

// ErrHostRAM is the refusal of a grant on host-RAM grounds. The embedded check carries the numbers
// the decision rested on; Error() is the sentence ("waiting for host RAM: needs N GiB, committed X
// of Y GiB physical, Z GiB headroom").
type ErrHostRAM struct {
	gpuprobe.HostRAMCheck
}

func (e *ErrHostRAM) Error() string { return e.Why }

// installedHostHeadroom is the operator's gpu_host_ram_headroom_gib as config.Load installed it, in
// math.Float64bits (0 = none installed, so gpuprobe's default stands).
var installedHostHeadroom atomic.Uint64

// SetDefaultHostRAMHeadroom installs the operator's headroom (config gpu_host_ram_headroom_gib) for
// EVERY Manager of this process, the way the orphan grace and the term limits are installed
// (SetDefaultOrphanGrace, SetDefaultTerms): config.Load calls it, so no constructor can forget.
// It used to be a per-Manager setter that no production path called, so the grant kept reading the
// built-in 8 GiB while `gpu status` and the card allocator read the configured key: an operator who
// raised the headroom to be safer was admitted up to the difference past the limit he set, and one who
// lowered it to unblock a lane was still refused (the post-implementation review, 2026-10-10).
// Zero, negative or not-a-number restores the default.
func SetDefaultHostRAMHeadroom(gib float64) {
	if math.IsNaN(gib) || math.IsInf(gib, 0) || gib < 0 {
		gib = 0
	}
	installedHostHeadroom.Store(math.Float64bits(gib))
}

// DefaultHostRAMHeadroom is the headroom a Manager without its own keeps uncommitted: the installed
// one, else gpuprobe.DefaultHostRAMHeadroomGiB.
func DefaultHostRAMHeadroom() float64 {
	if g := math.Float64frombits(installedHostHeadroom.Load()); g > 0 {
		return g
	}
	return gpuprobe.DefaultHostRAMHeadroomGiB
}

// SetHostRAMHeadroom gives THIS Manager a headroom of its own, over the installed one. A test seam:
// production leaves it unset and every grant reads the process-wide value config.Load installed.
// Zero or negative means the process-wide value.
func (m *Manager) SetHostRAMHeadroom(gib float64) { m.hostHeadroomGiB = gib }

// HostRAMHeadroom is the headroom in force for this Manager's grants: its own when set, else the
// process-wide one (SetDefaultHostRAMHeadroom), else the built-in default.
func (m *Manager) HostRAMHeadroom() float64 {
	if m.hostHeadroomGiB > 0 {
		return m.hostHeadroomGiB
	}
	return DefaultHostRAMHeadroom()
}

func (m *Manager) hostMemory() (gpuprobe.HostMemory, bool) {
	if m.hostMem != nil {
		return m.hostMem()
	}
	return gpuprobe.ReadHostMemory()
}

// heldGiB is what the processes under a lease holder hold RESIDENT right now, and whether that could be
// read.
func (m *Manager) heldGiB(holderPID int) (float64, bool) {
	if m.workload != nil {
		return m.workload(holderPID)
	}
	return descendantsResidentGiB(holderPID)
}

// holderKey is what the memory a lease has loaded is measured from, and what leases are pooled by. A live holder is its
// pid. A tree-orphan's holder is dead, and the job it started is not below that pid (Linux reparents it; Windows reaches it
// only through the dead pid's parent links, and not at all once the pid is reused), so it stands alone, keyed by its epoch
// and measured from the members it recorded.
type holderKey struct {
	pid   int
	epoch uint64
}

func holderOf(l Info) holderKey {
	if l.TreeOrphan {
		return holderKey{epoch: l.Epoch}
	}
	return holderKey{pid: l.PID}
}

// heldOf is what the processes of a lease's job hold RESIDENT right now: below its holder (heldGiB), or, for a tree-orphan,
// the live members the reader found for it and everything below them (membersResidentGiB).
func (m *Manager) heldOf(l Info) (float64, bool) {
	if l.TreeOrphan {
		return membersResidentGiB(l.TreeLive)
	}
	return m.heldGiB(l.PID)
}

// membersResidentGiB sums the resident memory of every listed member and of each process below it (a member can have
// started more since the record was written), counting a process once. Unlike descendantsResidentGiB the roots are counted:
// they are the job itself, not a long-lived holder.
func membersResidentGiB(members []TreeMember) (float64, bool) {
	seen := map[int]bool{}
	var bytes uint64
	readAny := false
	for _, mem := range members {
		if mem.PID <= 0 {
			continue
		}
		tree, err := ProcessTree(mem.PID)
		if err != nil {
			continue
		}
		readAny = true
		for _, p := range tree {
			if seen[p.PID] {
				continue
			}
			seen[p.PID] = true
			if _, res, ok := processMemory(p.PID); ok {
				bytes += res
			}
		}
	}
	if !readAny && len(members) > 0 {
		return 0, false
	}
	return float64(bytes) / (1 << 30), true
}

// descendantsResidentGiB sums the resident memory (the working set) of every process below holderPID, not
// the holder itself. A wrapper lease's holder is `gpu reserve`, a few MiB, and the job is below it; a pipeline
// lease's holder is the long-lived server, whose own memory is not the job's and must not be subtracted from
// it. A process whose memory cannot be read (another security context) counts 0, which leaves more of the
// declared need pending: the conservative reading.
//
// RESIDENT, not private (G3 of the P0 plan). A lease declares the host RAM it will load, in RAM units
// (the files that stream from RAM, hostneed), and this is what is subtracted from it to find what is still to
// come. Private bytes are commit: memory a process has been promised, resident or not, and on Windows a
// process's GPU allocations may be charged to it without ever occupying system RAM (a llama-server with every
// layer on the card was reported at 29.2 GiB private beside 25.7 GiB of VRAM). Subtracting a figure that
// includes that from a need that does not would call a lane fully loaded while most of its RAM was still to
// come, and the second lane would be admitted into the gap. The working set is the RAM the tree occupies now;
// where private and resident coincide (no card memory in commit) the two readings are the same.
func descendantsResidentGiB(holderPID int) (float64, bool) {
	tree, err := ProcessTree(holderPID)
	if err != nil {
		return 0, false
	}
	var bytes uint64
	for _, p := range tree {
		if p.PID == holderPID {
			continue
		}
		if _, res, ok := processMemory(p.PID); ok {
			bytes += res
		}
	}
	return float64(bytes) / (1 << 30), true
}

// TreeMemory is the private and the resident memory (GiB) of root and every process below it: what a render's
// whole process tree holds right now. It is the measurement path of G3 (the P0 plan): sampled while a render
// runs, its peaks are what the footprint store keeps beside the VRAM peak. Unlike descendantsResidentGiB the root
// is counted, because here the root is the render's own runner, not a long-lived server. A process that cannot
// be read counts as holding nothing (the reading is then LOW, never high: callers that act on it only ever
// raise a declaration with it). err is non-nil only when the process table itself cannot be read.
func TreeMemory(root int) (privateGiB, residentGiB float64, err error) {
	tree, err := ProcessTree(root)
	if err != nil {
		return 0, 0, err
	}
	var priv, res uint64
	for _, p := range tree {
		if pv, rs, ok := processMemory(p.PID); ok {
			priv += pv
			res += rs
		}
	}
	return float64(priv) / (1 << 30), float64(res) / (1 << 30), nil
}

// pendingGiB is the part of the declared needs of leases ALREADY GRANTED that has not loaded yet:
// the memory those jobs are about to commit that the commit charge does not include. Leases that
// share a holder pid are pooled (the pipeline holds one lease per card in one server process, and
// their runners are all below it, so a lease-by-lease subtraction would credit each lease with the
// others' memory and count nothing pending): declared minus held, per holder, never below zero.
// held reports what the processes of a lease's job hold RESIDENT now, the unit a declaration is in: the ones below its
// holder (descendantsResidentGiB), or, for a tree-orphan whose holder is dead, its recorded live members (heldOf). A
// holder it cannot read holds nothing, so its whole declared need is pending.
func pendingGiB(live []Info, held func(l Info) (float64, bool)) float64 {
	declared := map[holderKey]float64{}
	probe := map[holderKey]Info{}
	for _, l := range live {
		if l.HostRAMGiB > 0 {
			k := holderOf(l)
			declared[k] += l.HostRAMGiB
			if _, ok := probe[k]; !ok {
				probe[k] = l
			}
		}
	}
	keys := make([]holderKey, 0, len(declared))
	for k := range declared {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pid != keys[j].pid {
			return keys[i].pid < keys[j].pid
		}
		return keys[i].epoch < keys[j].epoch
	})
	var pending float64
	for _, k := range keys {
		have := 0.0
		if k.pid > 0 || k.epoch > 0 {
			if g, ok := held(probe[k]); ok {
				have = g
			}
		}
		if rest := declared[k] - have; rest > 0 {
			pending += rest
		}
	}
	return pending
}

// HostRAMPending is pendingGiB over the leases live right now, for the readers that apply the same
// rule before a grant (the card allocator's pre-filter) or report it (status).
func (m *Manager) HostRAMPending() float64 { return pendingGiB(m.Leases(), m.heldOf) }

// DeclaredHostRAMGiB is the sum of the host RAM the live leases declared.
func DeclaredHostRAMGiB(leases []Info) float64 {
	var sum float64
	for _, l := range leases {
		sum += l.HostRAMGiB
	}
	return sum
}

// hostReading is the machine's side of a host-RAM check: the host's memory counters and what the processes below each
// declaring lease holder hold resident. A card-scoped grant takes it BEFORE the epoch lock (readHost) and compares it
// inside (hostRAMCheckWith): the counters are syscalls, but the holders' process trees are a walk with a memory read per
// process (measured by the reviewer at about 26 ms median and 63 ms worst for one holder), and that walk used to run inside
// the machine-wide lock on every grant attempt and every waiter poll (F57).
//
// What is read early is only the machine's side. The leases a need is judged against, and so the declared need of every one
// of them, are still read under the lock, so a grant that lands between the reading and the lock is counted. The stale parts
// err the conservative way: a resident figure read a moment early is lower, which leaves more of a declared need still to
// load; a holder that appeared after the reading has no figure at all and counts as having loaded nothing, which is also
// the reading the guard takes of a process it cannot read (descendantsResidentGiB).
type hostReading struct {
	mem   gpuprobe.HostMemory
	memOK bool
	held  map[holderKey]heldReading // by holder (a live holder's pid, a tree-orphan's epoch)
}

type heldReading struct {
	gib float64
	ok  bool
}

// heldOf is Manager.heldOf from the reading: the figure taken early for a holder that was live then, else "cannot be read".
func (r *hostReading) heldOf(l Info) (float64, bool) {
	h, ok := r.held[holderOf(l)]
	if !ok {
		return 0, false
	}
	return h.gib, h.ok
}

// readHost takes the readings a check of needGiB will compare: the host's memory and the resident memory below the holder of
// every live lease that declared a need. A need of 0 is admitted without reading anything (hostRAMCheckAgainst's rule), so it
// takes none (nil).
func (m *Manager) readHost(needGiB float64) *hostReading {
	if needGiB <= 0 {
		return nil
	}
	r := &hostReading{held: map[holderKey]heldReading{}}
	r.mem, r.memOK = m.hostMemory()
	for _, l := range m.Leases() {
		k := holderOf(l)
		if l.HostRAMGiB <= 0 || (k.pid <= 0 && k.epoch == 0) {
			continue
		}
		if _, done := r.held[k]; done {
			continue
		}
		g, ok := m.heldOf(l)
		r.held[k] = heldReading{g, ok}
	}
	return r
}

// hostRAMCheckWith is hostRAMCheckAgainst over a reading taken earlier: the same function of the same four things (the
// host's memory, the need, the not-yet-loaded part of the leases in live, the headroom), with no machine read of its own. It
// is what the grant calls under the epoch lock.
func (m *Manager) hostRAMCheckWith(needGiB float64, live []Info, r *hostReading) gpuprobe.HostRAMCheck {
	if needGiB <= 0 {
		return gpuprobe.HostRAMCheck{OK: true}
	}
	if r == nil {
		return m.hostRAMCheckAgainst(needGiB, live) // a caller that took no reading reads here: correct, only not early
	}
	var pending float64
	if len(live) > 0 {
		pending = pendingGiB(live, r.heldOf)
	}
	return gpuprobe.HostRAMAdmits(r.mem, r.memOK, needGiB, pending, m.HostRAMHeadroom())
}

// hostRAMRefusalWith is hostRAMRefusal over a reading taken earlier.
func (m *Manager) hostRAMRefusalWith(opts Options, live []Info, r *hostReading) *ErrHostRAM {
	if chk := m.hostRAMCheckWith(opts.HostRAMGiB, live, r); !chk.OK {
		return &ErrHostRAM{chk}
	}
	return nil
}

// hostRAMCheckAgainst is THE function that puts the admission rule to a need: the host's memory as it
// reads now, the not-yet-loaded part of the leases in live, this Manager's headroom. The grant
// (hostRAMRefusal, inside its critical section, with the leases it just read) and the read-only
// per-node check (HostRAMCheck) both call it, so a placement that asks before it asks the node and the
// node's own grant cannot disagree: there is one rule and one place that applies it. A need of 0 is
// admitted without reading anything: it adds no memory, so a box that is already over is not made to
// wait for a job that adds nothing to it.
func (m *Manager) hostRAMCheckAgainst(needGiB float64, live []Info) gpuprobe.HostRAMCheck {
	if needGiB <= 0 {
		return gpuprobe.HostRAMCheck{OK: true}
	}
	mem, ok := m.hostMemory()
	var pending float64
	if len(live) > 0 {
		pending = pendingGiB(live, m.heldOf)
	}
	return gpuprobe.HostRAMAdmits(mem, ok, needGiB, pending, m.HostRAMHeadroom())
}

// HostRAMCheck is the admission rule put to a need as it stands right now on THIS node: the host's
// memory, the leases live now and this Manager's headroom, the sentence a refusal would carry. It is
// read-only (it takes no lease, registers no waiter, spends no epoch and writes nothing), so a placer
// can ask whether a lane would be admitted here before it asks the node to take it; the node's own
// grant stays the authority, because a lease can land between this answer and the grant, and the grant
// re-checks under the epoch lock. needGiB <= 0 is always admitted.
func (m *Manager) HostRAMCheck(needGiB float64) gpuprobe.HostRAMCheck {
	return m.hostRAMCheckAgainst(needGiB, m.Leases())
}

// HostRAMCheckWithout is HostRAMCheck for a load that follows a lease which is about to be released: the
// lease with that epoch is left out of the not-yet-loaded sum, because its command has exited and it will
// load nothing more, so counting its declared need as still to come would refuse the very load its release
// makes room for (the warm-back of a seat is such a load). epoch 0 leaves nothing out.
func (m *Manager) HostRAMCheckWithout(needGiB float64, epoch uint64) gpuprobe.HostRAMCheck {
	live := m.Leases()
	if epoch != 0 {
		kept := live[:0:0]
		for _, l := range live {
			if l.Epoch != epoch {
				kept = append(kept, l)
			}
		}
		live = kept
	}
	return m.hostRAMCheckAgainst(needGiB, live)
}

// hostRAMRefusal applies the rule to a grant that declares opts.HostRAMGiB, given the leases live
// at the moment (the caller reads them inside its critical section). nil admits.
func (m *Manager) hostRAMRefusal(opts Options, live []Info) *ErrHostRAM {
	if chk := m.hostRAMCheckAgainst(opts.HostRAMGiB, live); !chk.OK {
		return &ErrHostRAM{chk}
	}
	return nil
}

// AwaitHostRAMOptions bound one AwaitHostRAM.
type AwaitHostRAMOptions struct {
	// Deadline ends the wait with the refusal it last had. The zero time, or one already past, is a single attempt.
	Deadline time.Time
	// Cancel, when closed, ends the wait with ErrCancelled (a signal, a lost lease).
	Cancel <-chan struct{}
	// OnWait is called ONCE, at the first refusal of a wait that will go on (Deadline in the future): the CLI says
	// there that the lease holds its cards and is waiting for the host. Once, because a line per poll is a
	// notification per second in the session that wraps the reserve.
	OnWait func(*ErrHostRAM)
	// HoldUntil, when set, is the moment the caller releases the lease if the host still lacks the room (the CLI passes the
	// earlier of its hold bound and its --wait deadline, which is also the Deadline): it is stamped on the lease's record
	// (Meta.HostRAMHoldUntilMs) when the wait BEGINS, so `gpu status` can say when the lease will be released and the lease's
	// hidden holder can release it itself if the caller died. It is taken off the record only by admission, in the same
	// critical section that declares the figure: a wait that ends any other way leaves it, because a lease that still asks for
	// the host and carries no moment is one nobody is bound to release, and clearing it on the way out left a window of
	// milliseconds in which a caller that died stranded the cards. It bounds nothing by itself; Deadline does.
	HoldUntil time.Time
	// OnStampError, when set, is told when the bound's stamp could not be written. The wait does not fail on it: the stamp is
	// what `gpu status` and the hidden holder read, not what ends the wait, so a failed write costs the status line and the
	// holder's own enforcement, and the CLI says so once.
	OnStampError func(err error)
}

// AwaitHostRAM admits the host RAM a lease asked for after it cleared the seat (Options.HostRAMWaitGiB) and
// returns nil when the host admitted it, which is also what a lease that waits for nothing answers. It
// returns the *ErrHostRAM it last had when the Deadline passes (Impossible at once when no state of the box could
// ever admit the figure), ErrCancelled when Cancel closes, and an error when the lease is gone.
//
// It is the grant's comparison at a later moment, not a looser one. The figure is put to HostRAMAdmits with the
// same host reading, the same headroom and the not-yet-loaded part of every lease that HAS been admitted, and
// the decision and the stamp that makes the figure the lease's declared need (HostRAMGiB, the field every other
// grant counts) happen under the epoch lock, so two leases cannot both take the last headroom: the second one's
// check sees the first one's record. A lease that is still waiting declares nothing, so two of them never wait for
// each other's figure: whichever the lock serves first is admitted and the other counts it. (A design that kept
// the figure on the record from the grant on would make each count the other's as still to load, and a host that
// fits one of them would admit neither.) The cost, stated: while it waits a lease holds no claim on host RAM,
// so a grant on other cards that fits is admitted ahead of it and the lease then waits for that grant's load.
func (m *Manager) AwaitHostRAM(epoch uint64, o AwaitHostRAMOptions) error {
	told := false
	stamped := false
	for {
		need, found := awaitedFigure(m.Leases(), epoch)
		if !found {
			return fmt.Errorf("gpulease: host RAM: the lease is gone (epoch %d)", epoch)
		}
		if need <= 0 {
			return nil // admitted already (or nothing was deferred)
		}
		if cancelled(o.Cancel) {
			return ErrCancelled
		}
		if !stamped && !o.HoldUntil.IsZero() {
			// The wait BEGINS here, before the host is looked at: put the moment it ends on the record, where `gpu status` and the
			// lease's hidden holder (other processes) read it. A reserve that dies before its first look has then still left a bound
			// the holder can enforce. A stamp that cannot be written costs the status line and the holder's own enforcement, not the
			// wait (the bound itself is the caller's Deadline): it is reported, tried again at the next look, and the wait goes on.
			until := o.HoldUntil.UnixMilli()
			if err := m.Restamp(epoch, func(mt *Meta) { mt.HostRAMHoldUntilMs = until }); err == nil {
				stamped = true
			} else if o.OnStampError != nil {
				o.OnStampError(err)
			}
		}
		reading := m.readHost(need) // before the lock, compared inside it (F57)
		var refused *ErrHostRAM
		var gone bool
		err := m.withEpochLock(func() error {
			live := m.Leases()
			cur, still := awaitedFigure(live, epoch)
			if !still {
				gone = true
				return nil
			}
			if cur <= 0 {
				return nil // another caller of this epoch admitted it between the read and the lock
			}
			if r := m.hostRAMRefusalWith(Options{HostRAMGiB: cur}, live, reading); r != nil {
				refused = r
				return nil
			}
			return m.restampLocked(epoch, func(mt *Meta) {
				mt.HostRAMGiB = cur
				mt.HostRAMWaitGiB = 0
				mt.HostRAMHoldUntilMs = 0
			})
		})
		switch {
		case err != nil:
			return err
		case gone:
			return fmt.Errorf("gpulease: host RAM: the lease is gone (epoch %d)", epoch)
		case refused == nil:
			return nil
		case refused.Impossible:
			return refused
		}
		remaining := o.Deadline.Sub(m.now())
		if remaining <= 0 {
			return refused
		}
		if !told && o.OnWait != nil {
			told = true
			o.OnWait(refused)
		}
		m.pauseOrCancel(min(m.pollInterval(), remaining), o.Cancel)
		if cancelled(o.Cancel) {
			return ErrCancelled
		}
	}
}

// holdUntilOf is Meta.HostRAMHoldUntilMs as a time (the zero Time when no bound is running).
func holdUntilOf(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// awaitedFigure is the host RAM the live lease with this epoch is waiting to have admitted (0 = none), and whether the
// lease is live at all.
func awaitedFigure(live []Info, epoch uint64) (float64, bool) {
	for _, l := range live {
		if l.Epoch == epoch {
			return l.HostRAMWaitGiB, true
		}
	}
	return 0, false
}

// AwaitHostRAM is Manager.AwaitHostRAM for the lease's own holder.
func (l *Lease) AwaitHostRAM(o AwaitHostRAMOptions) error {
	if l.done.Load() {
		return errors.New("gpulease: lease already released")
	}
	return l.mgr.AwaitHostRAM(l.epoch, o)
}

// removeClaimFault is a test seam: when set, removeClaim (every lease and claim removal goes through it) fails with what it
// returns. nil in production. UseRemoveFault is the way a test in another package reaches it.
var removeClaimFault func(path string) error

// checkFaultHook is a test seam: when set, Lease.Check fails with what it returns for the epoch. nil in production.
var checkFaultHook func(epoch uint64) error

// UseCheckFault makes Lease.Check fail with what fn returns for the epoch (nil from fn lets the fence run) until the returned
// function is called: the way a test in another package provokes a fence read that fails for a moment, as a read of a record
// that is being replaced does on Windows. Nothing in production calls it.
func UseCheckFault(fn func(epoch uint64) error) (restore func()) {
	old := checkFaultHook
	checkFaultHook = fn
	return func() { checkFaultHook = old }
}

// UseRemoveFault makes every lease-file removal in this process fail with what fn returns for the path (nil from fn lets it
// through), until the returned function is called. A test seam, for a release that must fail where a portable test cannot
// provoke a real filesystem error: nothing in production calls it.
func UseRemoveFault(fn func(path string) error) (restore func()) {
	old := removeClaimFault
	removeClaimFault = fn
	return func() { removeClaimFault = old }
}

// statFaultHook, when set, is consulted by statRecord before the real stat: a test seam (UseStatFault).
var statFaultHook func(path string) error

// statRecord is os.Stat of a lease record, with the test seam in front of it.
func statRecord(path string) error {
	if statFaultHook != nil {
		if err := statFaultHook(path); err != nil {
			return err
		}
	}
	_, err := os.Stat(path)
	return err
}

// UseStatFault makes the examination of a lease record before its release fail with what fn returns for the path (nil from fn
// lets the real stat run), until the returned function is called: the way a test provokes a stat that fails for a reason
// other than the record being absent, as one does on Windows while the record is being replaced. Nothing in production calls it.
func UseStatFault(fn func(path string) error) (restore func()) {
	old := statFaultHook
	statFaultHook = fn
	return func() { statFaultHook = old }
}

// HostRAMHold is what this lease's OWN record says about its wait for the host: the figure it still asks for (0 = admitted, or
// nothing was deferred) and the moment its wait is stamped to end (the zero time = no wait is running). The hidden holder of a
// detached lease reads it on every poll, so that a reserve that died during the wait does not leave the cards held for good. An
// error means the record cannot be read (the lease is gone, or another lease holds the cards now).
func (l *Lease) HostRAMHold() (askedGiB float64, until time.Time, err error) {
	if l.done.Load() {
		return 0, time.Time{}, errors.New("gpulease: lease already released")
	}
	var meta *Meta
	if l.v2 {
		meta, err = readEpochRecord(l.mgr.leaseDir(), l.epoch)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("gpulease: lease is gone (epoch %d)", l.epoch)
		}
	} else {
		meta, err = l.mgr.readMeta()
		if err != nil || meta == nil {
			return 0, time.Time{}, fmt.Errorf("gpulease: lease is gone (epoch %d)", l.epoch)
		}
		if meta.Epoch != l.epoch {
			return 0, time.Time{}, fmt.Errorf("gpulease: fenced out: our epoch %d, current epoch %d", l.epoch, meta.Epoch)
		}
	}
	return meta.HostRAMWaitGiB, holdUntilOf(meta.HostRAMHoldUntilMs), nil
}
