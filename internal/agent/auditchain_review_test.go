package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestEndRunIsIdempotentAndSkipsAnEmptyRun (SF-08 review 2, 4): a run with no
// decision writes no run_end (a read-only run leaves the trail untouched), and a
// second EndRun writes nothing more.
func TestEndRunIsIdempotentAndSkipsAnEmptyRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	empty := NewAuditLog(path).WithChain("run-empty")
	if err := empty.EndRun(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("an empty run wrote to the trail (stat err %v)", err)
	}
	chainedRun(t, path, "run-twice", 2)
	l := NewAuditLog(path).WithChain("run-twice-2")
	_ = l.record(Action{Kind: ActWrite, Path: "a"}, Allow, "ok", Rule{})
	_ = l.EndRun()
	_ = l.EndRun()
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || rep.Chained != 3+2 {
		t.Errorf("a second EndRun changed the trail: %+v", rep)
	}
}

// TestARowAfterRunEndIsLateNotBroken (SF-08 review 2): a tool goroutine abandoned at a
// timeout can write after the door closed the run; a row that still chains is reported
// late, not as tampering.
func TestARowAfterRunEndIsLateNotBroken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	l := NewAuditLog(path).WithChain("run-late")
	_ = l.record(Action{Kind: ActWrite, Path: "a"}, Allow, "ok", Rule{})
	_ = l.EndRun()
	_ = l.record(Action{Kind: ActRead, Path: ".env"}, Warn, "late", Rule{})
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || len(rep.Late) != 1 || rep.Late[0] != "run-late" {
		t.Errorf("a chained row after run_end: %+v, want late not broken", rep)
	}
}

// TestAnEditedRunEndBreaksTheChain (SF-08 review 5): run_end's count and head are
// checked against the chain.
func TestAnEditedRunEndBreaksTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	chainedRun(t, path, "run-e", 2)
	ls := lines(t, path)
	last := len(ls) - 1
	ls[last] = bytes.Replace(ls[last], []byte(`"count":2`), []byte(`"count":1`), 1)
	writeLines(t, path, ls)
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 1 || rep.Breaks[0].RunID != "run-e" || !strings.Contains(rep.Breaks[0].Problem, "run_end") {
		t.Errorf("an edited run_end: %+v", rep)
	}
}

// TestAFailedWriteDoesNotAdvanceTheChain (SF-08 review 5): a row that never reached the
// file is not linked to; the next row chains to the last row that did land.
func TestAFailedWriteDoesNotAdvanceTheChain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-audit.jsonl")
	l := NewAuditLog(path).WithChain("run-f")
	_ = l.record(Action{Kind: ActWrite, Path: "a"}, Allow, "ok", Rule{})
	good := l.path
	l.path = dir // a directory: the next write fails
	if err := l.record(Action{Kind: ActWrite, Path: "b"}, Allow, "ok", Rule{}); err == nil {
		t.Fatal("the write into a directory did not fail")
	}
	l.path = good
	_ = l.record(Action{Kind: ActWrite, Path: "c"}, Allow, "ok", Rule{})
	_ = l.EndRun()
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || len(rep.Open) != 0 || rep.Chained != 3 {
		t.Errorf("after a failed write: %+v, want a clean closed chain of 3 rows", rep)
	}
}

// TestOneSharedTrailKeepsItsChainUnderConcurrentCallers (SF-08 review 5): the CLI
// serve mode shares one AuditLog across requests; its mutex keeps seq and prev in the
// order rows reach the file.
func TestOneSharedTrailKeepsItsChainUnderConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	l := NewAuditLog(path).WithChain("run-shared")
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = l.record(Action{Kind: ActWrite, Path: fmt.Sprintf("w%d-%d", w, i)}, Allow, "ok", Rule{})
			}
		}(w)
	}
	wg.Wait()
	_ = l.EndRun()
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || rep.Chained != 401 || len(rep.Open) != 0 {
		t.Errorf("one shared trail, 8 callers: %+v", rep)
	}
}

// TestChainOffAddsNoField (SF-08 review 5): an unchained row carries none of the five
// chain fields.
func TestChainOffAddsNoField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	_ = NewAuditLog(path).record(Action{Kind: ActWrite, Path: "a"}, Allow, "ok", Rule{})
	var m map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(bytes.TrimSpace(b), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run_id", "seq", "prev_sha256", "count", "head"} {
		if _, ok := m[k]; ok {
			t.Errorf("an unchained row carries %q: %s", k, b)
		}
	}
}

// TestStrictVerifyFailsOpenRuns (SF-08 review 1): a truncated tail (the run_end gone) is
// an open run; the report flags it so a strict verify can fail it.
func TestStrictVerifyFailsOpenRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	chainedRun(t, path, "run-t", 3)
	ls := lines(t, path)
	writeLines(t, path, ls[:len(ls)-2]) // the last decision and the run_end cut off
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || len(rep.Open) != 1 {
		t.Fatalf("a truncated run: %+v, want one open run and no break", rep)
	}
	if rep.Open == nil || rep.Late == nil || rep.Breaks == nil {
		t.Errorf("report lists must be empty, not null, for JSON readers: %+v", rep)
	}
}
