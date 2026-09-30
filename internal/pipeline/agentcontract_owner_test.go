package pipeline

// Register C-78: RunAgentContract (the delegator's in-process local placement)
// materializes its job dir in the same pipeline-jobs/ root fleet-serve sweeps
// at startup, and its process outlives fleet-serve restarts. The dir therefore
// has to say whose it is (the owner marker) for the sweep to leave a run that
// is still going alone. Reuses agenttask_test.go's scripted llama fake.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/jobdir"
)

// jobDirView is what a test sees of one local run's directory while the run is
// in flight.
type jobDirView struct {
	name         string
	ownerPID     int
	ownerOK      bool
	markerBeside bool // <dir>/.owner exists
	markerInRoot bool // <dir>/context/.owner exists: reachable from the seat's read root
}

// runProbe looks at pipeline-jobs/ from inside a run. The fake seat's handler
// runs on the test server's goroutine, so what it sees is handed back under a
// lock and asserted after the run returns, never with t.Fatal from the handler.
type runProbe struct {
	mu    sync.Mutex
	root  string
	views []jobDirView
}

func (p *runProbe) setRoot(root string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.root = root
}

// capture records every local-run dir present right now.
func (p *runProbe) capture() {
	p.mu.Lock()
	defer p.mu.Unlock()
	jobs := filepath.Join(p.root, "pipeline-jobs")
	entries, _ := os.ReadDir(jobs)
	p.views = nil
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), jobdir.LocalRunPrefix) {
			continue
		}
		d := filepath.Join(jobs, e.Name())
		pid, ok := jobdir.ReadOwner(d)
		_, errBeside := os.Stat(filepath.Join(d, jobdir.OwnerFile))
		_, errInRoot := os.Stat(filepath.Join(d, "context", jobdir.OwnerFile))
		p.views = append(p.views, jobDirView{
			name: e.Name(), ownerPID: pid, ownerOK: ok,
			markerBeside: errBeside == nil, markerInRoot: errInRoot == nil,
		})
	}
}

func (p *runProbe) seen() []jobDirView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]jobDirView(nil), p.views...)
}

// While a local run is in flight its job dir carries an owner marker holding
// this process's id, beside context/ and never inside it, and the marker goes
// away with the dir when the run ends.
func TestRunAgentContractMarksItsJobDirWhileTheRunIsInFlight(t *testing.T) {
	probe := &runProbe{}
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			probe.capture()
			return doneChat("The answer is 42.")
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	p, home := agentContractPipeline(t, srv.URL)
	probe.setRoot(home)
	contract := testContract()
	contract.Depth = 0
	contract.OutputSchema = nil // no re-pack: the loop's answer comes back as is
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}

	views := probe.seen()
	if len(views) != 1 {
		t.Fatalf("while the run was in flight pipeline-jobs held %d local-run dir(s), want exactly 1: %+v", len(views), views)
	}
	v := views[0]
	if !v.markerBeside {
		t.Errorf("%s carried no owner marker while its run was in flight: a startup sweep cannot tell it from a crash's leftover", v.name)
	}
	if !v.ownerOK || v.ownerPID != os.Getpid() {
		t.Errorf("%s owner = (%d, %v), want this process (%d, true)", v.name, v.ownerPID, v.ownerOK, os.Getpid())
	}
	if v.markerInRoot {
		t.Errorf("%s: the owner marker is inside context/, where the seat (and a write door) can reach it", v.name)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "pipeline-jobs")); len(entries) != 0 {
		t.Errorf("pipeline-jobs left %d entries behind; the marker must go with the dir when the run ends", len(entries))
	}
}

// readNotesChat is an assistant turn that calls read_file on the contract's
// context doc.
func readNotesChat() string {
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"notes.md\"}"}}]},"finish_reason":"tool_calls"}]}`
}

// toolObservations is everything the loop has fed back as tool results so far,
// read from the chat request the seat was just sent.
func toolObservations(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	var b strings.Builder
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m["role"] != "tool" {
			continue
		}
		s, _ := m["content"].(string)
		b.WriteString(s)
		b.WriteByte('\n')
	}
	return b.String()
}

// The incident, end to end: fleet-serve restarts on a box while a local run is
// in flight there. Its startup sweep runs against the same base dir in the
// middle of the run, and the run must still read its context doc afterwards
// (before the marker it failed with "workspace root unavailable" and the seat
// answered "I could not read the file"). fleet-serve's own leftover next to it
// is still reclaimed.
func TestRunAgentContractSurvivesAStartupSweepMidRun(t *testing.T) {
	var (
		mu          sync.Mutex
		home        string
		swept, kept = -1, -1
		sweepErr    error
		observation string
	)
	fake := &agentFake{rosterIDs: []string{agentTestSeat}}
	fake.loopStream = func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			mu.Lock()
			h := home
			mu.Unlock()
			s, k, err := fleetnode.SweepOrphanedPipelineJobs(config.Config{Home: h})
			mu.Lock()
			swept, kept, sweepErr = s, k, err
			mu.Unlock()
			_, _ = w.Write([]byte(readNotesChat()))
			return
		}
		mu.Lock()
		observation = toolObservations(body)
		mu.Unlock()
		_, _ = w.Write([]byte(doneChat("The answer is 42.")))
	}
	srv := fake.server(t)
	defer srv.Close()

	p, h := agentContractPipeline(t, srv.URL)
	mu.Lock()
	home = h
	mu.Unlock()
	// What a crashed fleet-serve instance left behind: no marker, not a local run's.
	leftover := filepath.Join(h, "pipeline-jobs", "agent-99")
	if err := os.MkdirAll(filepath.Join(leftover, "context"), 0o755); err != nil {
		t.Fatal(err)
	}

	contract := testContract()
	contract.Depth = 0
	contract.OutputSchema = nil
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sweepErr != nil {
		t.Fatalf("the sweep failed: %v", sweepErr)
	}
	if strings.Contains(observation, "workspace root unavailable") {
		t.Errorf("the sweep took the run's context dir out from under it; the seat's read_file saw: %s", strings.TrimSpace(observation))
	}
	if !strings.Contains(observation, "the answer is 42") {
		t.Errorf("the run could not read its context doc after the sweep; the seat's read_file saw: %q", strings.TrimSpace(observation))
	}
	if wire.Deferred || wire.Steps != 2 {
		t.Errorf("deferred=%v steps=%d reason=%q, want a finished two-step run (read_file, then the answer)", wire.Deferred, wire.Steps, wire.Reason)
	}
	if swept != 1 || kept != 1 {
		t.Errorf("the mid-run sweep reported swept/kept = %d/%d, want 1/1 (the old leftover reclaimed, the live run kept)", swept, kept)
	}
	if _, serr := os.Stat(leftover); !os.IsNotExist(serr) {
		t.Errorf("fleet-serve's own leftover survived the sweep: %v", serr)
	}
	if entries, _ := os.ReadDir(filepath.Join(h, "pipeline-jobs")); len(entries) != 0 {
		t.Errorf("pipeline-jobs left %d entries behind after the run", len(entries))
	}
}

// The marker is the first thing in the dir: nothing else is in it when it is
// written, so there is no instant at which a sweep can list a dir that already
// holds context docs but no marker.
func TestRunAgentContractWritesTheOwnerMarkerBeforeAnythingElse(t *testing.T) {
	orig := writeJobOwner
	t.Cleanup(func() { writeJobOwner = orig })
	var present []string
	writeJobOwner = func(dir string) error {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			present = append(present, e.Name())
		}
		return orig(dir)
	}
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
	}
	srv := fake.server(t)
	defer srv.Close()

	p, _ := agentContractPipeline(t, srv.URL)
	contract := testContract()
	contract.Depth = 0
	contract.OutputSchema = nil
	if _, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{}); err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if len(present) != 0 {
		t.Fatalf("the job dir already held %v when its owner marker was written; the marker must come first", present)
	}
}

// A marker that cannot be written is a materialization failure like the
// others: an error, no model call, and nothing left on disk.
func TestRunAgentContractMarkerWriteFailureIsAnErrorAndLeavesNothing(t *testing.T) {
	orig := writeJobOwner
	t.Cleanup(func() { writeJobOwner = orig })
	writeJobOwner = func(string) error { return errors.New("disk full") }
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			t.Error("the seat was called for a run that could not be marked")
			return doneChat("never")
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	p, home := agentContractPipeline(t, srv.URL)
	contract := testContract()
	contract.Depth = 0
	_, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err == nil {
		t.Fatal("a run whose dir could not be marked must not start")
	}
	if !strings.Contains(err.Error(), "owner marker") || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q, want it to name the marker and carry the cause", err)
	}
	if fake.loopCalls.Load() != 0 {
		t.Errorf("the seat was called %d time(s) for a run that could not be marked", fake.loopCalls.Load())
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "pipeline-jobs")); len(entries) != 0 {
		t.Errorf("a run that could not be marked left %d entries behind", len(entries))
	}
}
