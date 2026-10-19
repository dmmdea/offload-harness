// pinline.go: a reasoned local pin is admitted to the local seat's line only while the line has room (ADR 0078 follow-up).
//
// A pin_reason makes route=local authoritative: the subtask runs on this box's seat and nowhere else, and it is never
// re-placed (isReplaceable keeps route=local in place). Until this file, authoritative also meant UNBOUNDED: every pinned
// subtask registered on the seat's run-cap line and waited there for its whole run wall. On 2026-10-19 a workflow's
// agents each passed route:"local", pin_reason:"operator" (no operator had named that placement) to offload_research;
// 36 runs registered on one seat, 184 subtasks deferred node_busy, the p50 latency was the 600 s wall, and four fleet
// nodes sat with free workers. The first incident of the shape (2026-10-07, pin.go) was a reasonless pin; the reason
// closed that door and a claimed reason re-opened it.
//
// Every fleet node already bounds its own line: it accepts at most fleet_max_queue_depth jobs, running and queued
// (FleetQueueLimit, default twice its workers), because a deeper backlog holds jobs it has no hope of starting inside
// any wall a caller would wait out (register S-04/C-25). The local seat's line now takes the same bound. A reasoned
// local pin that finds the line at that depth defers at once as capacity, and the reason says how deep the line is,
// which fleet nodes have a free worker right now, and that a call without the pin (route spread) would use them. The pin
// stays authoritative: the subtask still never runs anywhere else. What changes is that it answers in seconds instead
// of after the wall, and the line other sessions' work joins stays short.
//
// The registry count lags the subtasks this process has admitted but whose run has not registered yet (a fan-out of
// many calls admits them within milliseconds of each other), so the process also counts the pinned subtasks it admitted
// and has not finished, and the deeper of the two readings decides. Both fail open: an unreadable registry counts
// nothing (localRunCapRoom's own fail-open, logged there) and an unlimited queue limit admits everything.

package delegate

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/core"
)

// pinnedLocal counts the reasoned local-pinned subtasks this process admitted to the local seat and has not finished.
var pinnedLocal struct {
	mu sync.Mutex
	n  int
}

// admitPinnedLocal decides whether a reasoned route=local subtask may join the local seat's line. On admission it
// returns a release func the caller defers; on refusal it returns the defer to publish.
func (r *runner) admitPinnedLocal(ctx context.Context, localView NodeView) (release func(), refused *PlacedResult) {
	limit := r.cfg.FleetQueueLimit()
	if limit <= 0 {
		return func() {}, nil
	}
	ahead, lineNote := r.localRunLine()
	pinnedLocal.mu.Lock()
	depth := max(ahead, pinnedLocal.n)
	if depth < limit {
		pinnedLocal.n++
		pinnedLocal.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				pinnedLocal.mu.Lock()
				pinnedLocal.n--
				pinnedLocal.mu.Unlock()
			})
		}, nil
	}
	admitted := pinnedLocal.n
	pinnedLocal.mu.Unlock()

	free := r.freeFleetNodes(ctx)
	why := fmt.Sprintf("route=local pinned (pin_reason %s): the local seat's line is full — %s, %d pinned subtask(s) of this process admitted; the line takes at most %d (fleet_max_queue_depth, the same bound every fleet node applies to its own backlog), so this subtask would wait out its wall without starting",
		r.pin.reason, lineNote, admitted, limit)
	if len(free) > 0 {
		why += "; fleet nodes with a free worker right now: " + strings.Join(free, ", ") + " — drop route and pin_reason (route spread) to use them"
	} else {
		why += "; send fewer subtasks at once, or drop the pin so placement can use the fleet as it frees"
	}
	return nil, &PlacedResult{
		Node: localView.NodeID, Seat: localView.AgentSeat,
		Unplaced:        true,
		PlacementReason: "route=local pinned: local line full",
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassCapacity,
			Reason:        why,
		},
	}
}

// freeFleetNodes names the configured fleet nodes whose own numbers prove a free worker with nobody ahead
// (freeWorkerProven) and that serve an agent seat. A failed probe names nothing.
func (r *runner) freeFleetNodes(ctx context.Context) []string {
	if len(r.remotes) == 0 {
		return nil
	}
	views, _, _ := r.fetchViews(ctx)
	var out []string
	for _, v := range views {
		if v.AgentSeat == "" || !freeWorkerProven(v) {
			continue
		}
		out = append(out, v.NodeID)
	}
	return out
}

// CheckResearchPinReason refuses the pin reasons that cannot be true of a research call: its pages are public (the fetch
// refuses non-public hosts) and travel inline in the contract, so neither the material (privacy) nor the work (locality)
// is tied to this box. measurement and operator stay valid. Both research doors call it after CheckPinReason.
func CheckResearchPinReason(reason string) error {
	if reason == PinPrivacy || reason == PinLocality {
		return fmt.Errorf("pin_reason %s cannot apply to a research call: its pages are public and travel inline in the contract, so nothing ties them to this box; drop route and pin_reason (route spread) to use the fleet", reason)
	}
	return nil
}
