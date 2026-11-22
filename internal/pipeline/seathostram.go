package pipeline

// The host-RAM gate on a contract's cold load of the agent seat (2026-11-10).
//
// A lease release that would warm the seat back has asked the host first since B-56 (gpu_drain.go hostRAMWarmCheck): the
// seat's footprint (agent_seat_host_ram_gib), plus what the live leases have yet to load, must fit, and no lane that declared
// host RAM may be live. A contract that found the seat cold never asked: its warm-up (D-64) loaded the seat whatever the host
// read, and the warm-back's own refusal said so ("the seat loads on its next request"). On the 3-card reference box, with about
// 80 GiB already committed by the desktop and its sessions, one contract's cold load of the vLLM seat took the commit charge to
// 152.0 of 153.8 GiB; the page file had grown 7 GiB to make room, and the box was 1.8 GiB from refusing allocations.
//
// Both contract doors (runAgentTask and the MCP agent_run door) now put the warm-back's rule to a load they would START, before
// they start it, and defer as re-placeable when it does not fit, so the delegator places the contract on another node instead.
// A load already in flight (someone else's request started it) is not this contract's to refuse, and a seat that is already
// loaded costs nothing. A node that states no footprint for its seat keeps the old behaviour: the warm-back's fail-closed
// default (hostneed.DefaultSeatHostGiB) is a chosen figure, and refusing every contract on a node that never measured its seat
// would trade a measured hazard for an invented one.
//
// The gate is not one look. Right after another model unloads, the host's memory takes up to about a minute to come back, and a
// contract that arrived in that window was refused for a load that would fit seconds later. The door now settles
// (gpulease.Manager.HostRAMSettle): it re-reads the host every couple of seconds, asks the installed relief (the WSL page-cache
// drop, when the node enables it) on each refusal, and gives up when the host stops giving memory back or the bound is spent.
// The warm-back at a lease release does not wait (gpu_drain.go): the lease it follows is still held while it asks.
// A node that states the seat's commit (agent_seat_host_commit_gib) has it judged against the OS commit limit, not physical RAM.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// SeatColdLoadAdmission says whether this node's host can take a cold load of its agent seat, waiting for it when it cannot yet.
// why is "" when it can and the refusal when it cannot; waited is the time spent waiting for the host (count it as admission, not
// as the seat's load time); note says so in a line for the admission note when the wait was worth saying. It answers "" for a
// model that is not the config's agent seat, for a node that states no footprint for its seat (agent_seat_host_ram_gib), and when
// m is nil. Two refusals, the warm-back's own (hostRAMWarmCheck) with no lease left out:
//   - a lane that declared host RAM is live: a seat is not loaded while a lane streams, because the lane's own growth is not
//     in any counter yet. Immediate: a lane that streams does not end in a minute;
//   - the admission rule: the seat's footprint, plus what the live leases have yet to load, must leave the headroom of the
//     available RAM and keep the commit charge under its bound (physical RAM less the headroom when the node states no commit,
//     the OS commit limit less the headroom when it does). This one is re-read for up to min(gpulease.HostRAMSettleBound, budget).
func SeatColdLoadAdmission(ctx context.Context, cfg config.Config, m *gpulease.Manager, model string, budget time.Duration) (why string, waited time.Duration, note string) {
	if m == nil || !strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(cfg.AgentPlannerModel(""))) {
		return "", 0, ""
	}
	need := hostneed.SeatNeed(cfg)
	if need.Source != hostneed.SourceSeat || need.GiB <= 0 {
		return "", 0, ""
	}
	lane := func() string {
		for _, l := range m.Leases() {
			if l.DeclaresHostRAM() {
				return fmt.Sprintf("a lane that declared %.1f GiB of host RAM is live (%s lease epoch %d), and a seat is not loaded while a lane streams", max(l.HostRAMGiB, l.HostRAMCommitGiB), l.Class, l.Epoch)
			}
		}
		return ""
	}
	if why := lane(); why != "" {
		return why, 0, ""
	}
	load := need.HostLoad()
	chk, waited := m.HostRAMSettle(ctx, min(gpulease.HostRAMSettleBound, budget), func() gpuprobe.HostRAMCheck { return m.HostRAMCheckLoad(load) })
	if chk.OK {
		// A lane granted while the gate waited is judged like one that was live when it started.
		if waited > 0 {
			if why := lane(); why != "" {
				return why, waited, ""
			}
		}
		if waited >= time.Second {
			note = fmt.Sprintf("waited %s for host RAM before the seat's cold load", waited.Round(time.Second))
		}
		return "", waited, note
	}
	why = need.SeatRefusal(chk.Why)
	if waited >= time.Second {
		why += fmt.Sprintf(" (after %s waiting for the host to give memory back)", waited.Round(time.Second))
	}
	return why, waited, ""
}

// SeatColdLoadAdmissionFor is SeatColdLoadAdmission with the lease manager this config names (the state root and lease path it
// configures). A manager that cannot be opened refuses nothing: the gate is a guard on top of the load that always ran, and
// an unreadable lease directory is not evidence the host is short.
func SeatColdLoadAdmissionFor(ctx context.Context, cfg config.Config, model string, budget time.Duration) (why string, waited time.Duration, note string) {
	m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		return "", 0, ""
	}
	return SeatColdLoadAdmission(ctx, cfg, m, model, budget)
}

// AwaitSeatAdmissionState is AwaitSeatAdmission that also reports what the pre-flight saw of the seat itself: when it first
// saw the seat loading (zero = never), whether the seat read ready at the end, and whether the last reading established that
// the seat has no row in /running under any of its names (absent). A door asks it so that it can tell a load it would START
// (absent, none seen) from one already in flight, a seat already loaded, and a seat whose state could not be told. Only the
// first is put to SeatColdLoadAdmission: the names a seat is bound by (an alias) and listed by (its canonical id) differ on
// every box that binds seats by alias, and "not seen ready" refused loaded seats there.
func AwaitSeatAdmissionState(ctx context.Context, endpoint, seat string, budget time.Duration) (waited time.Duration, note string, loadSeen time.Time, ready, absent bool) {
	return awaitSeatAdmissionLoad(ctx, endpoint, seat, budget)
}
