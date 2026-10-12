package delegate

import (
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// RootGoneDefer reports whether a result is the lost-root defer (register C-79): an
// `infrastructure` defer whose reason carries core.RootGoneReason, i.e. the run's
// own file tools could not open the workspace root the executing node materialized
// for it, and the loop ended the run instead of letting the seat answer from
// nothing.
//
// It is the FOURTH infrastructure defer worth a retry, after the two
// admission-time ones (IncoherentSeatDefer, SeatWarmFailedDefer) and the seat-down
// defer (SeatDownDefer), and for the same kind of reason: the fault is a property
// of THAT run's directory, the contract is sound, and the same contract placed on
// another node materializes a root of its own. It is safe to re-place because a
// node-filed defer is an OBSERVED terminal: the node reported it, so nothing is
// still running the contract.
//
// It matches on the CONSTANT the producer writes (pipeline.rootGoneReason builds
// the reason from core.RootGoneReason), never on prose, and on the prefix only: the
// same words in another class, or in the middle of another reason, are not it. A
// node without C-79 never emits it and keeps filing the same fault as the seat's
// failed answer, which is retried as a verification failure as before.
func RootGoneDefer(r core.AgentWireResult) bool {
	return r.Deferred && r.DeferClass == core.DeferClassInfrastructure &&
		strings.HasPrefix(r.Reason, core.RootGoneReason)
}
