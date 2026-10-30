package main

// D-111 on the CLI verbs. `delegate --contract` and `research` prepared their subtasks in a loop
// that returned at the first one that failed intake, so a file listing one scratch path that was
// not there lost the answer to every other subtask in it, the very defect the MCP doors lost in
// the same release. They prepare each subtask on its own now (cliIntake): what is valid runs, what
// is not is reported at its own position as a contract-class defer, and only a call with nothing
// valid in it is refused, naming every bad subtask. The verbs cannot run a seat in a unit test,
// so the behaviour is pinned in the intake helper and the wiring is pinned at the source.

import (
	"errors"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

func intakeSpec(goal string, contextPaths ...string) delegate.SubtaskSpec {
	return delegate.SubtaskSpec{AgentContract: core.AgentContract{Goal: goal}, ContextPaths: contextPaths}
}

// [ok, refused, ok]: the engine is to be handed the two valid contracts, tagged with the verb's
// door, and the lints stay on the subtasks they belong to.
func TestCLIIntakeKeepsTheValidSubtasksAndTheirPositions(t *testing.T) {
	root := t.TempDir()
	third := intakeSpec("third")
	third.Acceptance = []string{"nonempty:summary"} // shape-only: the intake lint has something to say about it
	specs := []delegate.SubtaskSpec{
		intakeSpec("first"),
		intakeSpec("refused", "no-such-scratch-file.txt"),
		third,
	}
	batch, lints, err := cliIntake(specs, root, core.AgentContextMaxBytes, "cli:delegate", "subtask", nil)
	if err != nil {
		t.Fatalf("cliIntake: %v: a call with valid subtasks in it is not refused as a whole", err)
	}
	if len(batch.Contracts) != 2 || batch.Total != 3 || len(batch.Rejections) != 1 || batch.Rejections[0].Index != 1 {
		t.Fatalf("batch = %d contracts, total %d, rejections %+v, want the two valid subtasks and the refusal at 1", len(batch.Contracts), batch.Total, batch.Rejections)
	}
	for k, want := range []string{"first", "third"} {
		if batch.Contracts[k].Goal != want || batch.Contracts[k].Door != "cli:delegate" || batch.Indices[k] != []int{0, 2}[k] {
			t.Errorf("contract %d = goal %q door %q at %d, want %q on cli:delegate at %d", k, batch.Contracts[k].Goal, batch.Contracts[k].Door, batch.Indices[k], want, []int{0, 2}[k])
		}
	}
	want := delegate.LintAcceptance(batch.Contracts[1])
	if len(want) == 0 {
		t.Fatal("the third subtask draws no intake lint: the fixture no longer proves the lints stay on their own subtasks")
	}
	if len(lints) != 3 || lints[1] != nil || strings.Join(lints[2], "|") != strings.Join(want, "|") {
		t.Errorf("lints = %v, want one slot per subtask sent, none for the refused one, and the third's own lint at position 2: %v", lints, want)
	}
	// The spliced answer is one result per subtask sent, the refusal in its own place.
	results, sum := batch.Splice([]delegate.PlacedResult{{}, {}}, delegate.Summary{Succeeded: 2}, "subtask", nil)
	if len(results) != 3 || !results[1].Result.Deferred || results[1].Result.DeferClass != core.DeferClassContract ||
		!strings.HasPrefix(results[1].Result.Reason, "subtask 1:") || sum.Deferred != 1 || sum.Infrastructure != 0 {
		t.Errorf("spliced = %d results, [1] = %+v, summary %+v, want a contract defer at position 1 and no infrastructure", len(results), results[1].Result, sum)
	}
}

// Nothing valid: the call is refused as it always was, and the refusal names every bad subtask.
func TestCLIIntakeRefusesAWholeCallWithNothingValidNamingEveryOne(t *testing.T) {
	root := t.TempDir()
	_, _, err := cliIntake([]delegate.SubtaskSpec{
		intakeSpec("a", "gone-a.txt"), intakeSpec("b", "gone-b.txt"),
	}, root, core.AgentContextMaxBytes, "cli:delegate", "subtask", nil)
	if err == nil {
		t.Fatal("a call with no valid subtask was accepted")
	}
	for _, want := range []string{"subtask 0:", "gone-a.txt", "subtask 1:", "gone-b.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
}

// A research page is named by its place in the url list, the number the sources list shows, not
// by the contract's place among the usable pages.
func TestCLIIntakeNamesAPageByItsPlaceInTheURLList(t *testing.T) {
	_, _, err := cliIntake([]delegate.SubtaskSpec{intakeSpec("a", "gone.txt")}, t.TempDir(), core.AgentContextMaxBytes, "cli:research", "source", []int{4})
	if err == nil || !strings.Contains(err.Error(), "source 4:") {
		t.Fatalf("err = %v, want the refusal to name \"source 4:\" (the page's place in the url list)", err)
	}
}

// The engine refuses the whole call (an unknown route, say): the verb's error keeps the engine's own error and also names the
// subtasks intake refused, as the MCP doors' whole-call defer does.
func TestCLIEngineErrorStillNamesTheIntakeRejections(t *testing.T) {
	batch, _, err := cliIntake([]delegate.SubtaskSpec{intakeSpec("ok"), intakeSpec("refused", "gone.txt")}, t.TempDir(), core.AgentContextMaxBytes, "cli:delegate", "subtask", nil)
	if err != nil {
		t.Fatal(err)
	}
	engine := errors.New(`delegate: route "x" not recognized`)
	got := cliEngineErr(engine, batch, "subtask", nil)
	if !errors.Is(got, engine) {
		t.Errorf("err = %v: the engine's own error must stay wrapped", got)
	}
	if !strings.Contains(got.Error(), `route "x" not recognized; also refused at intake: subtask 1:`) || !strings.Contains(got.Error(), "gone.txt") {
		t.Errorf("err = %q, want the engine's error followed by the refused subtask", got)
	}
	clean, _, err := cliIntake([]delegate.SubtaskSpec{intakeSpec("ok")}, t.TempDir(), core.AgentContextMaxBytes, "cli:delegate", "subtask", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cliEngineErr(engine, clean, "subtask", nil); got != engine {
		t.Errorf("err = %v, want the engine's error untouched when nothing was refused", got)
	}
}

func writeDelegateCLIConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"agent_delegation_enabled": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The verb itself, up to the point a seat would be needed: every subtask in the file is refused at
// intake, so nothing runs and the error names all of them, not the first.
func TestRunDelegateRefusesAFileWithNothingValidNamingEveryBadSubtask(t *testing.T) {
	root := t.TempDir()
	contract := writeContract(t, `[{"goal":"a","context_paths":["gone-a.txt"]},{"goal":"b","context_paths":["gone-b.txt"]}]`)
	err := runDelegate([]string{"--config", writeDelegateCLIConfig(t), "--contract", contract, "--read-root", root, "--route", "local"})
	if err == nil {
		t.Fatal("runDelegate accepted a file whose every subtask failed intake")
	}
	for _, want := range []string{"subtask 0:", "gone-a.txt", "subtask 1:", "gone-b.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
}

// Nine subtasks with one bad one are refused for their number, as nine good ones are: the cap is
// read off what the file holds, and a refusal never buys a call a place under it.
func TestRunDelegateKeepsTheEightSubtaskCapWhenSomeAreRefused(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`[{"goal":"bad","context_paths":["gone.txt"]}`)
	for i := 0; i < 8; i++ {
		sb.WriteString(`,{"goal":"ok"}`)
	}
	sb.WriteString(`]`)
	err := runDelegate([]string{"--config", writeDelegateCLIConfig(t), "--contract", writeContract(t, sb.String()), "--read-root", t.TempDir(), "--route", "local"})
	if err == nil || !strings.Contains(err.Error(), "9 subtasks exceeds the max of 8") {
		t.Fatalf("err = %v, want the cap refusal for nine subtasks", err)
	}
	if strings.Contains(err.Error(), "subtask 0:") {
		t.Errorf("err = %q: the refusal is for the number sent, before any subtask is prepared", err)
	}
}

// The seat is out of reach of a unit test, so the wiring is pinned at the source, as the rescue's and the
// quote check's are: both verbs prepare through cliIntake (never PrepareContractWithCap in a loop), run
// the engine, splice the refusals back among its results, and only then build the body.
func TestTheCLIVerbsPrepareEachSubtaskOnItsOwnAndSpliceTheRefusals(t *testing.T) {
	fset := token.NewFileSet()
	f := parseMainSplit(t, fset)
	for _, tc := range []struct {
		verb   string
		engine string
	}{{"runDelegate", "RunWith"}, {"runResearch", "RunBatched"}} {
		t.Run(tc.verb, func(t *testing.T) {
			var fn *ast.FuncDecl
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == tc.verb && fd.Recv == nil {
					fn = fd
				}
			}
			if fn == nil {
				t.Fatalf("%s not found in main.go: the scan is broken", tc.verb)
			}
			var intake, engine, splice, wire, direct, engineErr token.Pos
			named := func(call *ast.CallExpr, name string) bool {
				for _, a := range call.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == name {
						return true
					}
				}
				return false
			}
			researchIDs := true // a research page is named by its place in the url list, in the intake error and in the splice
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == "cliEngineErr" {
						engineErr = call.Pos()
						if tc.verb == "runResearch" && !named(call, "resultSources") {
							researchIDs = false
						}
					}
					if fun.Name == "cliIntake" {
						intake = call.Pos()
						if tc.verb == "runResearch" && !named(call, "resultSources") {
							researchIDs = false
						}
					}
				case *ast.SelectorExpr:
					x, _ := fun.X.(*ast.Ident)
					switch {
					case x != nil && x.Name == "delegate" && fun.Sel.Name == tc.engine:
						engine = call.Pos()
					case x != nil && x.Name == "delegate" && fun.Sel.Name == "WireResponse":
						wire = call.Pos()
					case x != nil && x.Name == "delegate" && fun.Sel.Name == "PrepareContractWithCap":
						direct = call.Pos()
					case fun.Sel.Name == "Splice":
						splice = call.Pos()
						if tc.verb == "runResearch" && !named(call, "resultSources") {
							researchIDs = false
						}
					}
				}
				return true
			})
			if direct != token.NoPos {
				t.Errorf("%s calls delegate.PrepareContractWithCap itself: one bad subtask would reject the valid ones again (register D-111); prepare through cliIntake", tc.verb)
			}
			if engine == token.NoPos || wire == token.NoPos {
				t.Fatalf("%s no longer runs delegate.%s and builds delegate.WireResponse: the scan is broken", tc.verb, tc.engine)
			}
			if intake == token.NoPos || !(intake < engine) {
				t.Errorf("%s does not prepare its subtasks through cliIntake before the engine runs", tc.verb)
			}
			if engineErr == token.NoPos || !(engine < engineErr) {
				t.Errorf("%s does not return the engine's error through cliEngineErr: a call the engine refuses would hide the subtasks intake already refused", tc.verb)
			}
			if !researchIDs {
				t.Errorf("runResearch does not hand resultSources to cliIntake and Splice: a refused page would be named by the contract's place, not the url list's")
			}
			if splice == token.NoPos || !(engine < splice && splice < wire) {
				t.Errorf("%s does not splice the refused subtasks back (batch.Splice) between the engine's results and the body: the answer would hold fewer results than subtasks sent", tc.verb)
			}
		})
	}
}
