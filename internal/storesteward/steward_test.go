package storesteward

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkfile writes n bytes at path with the given mtime.
func mkfile(t *testing.T, path string, n int, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetIsMinOfCeilingAndVolumeRule(t *testing.T) {
	// 100 GB ceiling on a volume with 60 used + 40 free: the volume rule (80) wins.
	capB, high, low := Budget(100e9, 60e9, 40e9)
	if capB != 80e9 || high != 76e9 || low != 68e9 {
		t.Fatalf("cap/high/low = %d/%d/%d, want 80e9/76e9/68e9", capB, high, low)
	}
	// 50 GB ceiling on the same volume: the ceiling wins.
	capB, _, _ = Budget(50e9, 60e9, 40e9)
	if capB != 50e9 {
		t.Fatalf("cap = %d, want 50e9", capB)
	}
	// No ceiling: volume rule alone.
	capB, _, _ = Budget(0, 60e9, 40e9)
	if capB != 80e9 {
		t.Fatalf("cap = %d, want 80e9", capB)
	}
}

func TestBudgetOnAFullVolumeShrinksToWhatIsUsed(t *testing.T) {
	// free = 0 (the dataset AT its quota): cap is 80 % of what the store holds,
	// so used > high and the next tick prunes — the prune must not wait for
	// free space that a full dataset can never report.
	capB, high, _ := Budget(100e9, 100e9, 0)
	if capB != 80e9 {
		t.Fatalf("cap = %d, want 80e9", capB)
	}
	if int64(100e9) <= high {
		t.Fatalf("a full volume must read as above high (high=%d)", high)
	}
}

func TestPruneRemovesOldestFirstAndStopsAtTarget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	// Four 1 KB pages, ages 4h, 3h, 2h, 1h.
	for i, age := range []time.Duration{4 * time.Hour, 3 * time.Hour, 2 * time.Hour, 1 * time.Hour} {
		mkfile(t, filepath.Join(dir, "p"+string(rune('a'+i))), 1024, now.Add(-age))
	}
	files, used, err := Scan(dir)
	if err != nil || used != 4096 {
		t.Fatalf("scan: used=%d err=%v", used, err)
	}
	removed, freed, err := Prune(files, used, 2048, now, MinAge, nil)
	if err != nil || removed != 2 || freed != 2048 {
		t.Fatalf("removed=%d freed=%d err=%v, want 2/2048/nil", removed, freed, err)
	}
	for _, name := range []string{"pa", "pb"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s (oldest) should be gone", name)
		}
	}
	for _, name := range []string{"pc", "pd"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s (newest) should remain: %v", name, err)
		}
	}
}

func TestPruneNeverTouchesAPageStillBeingWritten(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mkfile(t, filepath.Join(dir, "fresh"), 1024, now.Add(-5*time.Second)) // younger than MinAge
	files, used, _ := Scan(dir)
	removed, _, err := Prune(files, used, 0, now, MinAge, nil)
	if err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v: a page younger than MinAge must survive even when over target", removed, err)
	}
}

func TestPruneBelowTargetIsANoOp(t *testing.T) {
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "p"), 1024, time.Now().Add(-time.Hour))
	files, used, _ := Scan(dir)
	if removed, _, _ := Prune(files, used, used, time.Now(), MinAge, nil); removed != 0 {
		t.Fatalf("removed %d at target: want 0", removed)
	}
}

func TestTickPrunesOnlyAboveHighAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		mkfile(t, filepath.Join(dir, "p"+string(rune('a'+i))), 1000, now.Add(-time.Duration(10-i)*time.Hour))
	}
	// Volume: 10 KB used, 0 free → cap 8000, high 7600, low 6800: prune to ≤ 6800.
	s := New(dir, 0, 8, func(string) (uint64, error) { return 0, nil })
	s.now = func() time.Time { return now }
	st := s.Tick()
	if st.Error != "" {
		t.Fatalf("tick error: %s", st.Error)
	}
	if st.Prunes != 1 || st.LastRemoved != 4 || st.Files != 6 {
		t.Fatalf("prunes=%d removed=%d files=%d, want 1/4/6 (10000 → 6000 ≤ 6800)", st.Prunes, st.LastRemoved, st.Files)
	}
	if st.LastPrune == "" || st.LastScan == "" {
		t.Fatalf("timestamps not stamped: %+v", st)
	}
	// Second tick: 6 KB used, plenty free → below high → no prune.
	s.free = func(string) (uint64, error) { return 100e9, nil }
	st = s.Tick()
	if st.Prunes != 1 || st.Files != 6 {
		t.Fatalf("second tick pruned: %+v", st)
	}
}

func TestJobDoneTicksEveryNJobsInTheBackground(t *testing.T) {
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "p"), 1024, time.Now().Add(-time.Hour))
	s := New(dir, 0, 3, func(string) (uint64, error) { return 100e9, nil })
	if s.JobDone() || s.JobDone() {
		t.Fatal("a tick before the third job")
	}
	if !s.JobDone() {
		t.Fatal("the third job must start a tick")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.Status(); st.LastScan != "" && st.Files == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background tick never landed: %+v", s.Status())
}

func TestTickReportsAScanErrorInsteadOfHidingIt(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "missing"), 0, 8, func(string) (uint64, error) { return 1, nil })
	st := s.Tick()
	if st.Error == "" {
		t.Fatalf("a missing root must be an advertised error, got %+v", st)
	}
	if err := Validate(s.root); err == nil {
		t.Fatal("Validate must refuse a missing root")
	}
}

func TestValidateRefusesAPopulatedRootWithoutTheMarker(t *testing.T) {
	// The mistyped-config case: a populated directory that is NOT a store.
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "precious"), 10, time.Now().Add(-time.Hour))
	if err := Validate(dir); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("populated root without marker: err = %v, want ErrNoMarker", err)
	}
	// The operator confirms on purpose: marker present -> accepted.
	if err := os.WriteFile(filepath.Join(dir, MarkerFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(dir); err != nil {
		t.Fatalf("marked root refused: %v", err)
	}
	// A fresh (empty) root gets the marker written.
	empty := t.TempDir()
	if err := Validate(empty); err != nil {
		t.Fatalf("empty root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, MarkerFile)); err != nil {
		t.Fatal("Validate must write the marker into an empty root")
	}
}

func TestScanSkipsTheMarkerAndPruneLogsEveryRemoval(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// The marker is the OLDEST file in the store by mtime and must never be a prune candidate.
	mkfile(t, filepath.Join(dir, MarkerFile), 0, now.Add(-48*time.Hour))
	mkfile(t, filepath.Join(dir, "old"), 1024, now.Add(-3*time.Hour))
	mkfile(t, filepath.Join(dir, "new"), 1024, now.Add(-2*time.Hour))
	files, used, err := Scan(dir)
	if err != nil || len(files) != 2 || used != 2048 {
		t.Fatalf("scan: files=%d used=%d err=%v (the marker must be excluded)", len(files), used, err)
	}
	var lines []string
	removed, _, err := Prune(files, used, 1024, now, MinAge, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], filepath.Join(dir, "old")) {
		t.Fatalf("every removal must be logged with its path: %v", lines)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err != nil {
		t.Fatal("the marker was removed")
	}
}
