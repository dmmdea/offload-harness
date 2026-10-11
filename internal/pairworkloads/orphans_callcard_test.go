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

// tasksThatNeverMarkWorking is only as true as internal/pipeline, in BOTH directions its comment in calls.go claims:
// transcribe's lane must not reach core.MarkWorking (if it did, its card would turn running and the claim would be
// needless), and the lane of every other long call must (or the orphan text's "never started" would be wrong for it:
// a card that stays queued while the engine works). The second half used to be a count (">= 5 calls found") and
// held for nothing: the tts_endpoint lane of generate_audio took no lease, never marked its card, and every test was
// green (0.180 final check). Each lane that serves a long-call task is now named, and each must REACH a mark: through
// the lease or slot it takes (a mark inside a helper counts, by the helper's name) or by marking directly.

// laneEntries are the functions of internal/pipeline that serve each long-call task, one per lane. A task with
// several lanes (a ComfyUI one, an sd.cpp one, a voice endpoint) lists them all, because a mark on one lane says nothing
// of the others.
var laneEntries = map[string][]string{
	"generate_image":        {"runGenerateImage", "runGenerateImageSdcpp"},
	"inpaint_image":         {"runInpaintImage"},
	"edit_image_generative": {"runEditImageGenerative"},
	"upscale_image":         {"runUpscaleImage"},
	"generate_video":        {"runGenerateVideo", "runGenerateVideoSdcpp"},
	"animate_character":     {"runAnimateCharacter", "runAnimateCharacterSdcpp"},
	"generate_audio":        {"runGenerateAudio", "runGenerateAudioAudiocpp", "runVoiceEndpoint"},
	"run_graph":             {"runRunGraph"},
	"compose_video":         {"runComposeVideo"},
	"transcribe":            {"runTranscribe"},
}

// pipelineCalls reads internal/pipeline once: for every function, by bare name (a method and a function that share a
// name share an entry), the names it calls and whether it calls MarkWorking itself. marks counts the direct calls.
func pipelineCalls(t *testing.T) (calls map[string]map[string]bool, direct map[string]bool, marks int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "pipeline", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no pipeline sources (%v)", err)
	}
	calls, direct = map[string]map[string]bool{}, map[string]bool{}
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
			name := fn.Name.Name
			if calls[name] == nil {
				calls[name] = map[string]bool{}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					calls[name][fun.Sel.Name] = true
					if fun.Sel.Name == "MarkWorking" {
						direct[name] = true
						marks++
					}
				case *ast.Ident:
					calls[name][fun.Name] = true
				}
				return true
			})
		}
	}
	return calls, direct, marks
}

// reachesMark reports whether fn marks the card itself or calls (transitively, by name) a function that does.
func reachesMark(fn string, calls map[string]map[string]bool, direct map[string]bool) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if seen[n] {
			return false
		}
		seen[n] = true
		if direct[n] {
			return true
		}
		for c := range calls[n] {
			if _, known := calls[c]; known && walk(c) {
				return true
			}
		}
		return false
	}
	return walk(fn)
}

func TestOnlyTranscribeNeverMarksWorking(t *testing.T) {
	if len(tasksThatNeverMarkWorking) != 1 || !tasksThatNeverMarkWorking[string(core.TaskTranscribe)] {
		t.Fatalf("tasksThatNeverMarkWorking = %v, want exactly {transcribe}: a long-call lane that cannot mark its work started needs the orphan text's care, and one that can must not be listed", tasksThatNeverMarkWorking)
	}
	// The table names every long-call task and nothing else: a task added to longCallTasks must say how its lanes mark.
	for task := range longCallTasks {
		if _, ok := laneEntries[task]; !ok {
			t.Errorf("long-call task %q has no entry in laneEntries: name the functions of internal/pipeline that serve it, so the test can check each marks its card", task)
		}
	}
	for task := range laneEntries {
		if !longCallTasks[task] {
			t.Errorf("laneEntries names %q, which is not a long-call task", task)
		}
	}
	calls, direct, marks := pipelineCalls(t)
	if marks < 5 {
		t.Fatalf("found only %d core.MarkWorking calls in internal/pipeline: the walk no longer reads them", marks)
	}
	for task, lanes := range laneEntries {
		for _, lane := range lanes {
			if _, ok := calls[lane]; !ok {
				t.Errorf("%s: lane function %s is not in internal/pipeline any more: update laneEntries", task, lane)
				continue
			}
			got := reachesMark(lane, calls, direct)
			switch {
			case tasksThatNeverMarkWorking[task] && got:
				t.Errorf("%s now marks its card working (%s reaches core.MarkWorking): remove it from tasksThatNeverMarkWorking, so its swept card keeps the plain 'while queued' text", task, lane)
			case !tasksThatNeverMarkWorking[task] && !got:
				t.Errorf("%s: lane %s never marks its card working, so its card is queued for the whole run and a sweep of it would say the call never started. Mark it where the work starts (core.MarkWorking), or list the task in tasksThatNeverMarkWorking and say why", task, lane)
			}
		}
	}
}
