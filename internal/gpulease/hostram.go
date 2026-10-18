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
// So a lease now DECLARES the host memory it will take, in the two quantities the OS accounts apart: the working set it will
// touch (Options.HostRAMGiB, stamped on the record as Meta.HostRAMGiB) and the commit it will be charged
// (Options.HostRAMCommitGiB, stamped as Meta.HostRAMCommitGiB; 0 = the same figure). The grant admits both by
// gpuprobe.HostRAMAdmitsLoad: what it touches, plus what the leases already granted have yet to touch, must leave the headroom
// of the memory that is available; and the commit charge now, plus what it will be charged, must stay under a line that depends
// on who said what the commit is. A commit STATED (Options.HostRAMCommitGiB above 0) is judged where the OS enforces a commit
// limit, with what the leases already granted have yet to be charged, against that limit less the headroom: committed memory is
// not memory in use, and the limit is the line the OS enforces (it refuses allocations there once the page file has reached its
// maximum, and the current limit moves with a system-managed file). A commit nobody stated (a caller that declares one
// figure) is that figure standing in for a commit, and is judged as it always was, with what the leases already granted have
// yet to touch, against physical RAM less the headroom (or an enforced commit limit less the headroom when that sits below physical RAM),
// because a job can commit more than it touches. The check runs on
// EVERY grant path:
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
	"io"
	"io/fs"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// WaitHostRAM is Waiter.WaitingFor of a request the cards would admit but the host's memory does
// not yet.
const WaitHostRAM = "host-ram"

// ErrHostRAM is the refusal of a grant on host-RAM grounds. The embedded check carries the numbers
// the decision rested on; Error() is the sentence, which names the condition that refused and its numbers
// ("waiting for host RAM: needs N GiB, only X GiB of Y GiB physical is available, Z GiB headroom" for the touched set,
// "waiting for host RAM: needs N GiB, committed X of Y GiB physical, Z GiB headroom" for a commit nobody stated ("... of Y GiB commit limit ..." where an
// enforced limit sits below physical RAM), and
// "waiting for host RAM: needs N GiB of commit, committed X of Y GiB commit limit, Z GiB headroom" for a stated one).
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

// heldLoad is what the processes under a lease holder hold right now, and whether that could be read: TouchGiB is their
// working set (resident) and CommitGiB their private bytes (the currency of the system commit charge: PrivateUsage on
// Windows, the private writable mappings on Linux).
func (m *Manager) heldLoad(holderPID int) (gpuprobe.HostLoad, bool) {
	if m.workload != nil {
		return m.workload(holderPID)
	}
	return descendantsHeld(holderPID)
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

// heldOf is what the processes of a lease's job hold right now: below its holder (heldLoad), or, for a tree-orphan, the live
// members the reader found for it and everything below them (membersHeld).
func (m *Manager) heldOf(l Info) (gpuprobe.HostLoad, bool) {
	if l.TreeOrphan {
		return membersHeld(l.TreeLive)
	}
	return m.heldLoad(l.PID)
}

const gibBytes = 1 << 30

// membersHeld sums the resident and the private memory of every listed member and of each process below it (a member can have
// started more since the record was written), counting a process once. Unlike descendantsHeld the roots are counted:
// they are the job itself, not a long-lived holder.
func membersHeld(members []TreeMember) (gpuprobe.HostLoad, bool) {
	seen := map[int]bool{}
	var priv, res uint64
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
			if pv, rs, ok := processMemory(p.PID); ok {
				priv += pv
				res += rs
			}
		}
	}
	if !readAny && len(members) > 0 {
		return gpuprobe.HostLoad{}, false
	}
	return gpuprobe.HostLoad{TouchGiB: float64(res) / gibBytes, CommitGiB: float64(priv) / gibBytes}, true
}

// membersResidentGiB is the working-set half of membersHeld.
func membersResidentGiB(members []TreeMember) (float64, bool) {
	l, ok := membersHeld(members)
	return l.TouchGiB, ok
}

// descendantsHeld sums the resident memory (the working set) and the private bytes of every process below holderPID, not
// the holder itself. A wrapper lease's holder is `gpu reserve`, a few MiB, and the job is below it; a pipeline
// lease's holder is the long-lived server, whose own memory is not the job's and must not be subtracted from
// it. A process whose memory cannot be read (another security context) counts 0, which leaves more of the
// declared need pending: the conservative reading.
//
// EACH QUANTITY IS SUBTRACTED FROM ITS OWN DECLARATION. A lease declares the host RAM it
// will touch, in RAM units (the files that stream from RAM, hostneed), and the working set is what is subtracted from it to
// find what is still to be touched. It declares the commit it will be charged, and the private bytes are what is subtracted
// from that: private bytes ARE the currency of the commit charge, memory a process has been promised, resident or not (on
// Linux that is VmData, the private writable mappings, because Committed_AS charges a mapping when it is made; the touched
// part, RssAnon, would count an untouched mapping once in the charge and again here). On
// Windows a process's GPU allocations may be charged to it without ever occupying system RAM (a llama-server with every
// layer on the card was reported at 29.2 GiB private beside 25.7 GiB of VRAM), which is exactly why private bytes must not be
// subtracted from a touched-set declaration (a lane would read as fully loaded while most of its RAM was still to come, and
// the second lane would be admitted into the gap) and why they are the right figure against a commit declaration. Where
// private and resident coincide (no card memory in commit) the two readings are the same.
func descendantsHeld(holderPID int) (gpuprobe.HostLoad, bool) {
	tree, err := ProcessTree(holderPID)
	if err != nil {
		return gpuprobe.HostLoad{}, false
	}
	var priv, res uint64
	for _, p := range tree {
		if p.PID == holderPID {
			continue
		}
		if pv, rs, ok := processMemory(p.PID); ok {
			priv += pv
			res += rs
		}
	}
	return gpuprobe.HostLoad{TouchGiB: float64(res) / gibBytes, CommitGiB: float64(priv) / gibBytes}, true
}

// descendantsResidentGiB is the working-set half of descendantsHeld.
func descendantsResidentGiB(holderPID int) (float64, bool) {
	l, ok := descendantsHeld(holderPID)
	return l.TouchGiB, ok
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

// pendingLoad is the part of the declared needs of leases ALREADY GRANTED that has not loaded yet, in both quantities: the memory
// those jobs are about to touch that the available figure does not yet lose, and the commit they are about to be charged that
// the commit charge does not include. Leases that
// share a holder pid are pooled (the pipeline holds one lease per card in one server process, and
// their runners are all below it, so a lease-by-lease subtraction would credit each lease with the
// others' memory and count nothing pending): declared minus held, per holder and per quantity, never below zero.
// held reports what the processes of a lease's job hold now, in the unit each declaration is in: the ones below its
// holder (descendantsHeld), or, for a tree-orphan whose holder is dead, its recorded live members (heldOf). A
// holder it cannot read holds nothing, so its whole declared need is pending.
func pendingLoad(live []Info, held func(l Info) (gpuprobe.HostLoad, bool)) gpuprobe.HostLoad {
	declared := map[holderKey]gpuprobe.HostLoad{}
	probe := map[holderKey]Info{}
	for _, l := range live {
		if l.DeclaresHostRAM() {
			k := holderOf(l)
			d := declared[k]
			d.TouchGiB += l.HostRAMGiB
			d.CommitGiB += effectiveCommit(l.HostRAMGiB, l.HostRAMCommitGiB)
			declared[k] = d
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
	var pending gpuprobe.HostLoad
	for _, k := range keys {
		var have gpuprobe.HostLoad
		if k.pid > 0 || k.epoch > 0 {
			if g, ok := held(probe[k]); ok {
				have = g
			}
		}
		if rest := declared[k].TouchGiB - have.TouchGiB; rest > 0 {
			pending.TouchGiB += rest
		}
		if rest := declared[k].CommitGiB - have.CommitGiB; rest > 0 {
			pending.CommitGiB += rest
		}
	}
	return pending
}

// HostRAMPendingLoad is pendingLoad over the leases live right now, for the readers that apply the same
// rule before a grant (the card allocator's pre-filter) or report it (status).
func (m *Manager) HostRAMPendingLoad() gpuprobe.HostLoad { return pendingLoad(m.Leases(), m.heldOf) }

// HostRAMPending is the touched-set half of HostRAMPendingLoad: what the live leases have yet to touch.
func (m *Manager) HostRAMPending() float64 { return m.HostRAMPendingLoad().TouchGiB }

// HostRAMPendingCommit is the commit half of HostRAMPendingLoad: what the live leases have yet to be charged.
func (m *Manager) HostRAMPendingCommit() float64 { return m.HostRAMPendingLoad().CommitGiB }

// DeclaredHostRAMLoad is the sum of what the live leases declared, per quantity.
func DeclaredHostRAMLoad(leases []Info) gpuprobe.HostLoad {
	var sum gpuprobe.HostLoad
	for _, l := range leases {
		sum.TouchGiB += l.HostRAMGiB
		sum.CommitGiB += effectiveCommit(l.HostRAMGiB, l.HostRAMCommitGiB)
	}
	return sum
}

// DeclaredHostRAMGiB is the sum of the working set the live leases declared.
func DeclaredHostRAMGiB(leases []Info) float64 { return DeclaredHostRAMLoad(leases).TouchGiB }

// hostReading is the machine's side of a host-RAM check: the host's memory counters and what the processes below each
// declaring lease holder hold. A card-scoped grant takes it BEFORE the epoch lock (readHost) and compares it
// inside (hostRAMCheckWith): the counters are syscalls, but the holders' process trees are a walk with a memory read per
// process (measured by the reviewer at about 26 ms median and 63 ms worst for one holder), and that walk used to run inside
// the machine-wide lock on every grant attempt and every waiter poll (F57).
//
// What is read early is only the machine's side. The leases a need is judged against, and so the declared need of every one
// of them, are still read under the lock, so a grant that lands between the reading and the lock is counted. The stale parts
// err the conservative way: a figure read a moment early is lower, which leaves more of a declared need still to
// load; a holder that appeared after the reading has no figure at all and counts as having loaded nothing, which is also
// the reading the guard takes of a process it cannot read (descendantsHeld).
type hostReading struct {
	mem   gpuprobe.HostMemory
	memOK bool
	held  map[holderKey]heldReading // by holder (a live holder's pid, a tree-orphan's epoch)
}

type heldReading struct {
	load gpuprobe.HostLoad
	ok   bool
}

// heldOf is Manager.heldOf from the reading: the figure taken early for a holder that was live then, else "cannot be read".
func (r *hostReading) heldOf(l Info) (gpuprobe.HostLoad, bool) {
	h, ok := r.held[holderOf(l)]
	if !ok {
		return gpuprobe.HostLoad{}, false
	}
	return h.load, h.ok
}

// readHost takes the readings a check of need will compare: the host's memory and what is held below the holder of
// every live lease that declared a need. A need that declares nothing is admitted without reading anything
// (hostRAMCheckAgainst's rule), so it takes none (nil).
func (m *Manager) readHost(need gpuprobe.HostLoad) *hostReading {
	if !need.Declares() {
		return nil
	}
	r := &hostReading{held: map[holderKey]heldReading{}}
	r.mem, r.memOK = m.hostMemory()
	for _, l := range m.Leases() {
		k := holderOf(l)
		if !l.DeclaresHostRAM() || (k.pid <= 0 && k.epoch == 0) {
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
func (m *Manager) hostRAMCheckWith(need gpuprobe.HostLoad, live []Info, r *hostReading) gpuprobe.HostRAMCheck {
	if !need.Declares() {
		return gpuprobe.HostRAMCheck{OK: true}
	}
	if r == nil {
		return m.hostRAMCheckAgainst(need, live) // a caller that took no reading reads here: correct, only not early
	}
	var pending gpuprobe.HostLoad
	if len(live) > 0 {
		pending = pendingLoad(live, r.heldOf)
	}
	return gpuprobe.HostRAMAdmitsLoad(r.mem, r.memOK, need, pending, m.HostRAMHeadroom())
}

// hostRAMRefusalWith is hostRAMRefusal over a reading taken earlier.
func (m *Manager) hostRAMRefusalWith(opts Options, live []Info, r *hostReading) *ErrHostRAM {
	if chk := m.hostRAMCheckWith(opts.hostLoad(), live, r); !chk.OK {
		return &ErrHostRAM{chk}
	}
	return nil
}

// hostRAMCheckAgainst is THE function that puts the admission rule to a need: the host's memory as it
// reads now, the not-yet-loaded part of the leases in live, this Manager's headroom. The grant
// (hostRAMRefusal, inside its critical section, with the leases it just read) and the read-only
// per-node check (HostRAMCheck) both call it, so a placement that asks before it asks the node and the
// node's own grant cannot disagree: there is one rule and one place that applies it. A need that declares nothing is
// admitted without reading anything: it adds no memory, so a box that is already over is not made to
// wait for a job that adds nothing to it.
func (m *Manager) hostRAMCheckAgainst(need gpuprobe.HostLoad, live []Info) gpuprobe.HostRAMCheck {
	if !need.Declares() {
		return gpuprobe.HostRAMCheck{OK: true}
	}
	mem, ok := m.hostMemory()
	var pending gpuprobe.HostLoad
	if len(live) > 0 {
		pending = pendingLoad(live, m.heldOf)
	}
	return gpuprobe.HostRAMAdmitsLoad(mem, ok, need, pending, m.HostRAMHeadroom())
}

// HostRAMCheckLoad is the admission rule put to a declaration as it stands right now on THIS node: the host's
// memory, the leases live now and this Manager's headroom, the sentence a refusal would carry. It
// is read-only (it takes no lease, registers no waiter, spends no epoch and writes nothing), so a placer
// can ask whether a lane would be admitted here before it asks the node to take it; the node's own
// grant stays the authority, because a lease can land between this answer and the grant, and the grant
// re-checks under the epoch lock. A need that declares nothing is always admitted.
func (m *Manager) HostRAMCheckLoad(need gpuprobe.HostLoad) gpuprobe.HostRAMCheck {
	return m.hostRAMCheckAgainst(need, m.Leases())
}

// HostRAMCheck is HostRAMCheckLoad for a caller that declares one figure, taken for both quantities. It states no commit, so the
// commit is judged against physical RAM less the headroom (the nearer enforced commit limit, where one sits below it), the line such a
// caller has always been held to.
func (m *Manager) HostRAMCheck(needGiB float64) gpuprobe.HostRAMCheck {
	return m.HostRAMCheckLoad(gpuprobe.HostLoad{TouchGiB: needGiB, CommitGiB: needGiB})
}

// HostRAMCheckLoadWithout is HostRAMCheckLoad for a load that follows a lease which is about to be released: the
// lease with that epoch is left out of the not-yet-loaded sum, because its command has exited and it will
// load nothing more, so counting its declared need as still to come would refuse the very load its release
// makes room for (the warm-back of a seat is such a load). epoch 0 leaves nothing out.
func (m *Manager) HostRAMCheckLoadWithout(need gpuprobe.HostLoad, epoch uint64) gpuprobe.HostRAMCheck {
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
	return m.hostRAMCheckAgainst(need, live)
}

// HostRAMCheckWithout is HostRAMCheckLoadWithout for one figure, taken for both quantities (and, as HostRAMCheck, with no commit stated).
func (m *Manager) HostRAMCheckWithout(needGiB float64, epoch uint64) gpuprobe.HostRAMCheck {
	return m.HostRAMCheckLoadWithout(gpuprobe.HostLoad{TouchGiB: needGiB, CommitGiB: needGiB}, epoch)
}

// hostRAMRefusal applies the rule to a grant that declares opts.HostRAMGiB (and opts.HostRAMCommitGiB), given the leases live
// at the moment (the caller reads them inside its critical section). nil admits.
func (m *Manager) hostRAMRefusal(opts Options, live []Info) *ErrHostRAM {
	if chk := m.hostRAMCheckAgainst(opts.hostLoad(), live); !chk.OK {
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
	// RequireStamp makes a stamp that cannot be written a failure of the wait: the stamp is tried a few times, and if it still has
	// not landed the wait ends with *ErrHoldStamp before the host is looked at. The caller that sets it is one whose hidden holder
	// is the only thing that can release the lease if the caller dies, so a wait the holder cannot see the end of must not run.
	RequireStamp bool
}

// ErrHoldStamp is the failure of a wait that RequireStamp bound to its stamp: the moment the wait ends could not be put on the
// lease's record, after Tries attempts. The record is left as it was (an earlier, outer stamp stays).
type ErrHoldStamp struct {
	Epoch uint64
	Tries int
	Err   error
}

func (e *ErrHoldStamp) Error() string {
	return fmt.Sprintf("gpulease: host RAM: the moment the wait ends could not be written on the record of epoch %d after %d tries: %v", e.Epoch, e.Tries, e.Err)
}

func (e *ErrHoldStamp) Unwrap() error { return e.Err }

// StampHostRAMHold puts the moment a host-RAM wait ends on the record of lease epoch (Meta.HostRAMHoldUntilMs). It does it only
// while the lease still asks for the host: on an admitted lease the stamp would be a bound nothing is running.
func (m *Manager) StampHostRAMHold(epoch uint64, until time.Time) error {
	return m.StampHostRAMHoldPhase(epoch, until, HoldPhaseWait)
}

// The phases of the moment a lease's hold stamp (Meta.HostRAMHoldUntilMs) stands for. Outer: a detached reserve's bound while it
// still clears the seat (the drain, the unload); the host has not been asked, and a live reserve keeps moving it, so it is the
// moment the hidden holder releases the lease only if the reserve DIES. Wait: the end of the wait for the host. Release: the
// reserve is releasing the lease itself and has pushed the stamp out so the holder does not race it. An older writer leaves none.
const (
	HoldPhaseOuter   = "outer"
	HoldPhaseWait    = "wait"
	HoldPhaseRelease = "release"
)

// StampHostRAMHoldPhase is StampHostRAMHold saying which phase the stamped moment is.
func (m *Manager) StampHostRAMHoldPhase(epoch uint64, until time.Time, phase string) error {
	if stampFaultHook != nil {
		if err := stampFaultHook(epoch); err != nil {
			return err
		}
	}
	ms := until.UnixMilli()
	return m.Restamp(epoch, func(mt *Meta) {
		if mt.HostRAMWaitGiB <= 0 && mt.HostRAMWaitCommitGiB <= 0 {
			return // admitted (or nothing was deferred): no bound is running
		}
		mt.HostRAMHoldUntilMs = ms
		mt.HostRAMHoldPhase = phase
	})
}

// stampTries and stampRetryPause are how often, and how far apart, a stamp that RequireStamp binds the wait to is tried before the
// wait fails. Variables so a test need not stand in line for the pause.
var (
	stampTries      = 4
	stampRetryPause = 250 * time.Millisecond
)

// stampFaultHook is a test seam: when set, StampHostRAMHold fails with what it returns for the epoch. nil in production.
var stampFaultHook func(epoch uint64) error

// UseStampFault makes every StampHostRAMHold in this process fail with what fn returns for the epoch (nil from fn lets it
// through), until the returned function is called: the way a test in another package provokes a stamp that cannot be written
// while the other restamps of the lease (the drain's, the exclusive one) still work. Nothing in production calls it.
func UseStampFault(fn func(epoch uint64) error) (restore func()) {
	old := stampFaultHook
	stampFaultHook = fn
	return func() { stampFaultHook = old }
}

// AwaitHostRAM admits the host RAM a lease asked for after it cleared the seat (Options.HostRAMWaitGiB) and
// returns nil when the host admitted it, which is also what a lease that waits for nothing answers. It
// returns the *ErrHostRAM it last had when the Deadline passes (Impossible at once when no state of the box could
// ever admit the figure), ErrCancelled when Cancel closes, and an error when the lease is gone.
//
// It is the grant's comparison at a later moment, not a looser one. Both figures are put to HostRAMAdmitsLoad with the
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
		need, found := awaitedLoad(m.Leases(), epoch)
		if !found {
			if err := m.leaseMissing(epoch); err != nil {
				return err
			}
			continue // it turned up again: a read that failed for a moment
		}
		if !need.Declares() {
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
			tries := 1
			if o.RequireStamp {
				tries = stampTries
			}
			var last error
			for i := 0; i < tries && !stamped; i++ {
				if i > 0 {
					m.pauseOrCancel(stampRetryPause, o.Cancel)
					if cancelled(o.Cancel) {
						return ErrCancelled
					}
				}
				if last = m.StampHostRAMHold(epoch, o.HoldUntil); last == nil {
					stamped = true
				} else if o.OnStampError != nil {
					o.OnStampError(last)
				}
			}
			if !stamped && o.RequireStamp {
				if _, live := awaitedLoad(m.Leases(), epoch); !live {
					if err := m.leaseMissing(epoch); err != nil {
						return err // that, not the stamp, is what failed
					}
					continue
				}
				// The caller's hidden holder is the only thing that releases this lease if the caller dies, and it can enforce only
				// what is on the record: a wait it cannot see the end of must not run. The host has not been looked at.
				return &ErrHoldStamp{Epoch: epoch, Tries: tries, Err: last}
			}
		}
		reading := m.readHost(need) // before the lock, compared inside it (F57)
		var refused *ErrHostRAM
		var gone bool
		err := m.withEpochLock(func() error {
			live := m.Leases()
			cur, still := awaitedLoad(live, epoch)
			if !still {
				gone = true
				return nil
			}
			if !cur.Declares() {
				return nil // another caller of this epoch admitted it between the read and the lock
			}
			if chk := m.hostRAMCheckWith(cur, live, reading); !chk.OK {
				refused = &ErrHostRAM{chk}
				return nil
			}
			return m.restampLocked(epoch, func(mt *Meta) {
				mt.HostRAMGiB = cur.TouchGiB
				mt.HostRAMCommitGiB = 0
				if cur.CommitStated {
					mt.HostRAMCommitGiB = storedCommit(cur.CommitGiB)
				}
				mt.HostRAMWaitGiB = 0
				mt.HostRAMWaitCommitGiB = 0
				mt.HostRAMHoldUntilMs = 0
				mt.HostRAMHoldPhase = ""
			})
		})
		switch {
		case err != nil:
			return err
		case gone:
			if err := m.leaseMissing(epoch); err != nil {
				return err
			}
			continue
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

// The wait looks for a lease that is not among the live ones this many more times, this far apart, before it believes the lease is
// gone: a record read that fails for a moment (a rename that replaces it, a scanner holding it) shows the lease as absent.
var (
	missingRechecks     = 3
	missingRecheckPause = 100 * time.Millisecond
)

// leaseMissing is what AwaitHostRAM answers when the lease it waits for is not among the live ones. It looks again, and returns nil
// if the lease turned up. Otherwise it says why it is missing from the record itself: the lease is gone, or its record cannot be read
// (which is not the same, and keeps its cause).
func (m *Manager) leaseMissing(epoch uint64) error {
	var st RecordState
	var cause error
	for i := 0; i <= missingRechecks; i++ {
		if i > 0 {
			m.pause(missingRecheckPause)
		}
		if _, found := awaitedLoad(m.Leases(), epoch); found {
			return nil
		}
		st, cause = m.RecordState(epoch)
	}
	if st == RecordUnreadable {
		return fmt.Errorf("gpulease: host RAM: the record of epoch %d cannot be read, so the lease is not known to be gone: %w", epoch, cause)
	}
	return fmt.Errorf("gpulease: host RAM: the lease is gone (epoch %d)", epoch)
}

// holdPhaseOf is the phase a record's hold stamp is in: none without a stamp, and the wait's for a stamp an older writer made.
func holdPhaseOf(meta *Meta) string {
	switch {
	case meta.HostRAMHoldUntilMs <= 0:
		return ""
	case meta.HostRAMHoldPhase == "":
		return HoldPhaseWait
	}
	return meta.HostRAMHoldPhase
}

// holdUntilOf is Meta.HostRAMHoldUntilMs as a time (the zero Time when no bound is running).
func holdUntilOf(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// awaitedLoad is the host memory the live lease with this epoch is waiting to have admitted (both quantities; nothing
// declared = none), and whether the lease is live at all.
func awaitedLoad(live []Info, epoch uint64) (gpuprobe.HostLoad, bool) {
	for _, l := range live {
		if l.Epoch == epoch {
			return gpuprobe.HostLoad{TouchGiB: l.HostRAMWaitGiB, CommitGiB: effectiveCommit(l.HostRAMWaitGiB, l.HostRAMWaitCommitGiB), CommitStated: l.HostRAMWaitCommitStated}, true
		}
	}
	return gpuprobe.HostLoad{}, false
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

// readFaultHook, when set, is consulted by readRecordFile before the real read: a test seam (UseReadFault).
var readFaultHook func(path string) error

// readRecordFile reads a lease record (meta.json, e/<epoch>.json), with the test seam in front of the real read.
//
// It reads through a handle that shares delete (OpenShared), and that is what makes the READ side of a record rewrite safe.
// A record is replaced in place while its lease is held (the grant's granting-to-active write, every Restamp), by a rename over
// the live name. On Windows a plain os.ReadFile that meets that rename fails with ERROR_SHARING_VIOLATION (rarely ERROR_ACCESS_DENIED)
// although the record is there and good a moment later. The rename holds a handle with DELETE access on the old entry, and an open
// whose own share mode lacks FILE_SHARE_DELETE collides with it. A reader that gave up on that failure answered "no lease" for a
// lease that was held: v2() dropped the record, so Leases() was empty and EpochIsCurrent said the render child's lease was not its
// own. A reader that shares delete does not collide, and reads the old record whole until the new one is in place, as a POSIX reader
// does. (A rename still waits for any handle open on the old entry, this one included, which is why renameReplacing retries; a
// record read is short.) Off Windows this is os.Open.
func readRecordFile(path string) ([]byte, error) {
	if readFaultHook != nil {
		if err := readFaultHook(path); err != nil {
			return nil, err
		}
	}
	return readSharedFile(path)
}

// openSharedFile is OpenShared, as a variable so a test can make the open fail the way it fails while a rename replaces the file.
var openSharedFile = OpenShared

// readSharedFile reads a whole file through a handle that shares delete (OpenShared), for a file its writer replaces by a rename over
// the live name: a lease record (through readRecordFile) and the heartbeat hb.<epoch> (Renew). It does not pass through the record's
// read fault seam (UseReadFault): the heartbeat is read without the record's on purpose, because a record that cannot be read is
// exactly when the heartbeat is the evidence of life (keeper.go).
//
// The heartbeat matters more than the record here. A record that cannot be read is skipped, and the lease is only briefly missing from
// a listing. A heartbeat that cannot be read used to fall back to the stamp written at acquisition, which for a lease past its declared
// window is stale: the reclaim rule (stale heartbeat AND expired window) then called a live lease debris, the acquirer's sweep removed
// it, and the card was granted to a second job while the first still ran on it. It no longer does: heartbeatAt takes the file's own
// modification time when it can be examined and calls the heartbeat UNKNOWN when it cannot, and the reclaim rule never reads unknown as
// stale. The stamp from the acquisition stays only for a file that is settled absent (settled.go). So the wait below narrows the
// window in which a heartbeat is unknown, and no longer carries the weight of keeping a live lease from being reclaimed.
//
// On Windows an open that fails with a permission error is tried again, removeAttempts times at removePause (about 100 ms in all, the
// bound the claim removal uses). An open that finds the name ABSENT is not an answer while a rename may be installing it: with a fresh
// staged file beside the name (stagingPath, settled.go) the read waits for the rename to end, up to absentBudget, and if the budget runs
// out it returns an I/O failure (errRenameInFlight) and never "not found"; with none it looks once at the name itself, and answers
// absent only when that finds nothing too (gate.go). Any other failure is returned at once. A file that is never replaced by a rename (a
// card claim) costs one open and nothing else. The shared open takes the sharing violation away, but not
// the instant the rename has marked the old entry for deletion and the new one has not taken its name: the file is delete-pending, and
// every open of the name fails with ERROR_ACCESS_DENIED until the rename ends, a few milliseconds. Measured on a loaded host, with the
// shared open in, that was 5 failed reads in about 124,000 of a heartbeat that was renewed in a loop.
//
// A file that is still refused when the wait ends is not a rename in flight but a lasting fault (a deny-read ACL, a foreign delete
// whose handle never closes), and it is waited out ONCE: refusedFiles remembers it for refusedMemory, and a read of it in that time
// opens it once, so a repair is seen at once, and takes no pause if it is still refused. Without that the cost is per read, not per
// file: a grant reads each record five times and each heartbeat seven inside one hold of the epoch lock (the sweep, the scan for the
// unreadable and the verify each go over them), so one refused record held the lock for about 500 ms, and eight refused files for 4.4
// s, longer than the 2 s a second acquirer's spin allows. A refusal that passes inside the wait (the rename case) is never
// remembered, so it keeps the full wait.
//
// Off Windows there is no such instant, so a permission error is returned at once (sharedOpenMeetsDeletePending, beside OpenShared).
// It is a lasting refusal there, a record written with a mode the reader cannot read, and the wait cost about 108 ms per unreadable
// record per read: Leases() and the acquirer, which reads the records under the epoch lock, paid it for every one.
func readSharedFile(path string) ([]byte, error) {
	var watch gateWatch
	denied := 0 // the refusals met so far: the wait for a refused open is counted apart from the gate's
	for {
		f, err := openSharedFile(path)
		if err == nil {
			forgetRefusedFile(path)
			forgetAbsentExpiry(path)
			defer f.Close()
			return io.ReadAll(f)
		}
		if deniedOpenMayClear && errors.Is(err, fs.ErrNotExist) {
			// Absent. For the instant a rename installs the file that is not an answer yet (gate.go): wait while a fresh staged file
			// stands beside the name, and look at the name itself once when none does.
			switch watch.afterAbsentRead(path) {
			case gateWaited:
				continue
			case gateExpired:
				return nil, &fs.PathError{Op: "open", Path: path, Err: errRenameInFlight}
			case gateClear:
				if _, serr := statFile(path); serr != nil && errors.Is(serr, fs.ErrNotExist) {
					return nil, err // settled: nothing is installing it and the name is not there
				}
				if watch.nameIsThere(path) == gateExpired {
					return nil, &fs.PathError{Op: "open", Path: path, Err: errRenameInFlight}
				}
				continue
			}
			return nil, err // a file that is never replaced by a rename (a card claim): absent is the answer
		}
		if !deniedOpenMayClear || !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
		if denied == 0 && refusedFileRemembered(path) {
			return nil, err // it outlasted a whole wait a moment ago: this open was the look at whether it has been repaired
		}
		if denied >= removeAttempts {
			rememberRefusedFile(path)
			return nil, err
		}
		denied++
		readRetries.Add(1)
		time.Sleep(removePause)
	}
}

// ReadSharedFile is readSharedFile for a reader in another package of a file its writer replaces by a rename over the live name: the
// ComfyUI launch markers (internal/comfyinst), which render/comfy-ownership.mjs restamps through a staged .tmp file.
func ReadSharedFile(path string) ([]byte, error) { return readSharedFile(path) }

// readRetries counts the pauses readSharedFile has taken waiting out a refused open, since the process started: a test seam, not a
// metric (readrace_linux_test.go asserts it stays 0 where no rename can cause a refusal; readrelease_test.go, that a missing file never adds to it).
var readRetries atomic.Int64

// deniedOpenMayClear says whether a permission error on the open is worth waiting out: sharedOpenMeetsDeletePending on the build, as a
// variable so a test can run the wait off Windows and the refusal on it (readretry_test.go).
var deniedOpenMayClear = sharedOpenMeetsDeletePending

// refusedMemory is how long a file that outlasted the whole wait is read without a new wait. Long enough to cover the grants and
// the polls of a burst of acquirers that meet one lasting fault, short enough that a refusal which starts again much later is a
// new thing to wait for.
var refusedMemory = 5 * time.Second

// refusedFiles holds the files readSharedFile gave up waiting for, with the time its memory of each ends. refusedCount mirrors the
// size of the map, so the common read (nothing remembered) takes no lock.
var (
	refusedFilesMu sync.Mutex
	refusedFiles   = map[string]time.Time{}
	refusedCount   atomic.Int64
)

func rememberRefusedFile(path string) {
	now := time.Now()
	refusedFilesMu.Lock()
	defer refusedFilesMu.Unlock()
	for p, until := range refusedFiles { // drop what has run out, so files that are never read again do not pile up
		if !now.Before(until) {
			delete(refusedFiles, p)
		}
	}
	refusedFiles[path] = now.Add(refusedMemory)
	refusedCount.Store(int64(len(refusedFiles)))
}

func refusedFileRemembered(path string) bool {
	if refusedCount.Load() == 0 {
		return false
	}
	refusedFilesMu.Lock()
	defer refusedFilesMu.Unlock()
	until, ok := refusedFiles[path]
	if ok && !time.Now().Before(until) {
		delete(refusedFiles, path)
		refusedCount.Store(int64(len(refusedFiles)))
		return false
	}
	return ok
}

// forgetRefusedFile is called on every open that succeeds: a file that can be read again is no longer a lasting fault.
func forgetRefusedFile(path string) {
	if refusedCount.Load() == 0 {
		return
	}
	refusedFilesMu.Lock()
	defer refusedFilesMu.Unlock()
	delete(refusedFiles, path)
	refusedCount.Store(int64(len(refusedFiles)))
}

// UseReadFault makes the READ of a lease record fail with what fn returns for the path (nil from fn lets the real read run), until the
// returned function is called: the way a test provokes a record that EXISTS and can be removed but cannot be read, as one is while
// a scanner or another process holds it open on Windows. The file itself is untouched, so the record is still there for the fault to
// be lifted. Nothing in production calls it.
func UseReadFault(fn func(path string) error) (restore func()) {
	old := readFaultHook
	readFaultHook = fn
	return func() { readFaultHook = old }
}

// statFaultHook, when set, is consulted by statRecord before the real stat: a test seam (UseStatFault).
var statFaultHook func(path string) error

// statRecord is the stat of a lease record, with the test seam in front of it. A record is replaced by a rename over the live name, and a
// stat that met the instant the name is absent would call a held lease absent: it goes through statSettled, which waits for the rename
// (Windows) and answers an I/O failure, never an absence, when the wait runs out. Every caller already reads an error that is not "absent"
// as "cannot be examined".
func statRecord(path string) error {
	_, err := statSettled(path)
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

// RecordUnreadableError is what Lease.Check answers when the lease's record exists as far as anyone knows and could not be READ
// (an access error while it is replaced, a sharing violation, an I/O error): not "the lease is gone" and not "fenced out", which
// are verdicts, but a failure that may pass. A caller that would let go of its lease on a failed fence can tell the two apart.
type RecordUnreadableError struct {
	Epoch uint64
	Err   error
}

func (e *RecordUnreadableError) Error() string {
	return fmt.Sprintf("gpulease: the record of lease epoch %d cannot be read: %v", e.Epoch, e.Err)
}

func (e *RecordUnreadableError) Unwrap() error { return e.Err }

// IsRecordUnreadable is true for a failure of the fence that is a failed READ, not a verdict.
func IsRecordUnreadable(err error) bool {
	var r *RecordUnreadableError
	return errors.As(err, &r)
}

// MarkHolderGaveUp records on the lease's record that its hidden holder gave up releasing it, and why (best effort, by a holder
// that is about to exit with the lease still held).
func (m *Manager) MarkHolderGaveUp(epoch uint64, why string) error {
	if r := []rune(why); len(r) > 200 {
		why = string(r[:199]) + "…"
	}
	at := m.now().UnixMilli()
	return m.Restamp(epoch, func(mt *Meta) {
		mt.HolderGaveUpMs = at
		mt.HolderGaveUpWhy = why
	})
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
	return max(meta.HostRAMWaitGiB, meta.HostRAMWaitCommitGiB), holdUntilOf(meta.HostRAMHoldUntilMs), nil
}

// RecordState is what the record of one lease epoch says about whether that lease is held, as far as a release can tell.
type RecordState int

const (
	// RecordAbsent: no record of this epoch (a card lease's e/<epoch>.json, or a whole-node meta.json naming it) exists. Gone.
	RecordAbsent RecordState = iota
	// RecordHeld: the record exists and could be read.
	RecordHeld
	// RecordUnreadable: the record exists, or may exist, and could not be read (an access error while it is replaced, a sharing
	// violation, an I/O error, a directory where the file was). The lease is HELD for every decision that matters: never gone.
	RecordUnreadable
)

func (s RecordState) String() string {
	switch s {
	case RecordHeld:
		return "held"
	case RecordUnreadable:
		return "unreadable"
	}
	return "absent"
}

// RecordState looks at the record of lease epoch directly (not through the reader, which treats a record it cannot read as
// absent): absent, held, or unreadable, with the cause for the last.
func (m *Manager) RecordState(epoch uint64) (RecordState, error) {
	if epoch == 0 {
		return RecordAbsent, nil
	}
	// A card lease keeps its record in e/<epoch>.json: look at that file itself.
	if serr := statRecord(epochRecordPath(m.leaseDir(), epoch)); serr == nil {
		if _, rerr := readEpochRecord(m.leaseDir(), epoch); rerr != nil {
			return RecordUnreadable, &RecordUnreadableError{Epoch: epoch, Err: rerr}
		}
		return RecordHeld, nil
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return RecordUnreadable, &RecordUnreadableError{Epoch: epoch, Err: serr}
	}
	// Else a whole-node lease: meta.json names the epoch. The epoch counter is shared, so an epoch that is not meta.json's is not a
	// whole-node lease; a meta.json that cannot be read cannot say, and so counts as holding this epoch.
	meta, err := m.readMeta()
	switch {
	case err == nil && meta.Epoch == epoch:
		return RecordHeld, nil
	case err == nil, errors.Is(err, fs.ErrNotExist):
		return RecordAbsent, nil
	case isIOFailure(err):
		return RecordUnreadable, &RecordUnreadableError{Epoch: epoch, Err: err}
	}
	return RecordAbsent, nil // corrupt bytes: debris that an acquirer sweeps, as every other reader treats it
}

// ClaimsOf lists the cards (lease ids) whose claim names this epoch. A card lease's release removes its record and then its claims;
// a release that removed the record and failed on a claim has left the lease half released, and the record being absent does not
// say so.
func (m *Manager) ClaimsOf(epoch uint64) []string {
	entries, err := os.ReadDir(m.cardsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".claim") {
			continue
		}
		device := strings.TrimSuffix(e.Name(), ".claim")
		if c, ok := readCardClaim(m.leaseDir(), device); ok && c.Epoch == epoch {
			out = append(out, device)
		}
	}
	return out
}

// ErrLeaseMovedOn is the refusal for releasing an epoch that is no longer the current lease.
type ErrLeaseMovedOn struct{ Asked, Current uint64 }

func (e *ErrLeaseMovedOn) Error() string {
	return fmt.Sprintf("gpulease: lease has moved on (asked for epoch %d, current is %d)", e.Asked, e.Current)
}

// IsLeaseMovedOn is true for the refusal to release an epoch that is no longer the current lease: the one release error that says
// the lease is NOT this caller's any more, so there is nothing of its own left to release.
func IsLeaseMovedOn(err error) bool {
	var e *ErrLeaseMovedOn
	return errors.As(err, &e)
}
