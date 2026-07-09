package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadGoalQueue(t *testing.T) {
	q := filepath.Join(t.TempDir(), "goals.jsonl")
	content := "# a comment line\n" +
		`{"id":"alpha","goal":"first goal"}` + "\n" +
		"\n" + // blank line
		"bare goal text\n" +
		`{"goal":"no id goal"}` + "\n"
	if err := os.WriteFile(q, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	goals, err := readGoalQueue(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 3 {
		t.Fatalf("expected 3 goals (comment + blank skipped); got %d: %+v", len(goals), goals)
	}
	if goals[0].ID != "alpha" || goals[0].Goal != "first goal" {
		t.Errorf("goal0 wrong: %+v", goals[0])
	}
	if goals[1].Goal != "bare goal text" || goals[1].ID == "" {
		t.Errorf("a bare line should become a goal with a defaulted id; got %+v", goals[1])
	}
	if goals[2].Goal != "no id goal" || goals[2].ID == "" {
		t.Errorf("a missing id should default; got %+v", goals[2])
	}
}

func TestReadGoalQueueMissing(t *testing.T) {
	if _, err := readGoalQueue(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("a missing queue file should error")
	}
}

func TestSafeID(t *testing.T) {
	if got := safeID("alpha-1", 0); got != "alpha-1" {
		t.Errorf("safeID(alpha-1) = %q", got)
	}
	if got := safeID(`a/b\c..d`, 0); strings.ContainsAny(got, `/\`) {
		t.Errorf("safeID must strip path separators; got %q", got)
	}
	if got := safeID("", 5); got != "g5" {
		t.Errorf("empty id should fall back to gN; got %q", got)
	}
	if got := safeID("...", 5); got != "g5" {
		t.Errorf("all-dots id should fall back to gN (no traversal); got %q", got)
	}
	for _, dev := range []string{"con", "COM1", "nul.json", "AUX"} {
		if got := safeID(dev, 9); got != "g9" {
			t.Errorf("Windows reserved name %q must fall back to gN (else the trace is lost to a device); got %q", dev, got)
		}
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "_checkpoint.jsonl")
	if got := readCheckpoint(p); len(got) != 0 {
		t.Errorf("a missing checkpoint should be empty; got %v", got)
	}
	appendCheckpoint(p, "g1")
	appendCheckpoint(p, "g2")
	done := readCheckpoint(p)
	if !done["g1"] || !done["g2"] || len(done) != 2 {
		t.Errorf("checkpoint should record g1,g2; got %v", done)
	}
	appendCheckpoint(p, "g1") // re-append is harmless (set semantics on read)
	if got := readCheckpoint(p); len(got) != 2 {
		t.Errorf("re-appending g1 must not change the done SET; got %v", got)
	}
	appendCheckpoint("", "x") // empty path is a no-op (no panic)
	if got := readCheckpoint(""); len(got) != 0 {
		t.Errorf("empty path should yield an empty set; got %v", got)
	}
}

func TestReadGoalQueueSkipsEmptyGoalObject(t *testing.T) {
	q := filepath.Join(t.TempDir(), "g.jsonl")
	if err := os.WriteFile(q, []byte(`{"id":"x","goal":""}`+"\n"+`{"id":"y","goal":"real"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goals, err := readGoalQueue(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 || goals[0].ID != "y" {
		t.Errorf("a JSON object with an empty goal must be skipped (not fed as bare text); got %+v", goals)
	}
}
