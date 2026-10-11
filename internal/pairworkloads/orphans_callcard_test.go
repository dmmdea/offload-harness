package pairworkloads

// C02 follow-up (2026-10-10): a swept call card said "gone while queued: it never started" for a
// transcription that was killed mid-decode. calls.go has said since 0.140.6 that a lane that cannot tell when
// its engine starts (transcribe: the wait is inside llama-swap) never marks its card running, so the card is
// "queued" from the first frame to the last. "Queued" therefore proves nothing about such a call, and the
// sweep must not claim it. A media call, whose lane marks the card running once it holds its GPU, keeps the
// claim: for it queued does mean not started.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// sweepCallCard opens a call card for task at t=1000 ms, never marks it working, kills its producer and sweeps
// min minutes later; it returns the failure text the card was closed with.
func sweepCallCard(t *testing.T, task string, min int64) string {
	t.Helper()
	r := newOrphanRig(t)
	e := r.emitter(nil, nil)
	e.now = func() time.Time { return time.UnixMilli(1_000) }
	id, _, _ := e.Begin(task, "offload_"+task)
	if id == "" {
		t.Fatalf("Begin(%q) opened no card", task)
	}
	e.Wait()
	s := r.emitter(dead, nil)
	s.now = sweepNow(1_000, min)
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	failed := r.failedFrames()
	if len(failed) != 1 {
		t.Fatalf("want one failed frame, got %d", len(failed))
	}
	return errorOf(t, failed[0])
}

func TestSweepDoesNotClaimATranscriptionNeverStarted(t *testing.T) {
	got := sweepCallCard(t, "transcribe", 9)
	want := fmt.Sprintf("%s: pid %d gone 9m in, no outcome reported (killed, console closed or reboot)", OrphanError, os.Getpid())
	if got != want {
		t.Errorf("error = %q\nwant    %q", got, want)
	}
	if strings.Contains(got, "never started") || strings.Contains(got, "while queued") {
		t.Errorf("a transcription's card is queued while the engine decodes; the card must not say it never started: %q", got)
	}
}

func TestSweepStillSaysAMediaCallThatNeverGotItsCardNeverStarted(t *testing.T) {
	got := sweepCallCard(t, "generate_image", 3)
	want := fmt.Sprintf("%s: pid %d gone while queued: it never started, no outcome reported", OrphanError, os.Getpid())
	if got != want {
		t.Errorf("error = %q\nwant    %q", got, want)
	}
}

// tasksThatNeverMarkWorking is only as true as internal/pipeline: transcribe's lane must not call
// core.MarkWorking (if it did, its card would turn running and the claim would be needless), and the lanes of
// the other long calls must (or "never started" would be wrong for them).
func TestOnlyTranscribeNeverMarksWorking(t *testing.T) {
	if len(tasksThatNeverMarkWorking) != 1 || !tasksThatNeverMarkWorking[string(core.TaskTranscribe)] {
		t.Fatalf("tasksThatNeverMarkWorking = %v, want exactly {transcribe}: a long-call lane that cannot mark its work started needs the orphan text's care, and one that can must not be listed", tasksThatNeverMarkWorking)
	}
	files, err := filepath.Glob(filepath.Join("..", "pipeline", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no pipeline sources (%v)", err)
	}
	marks := 0
	inTranscribe := false
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("%s: %v", path, perr)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "MarkWorking" {
					marks++
					if fn.Name.Name == "runTranscribe" {
						inTranscribe = true
					}
				}
				return true
			})
		}
	}
	if marks < 5 {
		t.Fatalf("found only %d core.MarkWorking calls in internal/pipeline: the walk no longer reads them", marks)
	}
	if inTranscribe {
		t.Error("runTranscribe now marks its card working: remove transcribe from tasksThatNeverMarkWorking, so its swept card keeps the plain 'while queued' text")
	}
}
