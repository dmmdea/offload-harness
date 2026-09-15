package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteLimitRefusesPastTheFileCap: the cap counts DISTINCT paths, so an
// edit-then-fix cycle on one file stays inside a one-file budget while a second
// file does not.
func TestWriteLimitRefusesPastTheFileCap(t *testing.T) {
	l := NewWriteLimit(2, 0)
	if why := l.Admit("a.go", 10); why != "" {
		t.Fatalf("first file refused: %s", why)
	}
	if why := l.Admit("a.go", 10); why != "" {
		t.Fatalf("re-writing the SAME file must not spend a second file slot: %s", why)
	}
	if why := l.Admit("b.go", 10); why != "" {
		t.Fatalf("second file refused: %s", why)
	}
	why := l.Admit("c.go", 10)
	if why == "" {
		t.Fatal("a third file passed a two-file cap")
	}
	if !strings.Contains(why, "a.go") || !strings.Contains(why, "b.go") {
		t.Errorf("refusal %q does not name what was already touched — the model cannot correct without it", why)
	}
	if l.Files() != 2 {
		t.Errorf("Files() = %d after a refusal, want 2 — a refused write must not be charged", l.Files())
	}
}

// TestWriteLimitRefusesPastTheByteCap, and charges a re-write again: the bytes
// were written twice, so they cost twice.
func TestWriteLimitRefusesPastTheByteCap(t *testing.T) {
	l := NewWriteLimit(0, 100)
	if why := l.Admit("a.go", 60); why != "" {
		t.Fatalf("first write refused: %s", why)
	}
	if why := l.Admit("a.go", 60); why == "" {
		t.Fatal("120 bytes passed a 100-byte cap")
	}
	if l.Bytes() != 60 {
		t.Errorf("Bytes() = %d after a refusal, want 60", l.Bytes())
	}
	if why := l.Admit("a.go", 40); why != "" {
		t.Fatalf("a write that exactly fills the cap must be admitted: %s", why)
	}
	if l.Bytes() != 100 {
		t.Errorf("Bytes() = %d, want 100", l.Bytes())
	}
}

// TestNilWriteLimitAdmitsEverything: the operator's own CLI worktree has never
// had a byte cap, and this is what keeps it that way.
func TestNilWriteLimitAdmitsEverything(t *testing.T) {
	var l *WriteLimit
	for i := 0; i < 100; i++ {
		if why := l.Admit("f.go", 1<<20); why != "" {
			t.Fatalf("a nil limit refused a write: %s", why)
		}
	}
	if l.Files() != 0 || l.Bytes() != 0 || l.Names() != nil {
		t.Error("a nil limit must account nothing")
	}
}

// TestZeroBoundsAreNoBound: a zero ceiling means "no bound of that kind", never
// "nothing may be written" — a cap that silently refused everything would read
// in the logs exactly like a broken seat.
func TestZeroBoundsAreNoBound(t *testing.T) {
	l := NewWriteLimit(0, 0)
	for i := 0; i < 50; i++ {
		if why := l.Admit(filepath.Join("d", string(rune('a'+i%26))+".go"), 1<<20); why != "" {
			t.Fatalf("an unbounded limit refused a write: %s", why)
		}
	}
}

// TestWriteToolsEnforceTheLimitBeforeTheBytesLand is the point of enforcing at
// the tool rather than only after the run: a refused write leaves the file
// untouched and hands the model a "NOT performed" it can act on.
func TestWriteToolsEnforceTheLimitBeforeTheBytesLand(t *testing.T) {
	wt := t.TempDir()
	limit := NewWriteLimit(1, 0)
	tools, err := WriteToolsLimited(wt, NewPolicy(true, nil), limit)
	if err != nil {
		t.Fatal(err)
	}
	var write func(context.Context, string) (string, error)
	for _, tool := range tools {
		if tool.Name == "write_file" {
			write = tool.Exec
		}
	}
	if write == nil {
		t.Fatal("write_file absent from the limited tool set")
	}

	args := func(p, c string) string {
		b, _ := json.Marshal(map[string]string{"path": p, "content": c})
		return string(b)
	}
	if _, werr := write(context.Background(), args("first.txt", "ok")); werr != nil {
		t.Fatalf("the first write was refused: %v", werr)
	}
	out, werr := write(context.Background(), args("second.txt", "must not land"))
	if werr == nil {
		t.Fatalf("a second file passed a one-file budget: %q", out)
	}
	if !IsNotPerformed(werr) {
		t.Errorf("a budget refusal must be a NotPerformed (defer-not-crash), got %T: %v", werr, werr)
	}
	if !strings.Contains(werr.Error(), "write budget") {
		t.Errorf("refusal %q does not say it was the budget", werr.Error())
	}
	if _, serr := os.Stat(filepath.Join(wt, "second.txt")); serr == nil {
		t.Fatal("the refused write landed on disk anyway — the limit must be checked BEFORE the bytes")
	}
}
