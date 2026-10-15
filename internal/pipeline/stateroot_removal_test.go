package pipeline

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestFinishIsolationRetriesARemovalOnceAndReportsWhatStaysBehind pins the removal of this package's throwaway state root, the same rule
// as the other five suites that make one: a removal that fails (on Windows a process that has not yet let go of a file or of the folder as
// its working directory) is tried once more, and a root that is still there after the second attempt is named on stderr with the error, so
// that a leak is seen, never dropped. A root that stays behind also ends a passing suite with exit code 1, because `go test ./...` hides
// the stderr of a package that passed and the report alone would not reach anyone; a suite that failed keeps its own code, and a root that
// went on the retry reports nothing.
func TestFinishIsolationRetriesARemovalOnceAndReportsWhatStaysBehind(t *testing.T) {
	boom := errors.New("sharing violation")
	for _, c := range []struct {
		name       string
		failures   int
		suiteCode  int
		wantCalls  int
		wantReport bool
		wantCode   int
	}{
		{"it works at once", 0, 0, 1, false, 0},
		{"the retry works", 1, 0, 2, false, 0},
		{"it fails both times, the suite passed", 99, 0, 2, true, 1},
		{"it fails both times, the suite failed", 99, 3, 2, true, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var report bytes.Buffer
			was := isolationStderr
			isolationStderr = &report
			t.Cleanup(func() { isolationStderr = was })
			var tried []string
			removeAll := func(p string) error {
				tried = append(tried, p)
				if len(tried) <= c.failures {
					return boom
				}
				return nil
			}
			if code := finishIsolation(c.suiteCode, dir, removeAll); code != c.wantCode {
				t.Fatalf("code = %d, want %d (a root that stays behind fails a passing suite and leaves a failing one's code alone)", code, c.wantCode)
			}
			if len(tried) != c.wantCalls {
				t.Fatalf("the removal was tried %d times, want %d (once, and once more only when it failed)", len(tried), c.wantCalls)
			}
			for _, p := range tried {
				if p != dir {
					t.Fatalf("the removal was asked for %q, want the throwaway root %q", p, dir)
				}
			}
			got := report.String()
			if c.wantReport {
				for _, want := range []string{"pipeline tests:", filepath.Clean(dir), boom.Error()} {
					if !strings.Contains(got, want) {
						t.Fatalf("the report %q should contain %q", got, want)
					}
				}
			} else if got != "" {
				t.Fatalf("nothing is left behind, so nothing is reported, got %q", got)
			}
		})
	}
}

// TestMainRemovesItsRootThroughFinishIsolation pins the wiring the test above cannot reach: TestMain is not a function a test can call, so
// the source says it. TestMain must end through finishIsolation and must not call os.RemoveAll itself, the way it once dropped the error.
func TestMainRemovesItsRootThroughFinishIsolation(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "leaseisolation_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var main *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "TestMain" {
			main = fd
		}
	}
	if main == nil {
		t.Fatal("leaseisolation_test.go has no TestMain")
	}
	finish, direct := 0, 0
	ast.Inspect(main.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "finishIsolation" {
				finish++
			}
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok && x.Name == "os" && fn.Sel.Name == "RemoveAll" {
				direct++
			}
		}
		return true
	})
	if finish != 1 || direct != 0 {
		t.Fatalf("TestMain calls finishIsolation %d times and os.RemoveAll %d times; want 1 and 0, so a root it cannot remove is retried, reported and fails the run", finish, direct)
	}
}
