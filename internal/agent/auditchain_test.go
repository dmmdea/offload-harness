package agent

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// chainedRun writes n decisions through one chained trail and closes the run.
func chainedRun(t *testing.T, path, runID string, n int) {
	t.Helper()
	l := NewAuditLog(path).WithChain(runID)
	for i := 0; i < n; i++ {
		if err := l.record(Action{Kind: ActWrite, Path: fmt.Sprintf("f%d.txt", i)}, Allow, "ok", Rule{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.EndRun(); err != nil {
		t.Fatal(err)
	}
}

func lines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
}

func writeLines(t *testing.T, path string, ls [][]byte) {
	t.Helper()
	if err := os.WriteFile(path, append(bytes.Join(ls, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestChainedTrailVerifies (register SF-08): a chained run verifies clean, with its
// rows counted and its run closed.
func TestChainedTrailVerifies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	chainedRun(t, path, "run-a", 3)
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || rep.Runs != 1 || rep.Chained != 4 || len(rep.Open) != 0 {
		t.Errorf("clean chain: %+v", rep)
	}
}

// TestTamperingIsNamedByRunAndSeq: editing, deleting or reordering a row each fails
// verify, naming the run and the seq where the chain breaks.
func TestTamperingIsNamedByRunAndSeq(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func([][]byte) [][]byte
		why    string // the diagnosis the break must give
	}{
		{"edit", func(ls [][]byte) [][]byte {
			ls[1] = bytes.Replace(ls[1], []byte(`"reason":"ok"`), []byte(`"reason":"OK"`), 1)
			return ls
		}, "edited"},
		{"delete", func(ls [][]byte) [][]byte { return append(ls[:1:1], ls[2:]...) }, "removed, reordered or inserted"},
		{"reorder", func(ls [][]byte) [][]byte { ls[1], ls[2] = ls[2], ls[1]; return ls }, "removed, reordered or inserted"},
		{"edit the last decision", func(ls [][]byte) [][]byte {
			ls[2] = bytes.Replace(ls[2], []byte(`"path":"f2.txt"`), []byte(`"path":"g2.txt"`), 1)
			return ls
		}, "edited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
			chainedRun(t, path, "run-x", 3)
			writeLines(t, path, tc.tamper(lines(t, path)))
			rep, err := VerifyAuditFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Breaks) == 0 {
				t.Fatalf("%s went undetected: %+v", tc.name, rep)
			}
			b := rep.Breaks[0]
			if b.RunID != "run-x" || b.Seq == 0 || !strings.Contains(b.Problem, tc.why) {
				t.Errorf("%s: break %+v does not name the run, the seq and %q", tc.name, b, tc.why)
			}
		})
	}
}

// TestConcurrentWritersKeepEveryChainValid: eight runs writing to one file at once
// leave eight valid, closed chains.
func TestConcurrentWritersKeepEveryChainValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			chainedRun(t, path, fmt.Sprintf("run-%d", w), 50)
		}(w)
	}
	wg.Wait()
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || rep.Runs != 8 || rep.Chained != 8*51 || len(rep.Open) != 0 {
		t.Errorf("8 concurrent writers: %+v", rep)
	}
}

// TestUnchainedRowsAndOpenRunsAreReportedNotBroken: rows written with the chain off
// (today's trail) are counted as unchained, and a run with no run_end (a crash) is
// reported open, not as tampering.
func TestUnchainedRowsAndOpenRunsAreReportedNotBroken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	plain := NewAuditLog(path)
	_ = plain.record(Action{Kind: ActWrite, Path: "a"}, Allow, "ok", Rule{})
	open := NewAuditLog(path).WithChain("run-open")
	_ = open.record(Action{Kind: ActWrite, Path: "b"}, Allow, "ok", Rule{})
	rep, err := VerifyAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || rep.Unchained != 1 || len(rep.Open) != 1 || rep.Open[0] != "run-open" {
		t.Errorf("mixed trail: %+v", rep)
	}
}

// TestChainOffKeepsTheRowShape: with the chain off a row carries no chain field, so
// today's trail is byte-compatible.
func TestChainOffKeepsTheRowShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	_ = NewAuditLog(path).record(Action{Kind: ActWrite, Path: "a"}, Allow, "ok", Rule{})
	b, _ := os.ReadFile(path)
	for _, k := range []string{"run_id", "seq", "prev_sha256"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("an unchained row carries %q: %s", k, b)
		}
	}
}

// TestBuildChainsTheTrailWhenAsked: Build attaches a chained trail with a fresh run
// id when AuditChain is set, and EndAudit closes the run.
func TestBuildChainsTheTrailWhenAsked(t *testing.T) {
	dir := t.TempDir()
	audit := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: dir,
		Unattended: true, AllowWrite: true, Worktree: dir, AuditPath: audit, AuditChain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	res.Policy.Decide(Action{Kind: ActWrite, Path: "a.txt"})
	res.EndAudit()
	rep, err := VerifyAuditFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Breaks) != 0 || rep.Runs != 1 || rep.Chained != 2 || len(rep.Open) != 0 {
		t.Errorf("built chained run: %+v", rep)
	}
}
