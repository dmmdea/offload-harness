package pipeline

// Register C-79: a run whose workspace root vanished defers as infrastructure.
// When the job directory is taken under a run (the C-78 shape, or any other loss
// of it) every file tool answers "workspace root unavailable". The seat used to be
// handed that as an observation, wrote a final ("I could not read the file"), and
// the delegator scored it failed_verification: the box's fault filed as the
// model's, and never re-placed. These tests drive the whole node-side path with a
// scripted seat, on both doors that run it: the delegator's in-process local
// placement (RunAgentContract) and a fleet node's job (Pipeline.Run over a
// materialized context dir).

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/jobdir"
)

// requireLostRootDefer is what every door must publish for a run whose root went:
// a defer (a success shape at the job level), class infrastructure, a reason that
// opens with core.RootGoneReason and names the root, the delegator's own recognition
// of it (so it is re-placed), and neither an answer nor a write set.
func requireLostRootDefer(t *testing.T, wire core.AgentWireResult, root string) {
	t.Helper()
	if !wire.Deferred {
		t.Fatalf("a run whose root vanished answered instead of deferring: output %q, stop_reason %q, steps %d", wire.Output, wire.StopReason, wire.Steps)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Errorf("defer_class = %q (reason %q), want %q: the box lost the root, the model did not fail", wire.DeferClass, wire.Reason, core.DeferClassInfrastructure)
	}
	if !strings.HasPrefix(wire.Reason, core.RootGoneReason) {
		t.Errorf("reason = %q, want it to open with %q", wire.Reason, core.RootGoneReason)
	}
	if root == "" || !strings.Contains(wire.Reason, root) {
		t.Errorf("reason = %q, want it to name the root %q", wire.Reason, root)
	}
	if !delegate.RootGoneDefer(wire) {
		t.Errorf("the delegator does not recognise this defer as the lost-root one (class %q, reason %q), so it would not be re-placed", wire.DeferClass, wire.Reason)
	}
	if wire.Output != "" || wire.Structured != nil {
		t.Errorf("a deferred run published an answer: output %q structured %s", wire.Output, wire.Structured)
	}
	if wire.Diff != "" || len(wire.DiffFiles) != 0 {
		t.Errorf("a run whose root vanished published a write set: diff=%q files=%v", wire.Diff, wire.DiffFiles)
	}
}

// requireRootGoneDefer is requireLostRootDefer for a run that ENDED ON a tool's report of
// the lost root: the seat was asked exactly once (the one turn whose tool call found the
// root gone), after one step, and the trace carries the failed call.
func requireRootGoneDefer(t *testing.T, wire core.AgentWireResult, fake *agentFake, root string) {
	t.Helper()
	requireLostRootDefer(t, wire, root)
	if n := fake.loopCalls.Load(); n != 1 {
		t.Errorf("the seat was asked %d times, want 1: the run must end on the tool's report, not give the seat a turn to answer from nothing", n)
	}
	if wire.Steps != 1 {
		t.Errorf("steps = %d, want 1", wire.Steps)
	}
	if len(wire.Trace) != 1 || wire.Trace[0].Status != "failed" || !strings.Contains(wire.Trace[0].Note, "workspace root unavailable") {
		t.Errorf("trace = %+v, want the one failed call with the tool's own words, so the operator sees what the seat saw", wire.Trace)
	}
}

// The incident, end to end, on the delegator's local placement: the job directory
// goes while the seat decides what to read (what fleet-serve's startup sweep did
// before C-78, what an indexer or a cleaner can still do), the read finds the
// root gone, and the run defers instead of answering.
func TestRunAgentContractDefersAsInfrastructureWhenItsJobDirVanishesMidRun(t *testing.T) {
	var (
		mu     sync.Mutex
		home   string
		ctxDir string
	)
	fake := &agentFake{rosterIDs: []string{agentTestSeat}}
	fake.loopStream = func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			mu.Lock()
			jobs := filepath.Join(home, "pipeline-jobs")
			mu.Unlock()
			entries, _ := os.ReadDir(jobs)
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), jobdir.LocalRunPrefix) {
					continue
				}
				mu.Lock()
				ctxDir = filepath.Join(jobs, e.Name(), "context")
				mu.Unlock()
				_ = os.RemoveAll(filepath.Join(jobs, e.Name()))
			}
			_, _ = w.Write([]byte(readNotesChat()))
			return
		}
		_, _ = w.Write([]byte(doneChat("I could not read the file.")))
	}
	srv := fake.server(t)
	defer srv.Close()

	p, h := agentContractPipeline(t, srv.URL)
	mu.Lock()
	home = h
	mu.Unlock()
	contract := testContract()
	contract.Depth = 0
	contract.OutputSchema = nil
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v: a defer is a success shape, never an error", err)
	}
	mu.Lock()
	root := ctxDir
	mu.Unlock()
	requireRootGoneDefer(t, wire, fake, root)
	if entries, _ := os.ReadDir(filepath.Join(h, "pipeline-jobs")); len(entries) != 0 {
		t.Errorf("pipeline-jobs holds %d entries after the run", len(entries))
	}
}

// The fleet node's job runs the same function over a context dir its own
// buildAgentRun materialized: the node-side defer is a job-level SUCCESS (the job
// lands done, never error) so the delegator reads the class and the reason. The
// contract carries an output_schema, as every remote one does, so before the fix
// the seat's "I could not read the file" was re-packed into the schema and went
// back as an answer.
func TestRunAgentTaskDefersAsInfrastructureWhenItsContextDirVanishesMidRun(t *testing.T) {
	req := agentTestRequest(t, testContract())
	ctxDir, _ := req.Params["context_dir"].(string)
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		repack:    func(int64) string { return `{"answer":"I could not read the file"}` },
	}
	fake.loopStream = func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = os.RemoveAll(ctxDir)
			_, _ = w.Write([]byte(readNotesChat()))
			return
		}
		_, _ = w.Write([]byte(doneChat("I could not read the file.")))
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), req))
	requireRootGoneDefer(t, wire, fake, ctxDir)
}

// The write door's root lives inside the read root, so a run on the write door
// loses both. The write set is then not a change the caller can review: the door
// publishes no diff for it (a snapshot of a tree that is not there reads as every
// file deleted) and the class stays infrastructure, not `write`.
func TestRunAgentTaskRootGoneOutranksTheWriteDoor(t *testing.T) {
	contract := writeContract(".")
	contract.OutputSchema = nil
	req := agentTestRequest(t, contract)
	ctxDir, _ := req.Params["context_dir"].(string)
	fake := &agentFake{rosterIDs: []string{agentTestSeat}}
	fake.loopStream = func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = os.RemoveAll(ctxDir)
			_, _ = w.Write([]byte(editCallChat("e1", "util.go", "\treturn n\n", "\treturn n - 1\n")))
			return
		}
		_, _ = w.Write([]byte(doneChat("Fixed it.")))
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), req))
	if wire.Diff != "" || len(wire.DiffFiles) != 0 {
		t.Errorf("a run whose root vanished published a write set: diff=%q files=%v", wire.Diff, wire.DiffFiles)
	}
	if wire.DeferClass == core.DeferClassWrite {
		t.Errorf("the defer was filed as class %q (reason %q); the write door did not fail, the box lost the tree", wire.DeferClass, wire.Reason)
	}
	requireRootGoneDefer(t, wire, fake, ctxDir)
}

// The signal is the tool's report. A run that read its documents and then lost the
// root, with no tool called again, finished: the answer stands and nothing defers.
func TestRunAgentTaskAFinishedRunWhoseRootVanishedAfterItsLastReadStillAnswers(t *testing.T) {
	req := agentTestRequest(t, testContract())
	ctxDir, _ := req.Params["context_dir"].(string)
	contract, _ := req.Params["contract"].(core.AgentContract)
	contract.OutputSchema = nil
	req.Params["contract"] = contract
	fake := &agentFake{rosterIDs: []string{agentTestSeat}}
	fake.loopStream = func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(readNotesChat()))
			return
		}
		_ = os.RemoveAll(ctxDir) // after the read, before the answer
		_, _ = w.Write([]byte(doneChat("The answer is 42.")))
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), req))
	if wire.Deferred || wire.Output != "The answer is 42." {
		t.Fatalf("deferred=%v output=%q reason=%q, want the finished answer", wire.Deferred, wire.Output, wire.Reason)
	}
}

// The write door's deliverable is a diff of the tree, which needs the tree. A run that
// made its edit and lost the root before it answered has no tool call left to report the
// loss, and for a read-only contract its answer stands (the test above). Closing the door
// on that tree is another matter: a snapshot of a directory that is not there reads as
// every file deleted, a diff the caller is invited to apply. The run defers as
// infrastructure with no diff, and the delegator re-places it.
func TestRunAgentTaskAWriteDoorRunWhoseRootVanishedAfterItsLastToolCallDefersWithoutADiff(t *testing.T) {
	contract := writeContract(".")
	contract.OutputSchema = nil
	req := agentTestRequest(t, contract)
	ctxDir, _ := req.Params["context_dir"].(string)
	fake := &agentFake{rosterIDs: []string{agentTestSeat}}
	fake.loopStream = func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(editCallChat("e1", "util.go", "\treturn n\n", "\treturn n - 1\n")))
			return
		}
		_ = os.RemoveAll(ctxDir) // after the edit landed, before the answer; no tool is called again
		_, _ = w.Write([]byte(doneChat("Fixed it.")))
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), req))
	if wire.Diff != "" || len(wire.DiffFiles) != 0 {
		t.Errorf("a run whose root vanished published a write set: diff=%q files=%v", wire.Diff, wire.DiffFiles)
	}
	if wire.DeferClass == core.DeferClassWrite {
		t.Errorf("the defer was filed as class %q (reason %q); the write door did not fail, the box lost the tree", wire.DeferClass, wire.Reason)
	}
	requireLostRootDefer(t, wire, ctxDir)
	if n := fake.loopCalls.Load(); n != 2 {
		t.Errorf("the seat was asked %d times, want 2: the run finished (edit, then answer) and the loss was found when the door closed", n)
	}
}

// The same loss before the door opens. A run waits for its seat (a cold load can take
// minutes) between materializing its context dir and opening the door, and a sweep can
// take the dir in that window. The door's own opening then fails on the read root, which
// was filed as a `write` defer: the class that says another node may have opted in, and
// one the delegator does not re-place. The box lost the tree, and the seat is never asked.
func TestRunAgentTaskAWriteDoorRunWhoseRootIsGoneBeforeTheDoorOpensDefersAsInfrastructure(t *testing.T) {
	contract := writeContract(".")
	contract.OutputSchema = nil
	req := agentTestRequest(t, contract)
	ctxDir, _ := req.Params["context_dir"].(string)
	if err := os.RemoveAll(ctxDir); err != nil {
		t.Fatal(err)
	}
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			t.Error("the seat was asked to work on a tree that is not there")
			return doneChat("x")
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, writeDoorPipeline(t, srv.URL, true).Run(context.Background(), req))
	if wire.DeferClass == core.DeferClassWrite {
		t.Errorf("the defer was filed as class %q (reason %q); the write door did not fail, the box lost the tree", wire.DeferClass, wire.Reason)
	}
	requireLostRootDefer(t, wire, ctxDir)
	if n := fake.loopCalls.Load(); n != 0 {
		t.Errorf("the seat was asked %d times, want 0", n)
	}
}
