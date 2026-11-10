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

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// SeatColdLoadRefusal says why this node's host cannot take a cold load of its agent seat right now, or "" when it can. It
// answers "" for a model that is not the config's agent seat, for a node that states no footprint for its seat
// (agent_seat_host_ram_gib), and when m is nil. Two refusals, the warm-back's own (hostRAMWarmCheck) with no lease left out:
//   - a lane that declared host RAM is live: a seat is not loaded while a lane streams, because the lane's own growth is not
//     in any counter yet;
//   - the admission rule: the seat's footprint, taken for both quantities and with no commit stated, plus what the live leases
//     have yet to load, must leave the headroom of the available RAM and keep the commit charge under physical RAM less the
//     headroom.
func SeatColdLoadRefusal(cfg config.Config, m *gpulease.Manager, model string) string {
	if m == nil || !strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(cfg.AgentPlannerModel(""))) {
		return ""
	}
	need := hostneed.SeatNeed(cfg)
	if need.Source != hostneed.SourceSeat || need.GiB <= 0 {
		return ""
	}
	for _, l := range m.Leases() {
		if l.DeclaresHostRAM() {
			return fmt.Sprintf("a lane that declared %.1f GiB of host RAM is live (%s lease epoch %d), and a seat is not loaded while a lane streams", max(l.HostRAMGiB, l.HostRAMCommitGiB), l.Class, l.Epoch)
		}
	}
	if chk := m.HostRAMCheck(need.GiB); !chk.OK {
		return fmt.Sprintf("the host cannot take the seat's %.1f GiB (%s): %s", need.GiB, need.Source, chk.Why)
	}
	return ""
}

// SeatColdLoadRefusalFor is SeatColdLoadRefusal with the lease manager this config names (the state root and lease path it
// configures). A manager that cannot be opened refuses nothing: the gate is a guard on top of the load that always ran, and
// an unreadable lease directory is not evidence the host is short.
func SeatColdLoadRefusalFor(cfg config.Config, model string) string {
	m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		return ""
	}
	return SeatColdLoadRefusal(cfg, m, model)
}

// AwaitSeatAdmissionState is AwaitSeatAdmission that also reports what the pre-flight saw of the seat itself: when it first
// saw the seat loading (zero = never) and whether the seat read ready at the end. A door asks it so that it can tell a load
// it would START (not ready, none seen) from one already in flight or a seat already loaded.
func AwaitSeatAdmissionState(ctx context.Context, endpoint, seat string, budget time.Duration) (time.Duration, string, time.Time, bool) {
	return awaitSeatAdmissionLoad(ctx, endpoint, seat, budget)
}
