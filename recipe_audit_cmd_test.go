package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/eval"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/recipe"
)

// `recipe-audit` runs a contract set with and without a recipe on one seat and publishes the recipe
// only on a gate ADOPT with steps no worse. These tests drive the verb in-process (recipeAuditMain)
// and put a fake where the seat is: none of them needs a model, a config file or a GPU.

const raRecipeID = "recipe-cap-listing-01"

func raTaskIDs() []string {
	ids := make([]string, 16)
	for i := range ids {
		ids[i] = fmt.Sprintf("task-%02d", i+1)
	}
	return ids
}

// raFake is the seat. script decides what each arm does with each task; every call is recorded.
type raFake struct {
	calls  []string // "arm:goal"
	script func(arm string, c core.AgentContract) (core.AgentWireResult, error)
}

func (f *raFake) Run(_ context.Context, arm string, c core.AgentContract) (core.AgentWireResult, error) {
	f.calls = append(f.calls, arm+":"+c.Goal)
	return f.script(arm, c)
}

func (f *raFake) count(arm string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, arm+":") {
			n++
		}
	}
	return n
}

// raPin is the pin the fake seat stamps on every run it serves: the harness probes the seat after the
// loop and puts the hash on the wire result (wire.SeatConfigSHA256), so a served run carries one.
const raPin = "seat-pin-a"

// raOutcome scripts a finished run: DONE when it passes (the contracts check for it), with steps.
func raOutcome(pass bool, steps int) core.AgentWireResult {
	out := "no answer"
	if pass {
		out = "DONE"
	}
	return core.AgentWireResult{Output: out, Steps: steps, StopReason: "final", SeatConfigSHA256: raPin}
}

// raEnv is one audit's files: a store holding the proposed recipe, a frozen baseline over the 16
// tasks, and a contracts file.
type raEnv struct {
	store, baseline, tasks, inputOut string
	fake                             *raFake
	seatAsked                        string             // the --seat the factory was handed
	candidateRules                   core.AgentEnvRules // the rule set the factory was handed for the candidate arm
}

func newRAEnv(t *testing.T) *raEnv {
	t.Helper()
	dir := t.TempDir()
	e := &raEnv{
		store:    filepath.Join(dir, "recipes"),
		baseline: filepath.Join(dir, "baseline.json"),
		tasks:    filepath.Join(dir, "tasks.json"),
		inputOut: filepath.Join(dir, "gate-input.json"),
		fake: &raFake{script: func(string, core.AgentContract) (core.AgentWireResult, error) {
			return raOutcome(true, 3), nil
		}},
	}
	r, err := recipe.Decode(raFixture(t, "proposed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := (recipe.Store{Dir: e.store}).Save(r); err != nil {
		t.Fatal(err)
	}
	e.freeze(t, nil)
	e.writeTasks(t, raTaskIDs())
	return e
}

func raFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("internal", "recipe", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// freeze writes the pre-registration: superiority on the unit interval, judged by the acceptance DSL.
// mutate may change it first.
func (e *raEnv) freeze(t *testing.T, mutate func(*eval.PreReg)) {
	t.Helper()
	pre := eval.PreReg{
		Mode:    eval.ModeSuperiority,
		Primary: eval.PreRegPrimary{Name: "goal_reached", Class: eval.ClassUnitInterval},
		Judge:   eval.Judge{Kind: "deterministic", Identity: "acceptance-dsl"},
		Tasks:   raTaskIDs(),
	}
	if mutate != nil {
		mutate(&pre)
	}
	_, raw, _, err := eval.FreezeBaseline(pre, []byte("synthetic corpus"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.baseline, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *raEnv) writeTasks(t *testing.T, ids []string) {
	t.Helper()
	type entry struct {
		ID       string             `json:"id"`
		Contract core.AgentContract `json:"contract"`
	}
	var list []entry
	for _, id := range ids {
		list = append(list, entry{ID: id, Contract: core.AgentContract{
			SchemaVersion: core.AgentWireSchemaVersion, Goal: id, Acceptance: []string{"contains:DONE"},
		}})
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.tasks, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *raEnv) args(extra ...string) []string {
	return append([]string{
		"--store", e.store, "--recipe", raRecipeID, "--seat", "seat-a", "--seat-tag", "seat-class-a",
		"--contracts", e.tasks, "--baseline", e.baseline, "--engine-build", "engine-1",
		"--input-out", e.inputOut,
	}, extra...)
}

// use installs the fakes for one test. lease nil means "under a live lease".
func (e *raEnv) use(t *testing.T, lease func(config.Config) error) {
	t.Helper()
	if lease == nil {
		lease = func(config.Config) error { return nil }
	}
	prev := recipeAuditDeps
	recipeAuditDeps = recipeAuditSeams{
		config: func(*flag.FlagSet) config.Config { return config.Default() },
		lease:  lease,
		seat: func(_ config.Config, seat string, candidate core.AgentEnvRules) (recipeSeat, func(), error) {
			e.seatAsked, e.candidateRules = seat, candidate
			return e.fake, func() {}, nil
		},
	}
	t.Cleanup(func() { recipeAuditDeps = prev })
}

func (e *raEnv) run(args []string) (string, string, error) {
	var out, errb bytes.Buffer
	err := recipeAuditMain(args, &out, &errb)
	return out.String(), errb.String(), err
}

func (e *raEnv) saved(t *testing.T) recipe.Recipe {
	t.Helper()
	r, err := (recipe.Store{Dir: e.store}).Load(raRecipeID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// wantRefusal asserts a typed refusal of the given kind that ends the process with code 2 and asked
// the seat nothing.
func (e *raEnv) wantRefusal(t *testing.T, err error, kind string) {
	t.Helper()
	var ref *recipeRefusal
	if !errors.As(err, &ref) {
		t.Fatalf("want a typed refusal (%s), got %v", kind, err)
	}
	if ref.Kind != kind {
		t.Fatalf("refusal kind %q (%s), want %q", ref.Kind, ref.Detail, kind)
	}
	if code, line := exitStatus(err); code != 2 || !strings.Contains(line, kind) {
		t.Fatalf("exit status %d, line %q: want 2 naming %q", code, line, kind)
	}
	if n := len(e.fake.calls); n != 0 {
		t.Fatalf("the seat was asked %d time(s) before the refusal: %v", n, e.fake.calls)
	}
	if got := e.saved(t); got.Status != recipe.StatusProposed || got.Audit != nil {
		t.Fatalf("a refused audit changed the recipe: %+v", got)
	}
}

// With no lease epoch in the environment the verb refuses, runs nothing and leaves the recipe as it was.
func TestRecipeAuditRefusesWithoutALease(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil) // the fake seat and config, then the real lease check in place of the fake lease
	recipeAuditDeps.lease = realRecipeAuditLease
	t.Setenv("GPU_LEASE_EPOCH", "")
	t.Setenv("GPU_LEASE_DIR", "")
	_, _, err := e.run(e.args())
	e.wantRefusal(t, err, refusalNoLease)
}

// An epoch that no live lease carries is a lease this process does not hold.
func TestRecipeAuditRefusesALeaseThisProcessDoesNotHold(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	recipeAuditDeps.lease = realRecipeAuditLease
	t.Setenv("GPU_LEASE_EPOCH", "7")
	t.Setenv("GPU_LEASE_DIR", t.TempDir()) // a lease directory with no lease in it
	_, _, err := e.run(e.args())
	e.wantRefusal(t, err, refusalLeaseNotHeld)
	t.Setenv("GPU_LEASE_EPOCH", "seven")
	_, _, err = e.run(e.args())
	e.wantRefusal(t, err, refusalNoLease)
}

// The lease is asked before every seat call: one that goes away mid-audit ends it, and nothing is recorded.
func TestRecipeAuditChecksTheLeaseBeforeEveryRun(t *testing.T) {
	e := newRAEnv(t)
	asked := 0
	e.use(t, func(config.Config) error {
		asked++
		if asked > 6 { // the up-front check and five per-run checks pass
			return &recipeRefusal{Kind: refusalLeaseNotHeld, Detail: "the lease moved on"}
		}
		return nil
	})
	_, _, err := e.run(e.args())
	var ref *recipeRefusal
	if !errors.As(err, &ref) || ref.Kind != refusalLeaseLost {
		t.Fatalf("want a lease_lost refusal, got %v", err)
	}
	if code, _ := exitStatus(err); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if n := len(e.fake.calls); n != 5 {
		t.Fatalf("the seat was asked %d times, want the 5 that the lease covered", n)
	}
	if got := e.saved(t); got.Status != recipe.StatusProposed || got.Audit != nil {
		t.Fatalf("a lost lease changed the recipe: %+v", got)
	}
}

// A recipe is audited on the seat class it was distilled for.
func TestRecipeAuditRefusesASeatTagThatIsNotTheRecipes(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	args := e.args()
	for i, a := range args {
		if a == "--seat-tag" {
			args[i+1] = "seat-class-b"
		}
	}
	_, _, err := e.run(args)
	e.wantRefusal(t, err, refusalSeatTag)
}

// Whatever is wrong with the inputs is found before the seat is asked.
func TestRecipeAuditRefusesBadInputBeforeAnySeatCall(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, e *raEnv) []string
		kind   string
	}{
		{"no recipe flag", func(t *testing.T, e *raEnv) []string {
			return []string{"--store", e.store, "--seat", "seat-a", "--seat-tag", "seat-class-a", "--contracts", e.tasks, "--baseline", e.baseline, "--engine-build", "e"}
		}, refusalUsage},
		{"no engine build", func(t *testing.T, e *raEnv) []string { return e.args("--engine-build", "") }, refusalUsage},
		{"an argument nobody asked for", func(t *testing.T, e *raEnv) []string { return e.args("surplus") }, refusalUsage},
		{"attempts out of range", func(t *testing.T, e *raEnv) []string { return e.args("--attempts", "0") }, refusalUsage},
		{"unknown recipe", func(t *testing.T, e *raEnv) []string { return e.args("--recipe", "recipe-missing-09") }, refusalInput},
		{"a seat that is not an alias", func(t *testing.T, e *raEnv) []string { return e.args("--seat", "a b") }, refusalInput},
		{"a contract for a task the baseline never declared", func(t *testing.T, e *raEnv) []string {
			e.writeTasks(t, append(raTaskIDs(), "task-99"))
			return e.args()
		}, refusalInput},
		{"a task listed twice", func(t *testing.T, e *raEnv) []string {
			e.writeTasks(t, append(raTaskIDs(), "task-01"))
			return e.args()
		}, refusalInput},
		{"a contract with no acceptance check", func(t *testing.T, e *raEnv) []string {
			raw := `[{"id":"task-01","contract":{"schema_version":1,"goal":"g"}}]`
			if err := os.WriteFile(e.tasks, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			return e.args()
		}, refusalInput},
		{"a contracts file with an unknown key", func(t *testing.T, e *raEnv) []string {
			if err := os.WriteFile(e.tasks, []byte(`[{"id":"task-01","note":"x","contract":{}}]`), 0o644); err != nil {
				t.Fatal(err)
			}
			return e.args()
		}, refusalInput},
		{"a baseline that froze an incumbent", func(t *testing.T, e *raEnv) []string {
			inc := eval.Arm{Stamps: eval.Stamps{HarnessBuildSHA256: "a", SeatConfigSHA256: "b", EngineBuild: "c"}}
			pre := eval.PreReg{Mode: eval.ModeSuperiority, Primary: eval.PreRegPrimary{Name: "goal_reached", Class: eval.ClassUnitInterval},
				Judge: eval.Judge{Kind: "deterministic", Identity: "acceptance-dsl"}, Tasks: raTaskIDs()}
			_, raw, _, err := eval.FreezeBaseline(pre, []byte("c"), &inc)
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(e.baseline, raw, 0o644)
			return e.args()
		}, refusalInput},
		{"a baseline whose judge is a model", func(t *testing.T, e *raEnv) []string {
			e.freeze(t, func(p *eval.PreReg) { p.Judge = eval.Judge{Kind: "model", Identity: "some-judge"} })
			return e.args()
		}, refusalInput},
		// The verb scores every run 1 or 0, so a primary on any other scale, and an efficiency class
		// it cannot read off a run, are refused before the seat is asked and not found out afterwards.
		{"a baseline whose primary is on the 0 to 10 scale", func(t *testing.T, e *raEnv) []string {
			e.freeze(t, func(p *eval.PreReg) { p.Primary.Class = eval.ClassJudge010 })
			return e.args()
		}, refusalInput},
		{"a baseline whose primary is a custom range", func(t *testing.T, e *raEnv) []string {
			e.freeze(t, func(p *eval.PreReg) {
				p.Primary.Class, p.Primary.Range = eval.ClassCustom, []float64{0.5, 1}
			})
			return e.args()
		}, refusalInput},
		{"a baseline that declares a money metric", func(t *testing.T, e *raEnv) []string {
			e.freeze(t, func(p *eval.PreReg) {
				p.Efficiency = []eval.PreRegEfficiency{{Name: "spend", Class: eval.EffMoney}}
			})
			return e.args()
		}, refusalInput},
		{"a baseline that declares a memory metric beside a tokens one", func(t *testing.T, e *raEnv) []string {
			e.freeze(t, func(p *eval.PreReg) {
				p.Efficiency = []eval.PreRegEfficiency{{Name: "forwarded_tokens", Class: eval.EffTokens}, {Name: "resident", Class: eval.EffMemoryGiB}}
			})
			return e.args()
		}, refusalInput},
		// The gate stamps the build it ran under and refuses a stamp that is not a 64-character hash, and a
		// task with no contract is a pair the A/B never measures: both are known before the first seat call.
		{"a build that cannot be hashed", func(t *testing.T, e *raEnv) []string {
			prev := gateBuildSHA
			gateBuildSHA = func() string { return "" }
			t.Cleanup(func() { gateBuildSHA = prev })
			return e.args()
		}, refusalInput},
		{"a contracts file that covers only some of the declared tasks", func(t *testing.T, e *raEnv) []string {
			e.writeTasks(t, raTaskIDs()[:10])
			return e.args()
		}, refusalInput},
		{"a baseline that is not the frozen bytes", func(t *testing.T, e *raEnv) []string {
			raw, _ := os.ReadFile(e.baseline)
			os.WriteFile(e.baseline, bytes.Replace(raw, []byte("\n"), []byte("\n\n"), 1), 0o644) // hand-edited: not the canonical bytes
			return e.args()
		}, refusalInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRAEnv(t)
			e.use(t, nil)
			_, _, err := e.run(c.mutate(t, e))
			e.wantRefusal(t, err, c.kind)
		})
	}
}

// A refusal for a contract set that leaves tasks out names them, so the operator can add them.
func TestRecipeAuditNamesTheDeclaredTasksWithoutAContract(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	e.writeTasks(t, raTaskIDs()[:14]) // task-15 and task-16 have none
	_, _, err := e.run(e.args())
	e.wantRefusal(t, err, refusalInput)
	var ref *recipeRefusal
	errors.As(err, &ref)
	for _, id := range []string{"task-15", "task-16"} {
		if !strings.Contains(ref.Detail, id) {
			t.Errorf("the refusal %q does not name the task %s that has no contract", ref.Detail, id)
		}
	}
	if strings.Contains(ref.Detail, "task-14") {
		t.Errorf("the refusal %q names task-14, which has a contract", ref.Detail)
	}
}

// TestRecipeAuditIsRegisteredInMain: the verb is reachable. main.go carries exactly one case line and
// one usage line for it.
func TestRecipeAuditIsRegisteredInMain(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, `case "recipe-audit":`); n != 1 {
		t.Fatalf("main.go has %d `case \"recipe-audit\":` lines, want 1", n)
	}
	if !strings.Contains(src, "err = runRecipeAudit(args)") {
		t.Fatal("the recipe-audit case does not call runRecipeAudit")
	}
	if n := strings.Count(src, "local-offload recipe-audit "); n != 1 {
		t.Fatalf("main.go usage has %d recipe-audit lines, want 1", n)
	}
}

// The real lease check, end to end: an epoch that a live lease carries lets the audit run, and the
// same environment after the lease is released is a lease this process no longer holds.
func TestRecipeAuditRunsOnlyUnderALiveLease(t *testing.T) {
	e := newRAEnv(t)
	e.use(t, nil)
	recipeAuditDeps.lease = realRecipeAuditLease
	m, err := gpulease.OpenAt("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "recipe-audit test", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = l.Release()
		}
	})
	t.Setenv("GPU_LEASE_DIR", l.Dir())
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(l.Epoch(), 10))
	e.fake.script = raScript(false, true, 5, 3)
	if rep := e.report(t); rep.Status != recipe.StatusPublished {
		t.Fatalf("under a live lease the audit ended %s (%s), want published", rep.Status, rep.Decision)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	released = true
	e.fake.calls = nil
	r := e.saved(t)
	r.Status, r.Audit = recipe.StatusProposed, nil
	if err := (recipe.Store{Dir: e.store}).Save(r); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.run(e.args())
	e.wantRefusal(t, err, refusalLeaseNotHeld)
}
