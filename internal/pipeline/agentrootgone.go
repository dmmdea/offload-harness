package pipeline

import (
	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// rootGoneReason is the defer reason for a run whose workspace root could not be opened
// (register C-79), whoever asked: a file tool mid-run, the replay of its setup reads, or
// its write door where no tool was there to ask. It opens with core.RootGoneReason, the
// constant the delegator matches on (delegate.RootGoneDefer), and names the root itself
// with the operating system's own words for the failure, so an operator can tell a
// swept job directory from a permission or a disk fault without a log.
//
// It says what happened to the run and whose fault it is: the run was stopped, or its
// result withheld, instead of going on without its directory, and nothing about the
// contract or the model was learned, which is the meaning of the `infrastructure` class
// it is filed under. The tail holds wherever the loss was found: a run stopped by a
// tool's report, or a finished run whose answer and diff are withheld because the tree
// they came from is gone.
func rootGoneReason(g *agent.RootGoneError) string {
	return core.RootGoneReason + g.Root + ": " + g.Cause() +
		" - the run's directory could not be opened, so the run was stopped, or its result withheld, instead of going on without it; nothing about the contract was learned"
}
