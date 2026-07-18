package fleetnode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFootprintsRecordMaxKeep locks the store's core rule: observed peaks keep
// the max, vram_peak_gb = max observed × 1.2 rounded to 0.1, samples counted.
func TestFootprintsRecordMaxKeep(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "footprints.json"))

	f.Record("sdxl", "bf16", "image-gen", 2.0)
	entries := f.Entries()
	if len(entries) != 1 {
		t.Fatalf("Entries() len = %d, want 1", len(entries))
	}
	if entries[0].VramPeakGiB != 2.4 {
		t.Errorf("peak after 2.0 = %v, want 2.4 (2.0 x 1.2)", entries[0].VramPeakGiB)
	}

	f.Record("sdxl", "bf16", "image-gen", 1.5) // lower — must not regress
	if got := f.Entries()[0].VramPeakGiB; got != 2.4 {
		t.Errorf("peak after lower observation = %v, want 2.4", got)
	}

	f.Record("sdxl", "bf16", "image-gen", 3.0) // higher — must advance
	if got := f.Entries()[0].VramPeakGiB; got != 3.6 {
		t.Errorf("peak after 3.0 = %v, want 3.6", got)
	}
}

// TestFootprintsRounding locks x1.2-rounded-to-0.1: the contract wants clean
// numbers, and we must never double-pad beyond the recorded x1.2.
func TestFootprintsRounding(t *testing.T) {
	cases := []struct {
		observed float64
		want     float64
	}{
		{3.333, 4.0},  // 3.9996 -> 4.0
		{1.04, 1.2},   // 1.248 -> 1.2
		{10.0, 12.0},  // exact
		{0.51, 0.6},   // 0.612 -> 0.6
		{13.29, 15.9}, // 15.948 -> 15.9
	}
	for _, tc := range cases {
		f := OpenFootprints(filepath.Join(t.TempDir(), "fp.json"))
		f.Record("fam", "", "image-gen", tc.observed)
		if got := f.Entries()[0].VramPeakGiB; got != tc.want {
			t.Errorf("Record(%v): vram_peak_gb = %v, want %v", tc.observed, got, tc.want)
		}
	}
}

// TestFootprintsIgnoresNonPositive locks the never-write-zero-peaks contract
// rule: vram_peak_gb <= 0 entries are ignored by the dispatcher, so a
// non-positive observation must never create one.
func TestFootprintsIgnoresNonPositive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fp.json")
	f := OpenFootprints(path)
	f.Record("fam", "", "image-gen", 0)
	f.Record("fam", "", "image-gen", -1.5)
	if got := f.Entries(); len(got) != 0 {
		t.Errorf("Entries() after non-positive observations = %+v, want empty", got)
	}
	if _, err := os.Stat(path); err == nil {
		// A file is fine only if it holds no entries.
		if entries := OpenFootprints(path).Entries(); len(entries) != 0 {
			t.Errorf("persisted entries from non-positive observations: %+v", entries)
		}
	}
}

// TestFootprintsWireShape locks the JSON tags: model_family + vram_peak_gb
// always present; quant and task_type omitted when empty (omitempty).
func TestFootprintsWireShape(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "fp.json"))
	f.Record("whisper", "", "", 1.0)
	f.Record("wan2.2", "q8_0", "video-gen", 10.0)

	b, err := json.Marshal(f.Entries())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"model_family":"whisper"`) || !strings.Contains(s, `"vram_peak_gb":1.2`) {
		t.Errorf("wire JSON missing required fields: %s", s)
	}
	if !strings.Contains(s, `"quant":"q8_0"`) || !strings.Contains(s, `"task_type":"video-gen"`) {
		t.Errorf("wire JSON missing set optional fields: %s", s)
	}
	// The whisper entry has empty quant/task_type — they must be ABSENT, not "".
	var raw []map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, m := range raw {
		if m["model_family"] == "whisper" {
			if _, ok := m["quant"]; ok {
				t.Errorf("empty quant not omitted: %v", m)
			}
			if _, ok := m["task_type"]; ok {
				t.Errorf("empty task_type not omitted: %v", m)
			}
		}
	}
}

// TestFootprintsPersistAndReopen locks the atomic temp+rename write: what one
// store records, a fresh store opened on the same path sees, and no temp file
// is left behind.
func TestFootprintsPersistAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "footprints.json")

	f := OpenFootprints(path)
	f.Record("sdxl", "bf16", "image-gen", 6.5)
	f.Record("acestep", "", "audio-gen", 4.25)
	f.Record("sdxl", "bf16", "image-gen", 7.0) // advance the max

	g := OpenFootprints(path)
	entries := g.Entries()
	if len(entries) != 2 {
		t.Fatalf("reopened Entries() len = %d, want 2 (%+v)", len(entries), entries)
	}
	byFam := map[string]FootprintEntry{}
	for _, e := range entries {
		byFam[e.ModelFamily] = e
	}
	if got := byFam["sdxl"].VramPeakGiB; got != 8.4 {
		t.Errorf("reopened sdxl peak = %v, want 8.4 (7.0 x 1.2)", got)
	}
	if got := byFam["acestep"].VramPeakGiB; got != 5.1 {
		t.Errorf("reopened acestep peak = %v, want 5.1 (4.25 x 1.2)", got)
	}

	// Max-keep must survive the reopen too: a lower observation on g does not regress.
	g.Record("sdxl", "bf16", "image-gen", 5.0)
	if got := OpenFootprints(path).Entries(); len(got) != 2 {
		t.Fatalf("third open lost entries: %+v", got)
	} else {
		for _, e := range got {
			if e.ModelFamily == "sdxl" && e.VramPeakGiB != 8.4 {
				t.Errorf("sdxl peak regressed across reopen: %v, want 8.4", e.VramPeakGiB)
			}
		}
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, fi := range files {
		if fi.Name() != "footprints.json" {
			t.Errorf("leftover file after atomic writes: %s", fi.Name())
		}
	}
}

// TestFootprintsCorruptRecovery locks never-crash-on-corrupt: garbage on disk
// means start empty, and the store still records + persists afterwards.
func TestFootprintsCorruptRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footprints.json")
	if err := os.WriteFile(path, []byte("{not json!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := OpenFootprints(path)
	if got := f.Entries(); len(got) != 0 {
		t.Errorf("corrupt file should open empty, got %+v", got)
	}
	f.Record("sdxl", "", "image-gen", 5.0)
	if got := OpenFootprints(path).Entries(); len(got) != 1 || got[0].VramPeakGiB != 6.0 {
		t.Errorf("store did not recover to a working state: %+v", got)
	}
}

// TestFootprintsFiltersNonPositiveOnLoad locks the Entries() filter: a
// hand-edited or legacy file entry with vram_peak_gb <= 0 never reaches the
// wire (the dispatcher would ignore it; we never advertise it).
func TestFootprintsFiltersNonPositiveOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footprints.json")
	disk := `[
		{"model_family":"broken","vram_peak_gb":0,"observed_peak_gb":0,"samples":1,"updated":"2026-07-17T00:00:00Z"},
		{"model_family":"good","task_type":"image-gen","vram_peak_gb":2.4,"observed_peak_gb":2.0,"samples":3,"updated":"2026-07-17T00:00:00Z"}
	]`
	if err := os.WriteFile(path, []byte(disk), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := OpenFootprints(path).Entries()
	if len(entries) != 1 || entries[0].ModelFamily != "good" {
		t.Errorf("Entries() = %+v, want only the >0-peak entry", entries)
	}
}

// TestFootprintsConcurrentRecord exercises concurrent Record/Entries under
// -race: gpugen callbacks can fire while health reads Entries.
func TestFootprintsConcurrentRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footprints.json")
	f := OpenFootprints(path)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 1; j <= 20; j++ {
				f.Record("fam", "q", "image-gen", float64(n)+float64(j)/20)
				_ = f.Entries()
			}
		}(i)
	}
	wg.Wait()
	entries := f.Entries()
	if len(entries) != 1 {
		t.Fatalf("Entries() len = %d, want 1", len(entries))
	}
	// Max observed = 7 + 20/20 = 8.0 -> peak 9.6.
	if entries[0].VramPeakGiB != 9.6 {
		t.Errorf("concurrent max peak = %v, want 9.6", entries[0].VramPeakGiB)
	}
	if got := OpenFootprints(path).Entries(); len(got) != 1 || got[0].VramPeakGiB != 9.6 {
		t.Errorf("persisted concurrent result = %+v, want single 9.6 entry", got)
	}
}

// Live-found bug: fleet-serve held its startup (empty) store while fleet-measure
// (another process) wrote records — health served 0 footprints. ReloadIfChanged
// must surface the other process's records (mtime-gated) and never regress a
// higher in-memory observation.
func TestReloadIfChangedMergesOtherProcessRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footprints.json")
	server := OpenFootprints(path) // the long-running process, opened when file absent
	writer := OpenFootprints(path) // "fleet-measure" in another process
	writer.Record("hidream-o1", "bf16", "image-gen", 12.5)

	// Ensure a later mtime than any previous stat granularity.
	old := time.Now().Add(-1 * time.Minute)
	_ = os.Chtimes(path, old, time.Now())

	server.ReloadIfChanged()
	entries := server.Entries()
	if len(entries) != 1 || entries[0].ModelFamily != "hidream-o1" {
		t.Fatalf("server must see the other process's record, got %+v", entries)
	}
	if entries[0].VramPeakGiB != 15.0 {
		t.Fatalf("merged record keeps the x1.2 peak: got %v want 15.0", entries[0].VramPeakGiB)
	}

	// A higher IN-MEMORY observation must survive a reload of an older file.
	server.Record("hidream-o1", "bf16", "image-gen", 14.0)
	_ = os.Chtimes(path, old, time.Now().Add(2*time.Second))
	server.ReloadIfChanged()
	for _, e := range server.Entries() {
		if e.ModelFamily == "hidream-o1" && e.VramPeakGiB < 16.8 {
			t.Fatalf("reload regressed a newer in-memory max: %+v", e)
		}
	}
}
