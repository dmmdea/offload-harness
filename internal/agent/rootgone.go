package agent

import (
	"errors"
	"io/fs"
	"os"

	"github.com/dmmdea/offload-harness/internal/core"
)

// RootGoneError is a file tool's report that the run's workspace root could not
// be opened (register C-79). Every tool that reads or writes the workspace opens
// the root first (scope.open, update_plan), so the directory the run was built
// over being gone, or unreadable, is the one failure every one of them shares.
//
// It is the box's fault and no model action can undo it, which is why it is a
// type and not a sentence in a tool result: the loop used to feed it back as an
// ordinary error observation, the seat then wrote a final answer from nothing
// ("I could not read the file"), and the run was scored as the model's failure.
// The loop now ends the run on it (Loop.Run returns it) and the node files the
// defer as `infrastructure` (pipeline.runAgentTask).
//
// A file that is missing INSIDE a root that opens is not this: that is the
// model's mistake or the task's, and stays an observation.
type RootGoneError struct {
	Root string // the absolute workspace root the tool tried to open
	Err  error  // what the operating system answered
}

// Error is the text the seat always saw for this failure, byte for byte, so a
// transcript, an effect note and a log line read the same before and after the
// loop learned to end the run on it. The path is in Err's own text.
func (e *RootGoneError) Error() string { return core.RootGoneReason + e.Err.Error() }

func (e *RootGoneError) Unwrap() error { return e.Err }

// Cause is the operating system's own words for the failure without the path in
// front of them ("no such file or directory"), for a sentence that names the
// root itself and would otherwise print it twice.
func (e *RootGoneError) Cause() string {
	var pe *fs.PathError
	if errors.As(e.Err, &pe) && pe.Err != nil {
		return pe.Err.Error()
	}
	return e.Err.Error()
}

// RootGoneOf is the *RootGoneError an error is, or carries, and nil when it is
// neither: the one question the loop asks of a failed tool call, and the node asks
// of the error a run ended on.
func RootGoneOf(err error) *RootGoneError {
	var gone *RootGoneError
	if errors.As(err, &gone) {
		return gone
	}
	return nil
}

// CheckWorkspaceRoot is the tools' own question asked without a tool call: can the run's
// workspace root still be opened? It is nil when it can, and otherwise the *RootGoneError a
// file tool would have answered with. The pipeline asks it where no tool is there to ask
// (register C-79): the write door's tree is the deliverable, so a tree lost before the door
// opens, or after the seat's last call, would otherwise surface as a write fault, or as a
// snapshot of a directory that is not there, which reads as every file deleted.
//
// It goes through openWorkspaceRoot, the one place a failure to open becomes a
// *RootGoneError, so its answer is a tool's answer byte for byte. It closes the handle it
// opened: an open handle would hold the directory against its removal on Windows.
func CheckWorkspaceRoot(root string) *RootGoneError {
	r, err := openWorkspaceRoot(root)
	if err != nil {
		return RootGoneOf(err)
	}
	_ = r.Close()
	return nil
}

// openWorkspaceRoot opens the os.Root handle a tool confines itself to, and is
// the ONE place a failure to open it becomes a *RootGoneError: any failure (the
// directory is gone, is no longer a directory, or cannot be read) means the same
// to the run, because the root is the harness's own and nothing the model does
// can bring it back.
func openWorkspaceRoot(root string) (*os.Root, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, &RootGoneError{Root: root, Err: err}
	}
	return r, nil
}
