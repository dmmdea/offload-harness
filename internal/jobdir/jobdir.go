// Package jobdir is the ownership marker of a delegator-side job directory
// under <base>/pipeline-jobs/.
//
// That root has two kinds of writer. fleet-serve materializes the jobs it is
// dispatched there (agent-<n>, accel-<n> and pipeline job ids) and keeps the
// record of them in memory, so anything it finds on disk at startup is a
// previous instance's leftover. A delegator process does the same for its own
// in-process local placement (pipeline.RunAgentContract, reached from the MCP
// server, the delegate and research commands and the fleet smoke), and that
// process is not fleet-serve: it outlives fleet-serve restarts, so the
// directory of a local run in flight can be on disk at the moment fleet-serve
// starts.
//
// The marker tells the two apart. A delegator writes OwnerFile into its job dir
// with its own process id before it writes anything else; the startup sweep
// keeps a marked directory while that process is alive and the directory is
// younger than MaxRunLifetime, and removes it otherwise. The marker sits beside
// the directory's context/ folder and never inside it: the seat's read root is
// context/, and a write door's root is relative to that, so neither can name
// the marker.
//
// The package imports nothing but the standard library, so the writer
// (internal/pipeline) and the sweeper (internal/fleetnode) can both import it.
// The pipeline package imports fleetnode, which rules out the sweeper
// importing the writer's package, and liveness (gpulease.PIDAlive) stays the
// sweeper's call: one definition of "alive" across the harness.
package jobdir

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// OwnerFile is the marker's name, directly inside a job dir.
	OwnerFile = ".owner"

	// LocalRunPrefix is the name prefix of the directories a delegator mints for
	// its local runs (os.MkdirTemp pattern LocalRunPrefix+"*"). The writer and
	// the sweep share it so the two cannot drift apart: a directory with this
	// prefix and no usable marker is still a local run's, written by a binary
	// older than the marker or caught between its creation and its marker.
	LocalRunPrefix = "agent-local-"

	// MaxRunLifetime is the age past which a job directory is treated as a leak
	// whatever its owner's liveness says. It bounds the two ways an ownership
	// check can be wrong in the keep direction: a process id recycled by an
	// unrelated process after the real owner died, and a directory with no
	// marker at all, which has no owner to ask.
	//
	// It has to outlast the longest a LOCAL run can hold its directory, which is
	// the whole of pipeline.RunAgentContract (MkdirTemp to the deferred RemoveAll):
	//
	//	admission          300 s   core.AgentAdmissionSecDefault: the cordon, the
	//	                           pre-flight, the cold-load warm-up, the coherence
	//	                           and the window probes
	//	seat-cap line      900 s   modelaffinity.SeatCapDeadline: as long as the
	//	                           run's own wall, which the delegator doors clamp
	//	                           to core.AgentTimeoutSecCap
	//	                           (delegate.PrepareContract); the admission
	//	                           deadline moves out by the time spent in line,
	//	                           so both are paid in full
	//	loop + re-pack  14,400 s   the liveness safety ceiling, pipeline.CeilingFor,
	//	                           never above core.AgentCeilingSecCap (4 h)
	//	seat-pin probe       3 s   agent.ProbeSeatPin
	//	                  ------
	//	                  15,603 s   about 4.3 h, against 86,400 s here: 5.5 times
	//
	// Two inputs are not capped in code, and a run that outlives this on either
	// is treated as leaked: agent_admission_wait_sec is an operator setting with
	// no ceiling, and RunAgentContract does not clamp timeout_sec itself, so a
	// contract built without delegate.PrepareContract (agent_run with a route
	// builds its own) carries the caller's number into the seat-cap wait.
	MaxRunLifetime = 24 * time.Hour
)

// maxOwnerBytes bounds what ReadOwner reads: a process id is at most ten
// digits, so anything longer is not a marker.
const maxOwnerBytes = 64

// WriteOwner records the calling process as the owner of the job directory dir.
// Call it right after creating the directory and before anything else goes
// into it, so a sweep that lists the directory at any later instant can see
// whose it is. The marker is one short write, so a concurrent reader sees it
// whole or not yet at all (and ReadOwner treats a partial or empty one as
// unmarked, which a sweep resolves toward keeping).
func WriteOwner(dir string) error {
	return os.WriteFile(filepath.Join(dir, OwnerFile), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
}

// ReadOwner returns the process id recorded in dir's marker. ok is false for a
// missing, unreadable or garbled marker and for an id that is not a positive
// 31-bit number. !ok means only that the directory carries no usable owner; it
// never means the owner is dead, and a caller must not remove a directory on
// that evidence alone.
func ReadOwner(dir string) (pid int, ok bool) {
	f, err := os.Open(filepath.Join(dir, OwnerFile))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxOwnerBytes))
	if err != nil {
		return 0, false
	}
	// ParseUint, not Atoi: digits only (no sign, no underscore), and 31 bits so
	// the id survives the uint32 conversion a process probe applies on Windows
	// without wrapping around to a different process.
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 31)
	if err != nil || n == 0 {
		return 0, false
	}
	return int(n), true
}
