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

// heldGiB is what the processes under a lease holder hold privately right now, and whether that
// could be read.
func (m *Manager) heldGiB(holderPID int) (float64, bool) {
	if m.workload != nil {
		return m.workload(holderPID)
	}
	return descendantsPrivateGiB(holderPID)
}

// descendantsPrivateGiB sums the private memory of every process below holderPID, not the holder
// itself. A wrapper lease's holder is `gpu reserve`, a few MiB, and the job is below it; a pipeline
// lease's holder is the long-lived server, whose own memory is not the job's and must not be
// subtracted from it. A process whose memory cannot be read (another security context) counts 0,
// which leaves more of the declared need pending: the conservative reading.
func descendantsPrivateGiB(holderPID int) (float64, bool) {
	tree, err := ProcessTree(holderPID)
	if err != nil {
		return 0, false
	}
	var bytes uint64
	for _, p := range tree {
		if p.PID == holderPID {
			continue
		}
		if n, ok := privateBytes(p.PID); ok {
			bytes += n
		}
	}
	return float64(bytes) / (1 << 30), true
}

// pendingGiB is the part of the declared needs of leases ALREADY GRANTED that has not loaded yet:
// the memory those jobs are about to commit that the commit charge does not include. Leases that
// share a holder pid are pooled (the pipeline holds one lease per card in one server process, and
// their runners are all below it, so a lease-by-lease subtraction would credit each lease with the
// others' memory and count nothing pending): declared minus held, per holder, never below zero.
// held reports a holder's current private bytes; a holder it cannot read holds nothing, so its whole
// declared need is pending.
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

// hostRAMRefusal applies the rule to a grant that declares opts.HostRAMGiB, given the leases live
// at the moment (the caller reads them inside its critical section). nil admits. A lease that
// declares nothing is never read against the host: the memory is not touched, so a box that is
// already over is not made to wait for a job that adds nothing to it.
func (m *Manager) hostRAMRefusal(opts Options, live []Info) *ErrHostRAM {
	if opts.HostRAMGiB <= 0 {
		return nil
	}
	mem, ok := m.hostMemory()
	var pending float64
	if len(live) > 0 {
		pending = pendingGiB(live, m.heldGiB)
	}
	chk := gpuprobe.HostRAMAdmits(mem, ok, opts.HostRAMGiB, pending, m.HostRAMHeadroom())
	if chk.OK {
		return nil
	}
	return &ErrHostRAM{chk}
}
