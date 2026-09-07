package fleetnode

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"testing"
)

// TestProductionCodeNeverAdmitsWithABareSpec is the guard for the 2026-09-27
// defect class: the pull-queue claim loop called Jobs.Accept, i.e. an empty
// AcceptSpec, while the push path filled one. Every field it dropped had a
// consequence, and the worst was security-shaped — an unset Agent marker took
// a PULLED agent job out of handleJob's bearer gate, so its result was
// readable without the token while the identical contract arriving by dispatch
// was gated.
//
// Accept/AcceptAgent stay for TESTS, where a terse admission is the point and
// no wire authority depends on the spec. This test keeps them out of the
// production files, so the next admission path has to state its spec.
func TestProductionCodeNeverAdmitsWithABareSpec(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Two shapes of the same bug: a call to the terse helpers (jobs.Accept(,
	// s.jobs.Accept(, j.AcceptAgent(), and an Admit handed a literal empty
	// spec, which drops exactly the same fields with more typing.
	bare := regexp.MustCompile(`\.\s*Accept(Agent)?\s*\(`)
	emptySpec := regexp.MustCompile(`Admit\([^)]*AcceptSpec\{\s*\}`)
	// The methods' own declarations are not calls.
	decl := regexp.MustCompile(`func \(j \*Jobs\) Accept(Agent)?\(`)
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, line := range strings.Split(string(b), "\n") {
			// jobs.go DEFINES the terse helpers, so its own two one-line
			// bodies (`return j.Admit(id, AcceptSpec{}, run)`) are the
			// implementation, not a caller. Everywhere else, both shapes are
			// the bug.
			offending := bare.MatchString(line) || (name != "jobs.go" && emptySpec.MatchString(line))
			if decl.MatchString(line) || !offending {
				continue
			}
			t.Errorf("%s:%d admits with a bare spec: %s\n\tuse Jobs.Admit with an explicit AcceptSpec — Agent gates the bearer check on the job's result, Uncapped decides whether it burns a concurrency slot, OnDropped is the only cleanup a never-started job gets",
				name, i+1, strings.TrimSpace(line))
		}
	}
	if checked == 0 {
		t.Fatal("scanned no production files — the guard would pass vacuously")
	}
}

// TestClaimSpecMirrorsDispatch checks the pull path's spec by BEHAVIOUR: every
// field the bare Accept used to drop is set, and set the same way the dispatch
// handler sets it.
func TestClaimSpecMirrorsDispatch(t *testing.T) {
	// AgentModel so the resolved seat is non-empty, as it is on any node whose
	// agent lane is admissible at all.
	s, _ := newTestServer(t, config.Config{AgentModel: "qwen3.5-4b-vllm"}, &fakeRunner{}, nil)

	dropped := 0
	agent := s.claimSpec(string(core.TaskAgentRun), func() { dropped++ })
	if !agent.Agent {
		t.Fatal("a pulled AGENT job must carry the Agent marker — it is what bearer-gates the job's result on /fleet/jobs/{id}")
	}
	if agent.Uncapped {
		t.Fatal("an agent job is capped: it uses the shared text endpoint")
	}
	if agent.Task != string(core.TaskAgentRun) {
		t.Fatalf("Task = %q", agent.Task)
	}
	if agent.Model == "" {
		t.Fatal("Model must name the agent seat, as the dispatch path does")
	}
	if agent.OnDropped == nil {
		t.Fatal("OnDropped must be set: it is the only cleanup a never-started job gets")
	}
	agent.OnDropped()
	if dropped != 1 {
		t.Fatalf("OnDropped did not reach the cleanup closure (called %d times)", dropped)
	}

	render := s.claimSpec("image-gen", func() {})
	if render.Agent {
		t.Fatal("a render is not an agent job")
	}
	if !render.Uncapped {
		t.Fatal("a pulled render must be uncapped, exactly as a dispatched one is: it never touches the endpoint the cap protects")
	}
	if render.Model != "" {
		t.Fatal("Model names the agent seat only for an agent run")
	}

	// The dispatch handler is the reference: for the same task type the two
	// paths must agree on every field that carries authority.
	if got := s.concurrencyCapped(string(core.TaskAgentRun)); got == agent.Uncapped {
		t.Fatal("Uncapped must be the negation of concurrencyCapped, like the dispatch path")
	}
}
