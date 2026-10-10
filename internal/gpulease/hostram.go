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
//     the first one's record.
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

import (
	"math"
	"sort"
	"sync/atomic"

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
// held reports what the processes below a holder hold RESIDENT now (descendantsResidentGiB), the unit a
// declaration is in; a holder it cannot read holds nothing, so its whole declared need is pending.
func pendingGiB(live []Info, held func(holderPID int) (float64, bool)) float64 {
	declared := map[int]float64{}
	for _, l := range live {
		if l.HostRAMGiB > 0 {
			declared[l.PID] += l.HostRAMGiB
		}
	}
	pids := make([]int, 0, len(declared))
	for pid := range declared {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	var pending float64
	for _, pid := range pids {
		have := 0.0
		if pid > 0 {
			if g, ok := held(pid); ok {
				have = g
			}
		}
		if rest := declared[pid] - have; rest > 0 {
			pending += rest
		}
	}
	return pending
}

// HostRAMPending is pendingGiB over the leases live right now, for the readers that apply the same
// rule before a grant (the card allocator's pre-filter) or report it (status).
func (m *Manager) HostRAMPending() float64 { return pendingGiB(m.Leases(), m.heldGiB) }

// DeclaredHostRAMGiB is the sum of the host RAM the live leases declared.
func DeclaredHostRAMGiB(leases []Info) float64 {
	var sum float64
	for _, l := range leases {
		sum += l.HostRAMGiB
	}
	return sum
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
		pending = pendingGiB(live, m.heldGiB)
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
