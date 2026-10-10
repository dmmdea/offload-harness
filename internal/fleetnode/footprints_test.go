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
// the max, vram_peak_gb = RAW max observed rounded to 0.1 (no node padding; the
// dispatcher owns all margin — ADR 0013), samples counted.
func TestFootprintsRecordMaxKeep(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "footprints.json"))

	f.Record("sdxl", "bf16", "image-gen", 2.0)
	entries := f.Entries()
	if len(entries) != 1 {
		t.Fatalf("Entries() len = %d, want 1", len(entries))
	}
	if entries[0].VramPeakGiB != 2.0 {
		t.Errorf("peak after 2.0 = %v, want 2.0 (raw peak, no node padding)", entries[0].VramPeakGiB)
	}

	f.Record("sdxl", "bf16", "image-gen", 1.5) // lower — must not regress
	if got := f.Entries()[0].VramPeakGiB; got != 2.0 {
		t.Errorf("peak after lower observation = %v, want 2.0", got)
	}

	f.Record("sdxl", "bf16", "image-gen", 3.0) // higher — must advance
	if got := f.Entries()[0].VramPeakGiB; got != 3.0 {
		t.Errorf("peak after 3.0 = %v, want 3.0", got)
	}
}

// TestFootprintsRounding locks raw-observed-rounded-to-0.1: the contract wants
// clean numbers, and the node adds NO padding — the dispatcher owns all margin.
func TestFootprintsRounding(t *testing.T) {
	cases := []struct {
		observed float64
		want     float64
	}{
		{3.333, 3.3},  // 3.333 -> 3.3
		{1.04, 1.0},   // 1.04 -> 1.0
		{10.0, 10.0},  // exact
		{0.51, 0.5},   // 0.51 -> 0.5
		{13.29, 13.3}, // 13.29 -> 13.3
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
	if !strings.Contains(s, `"model_family":"whisper"`) || !strings.Contains(s, `"vram_peak_gb":1}`) {
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
	if got := byFam["sdxl"].VramPeakGiB; got != 7.0 {
		t.Errorf("reopened sdxl peak = %v, want 7.0 (raw)", got)
	}
	if got := byFam["acestep"].VramPeakGiB; got != 4.3 {
		t.Errorf("reopened acestep peak = %v, want 4.3 (4.25 raw, rounded to 0.1)", got)
	}

	// Max-keep must survive the reopen too: a lower observation on g does not regress.
	g.Record("sdxl", "bf16", "image-gen", 5.0)
	if got := OpenFootprints(path).Entries(); len(got) != 2 {
		t.Fatalf("third open lost entries: %+v", got)
	} else {
		for _, e := range got {
			if e.ModelFamily == "sdxl" && e.VramPeakGiB != 7.0 {
				t.Errorf("sdxl peak regressed across reopen: %v, want 7.0", e.VramPeakGiB)
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
	if got := OpenFootprints(path).Entries(); len(got) != 1 || got[0].VramPeakGiB != 5.0 {
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
		{"model_family":"good","task_type":"image-gen","vram_peak_gb":2.0,"observed_peak_gb":2.0,"samples":3,"updated":"2026-07-17T00:00:00Z"}
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
	// Max observed = 7 + 20/20 = 8.0 -> raw peak 8.0 (no node padding).
	if entries[0].VramPeakGiB != 8.0 {
		t.Errorf("concurrent max peak = %v, want 8.0", entries[0].VramPeakGiB)
	}
	if got := OpenFootprints(path).Entries(); len(got) != 1 || got[0].VramPeakGiB != 8.0 {
		t.Errorf("persisted concurrent result = %+v, want single 8.0 entry", got)
	}
}

// TestConcurrentProcessWritesMerge reproduces the live <node-a> symptom: fleet-measure
// and the MCP server share ~/.local-offload/footprints.json as separate processes.
// A write must MERGE the on-disk state, not overwrite it with only this process's
// in-memory entries — the reported "only comfy-graph advertised" was the MCP's
// Record clobbering fleet-measure's freshly-measured hidream record.
func TestConcurrentProcessWritesMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footprints.json")
	measure := OpenFootprints(path) // "fleet-measure" process
	server := OpenFootprints(path)  // "MCP" process — both opened while the file was absent
	measure.Record("hidream-o1", "bf16", "image-gen", 12.5)
	server.Record("comfy-graph", "", "run-graph", 0.5) // must NOT clobber the hidream record on disk
	final := OpenFootprints(path).Entries()
	if len(final) != 2 {
		t.Fatalf("a concurrent process's write clobbered a record: disk has %d entries, want 2 (hidream-o1 + comfy-graph): %+v", len(final), final)
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
	if entries[0].VramPeakGiB != 12.5 {
		t.Fatalf("merged record keeps the raw peak: got %v want 12.5", entries[0].VramPeakGiB)
	}

	// A higher IN-MEMORY observation must survive a reload of an older file.
	server.Record("hidream-o1", "bf16", "image-gen", 14.0)
	_ = os.Chtimes(path, old, time.Now().Add(2*time.Second))
	server.ReloadIfChanged()
	for _, e := range server.Entries() {
		if e.ModelFamily == "hidream-o1" && e.VramPeakGiB < 14.0 {
			t.Fatalf("reload regressed a newer in-memory max: %+v", e)
		}
	}
}

// --- host peaks (G3 of the P0 plan: the guard sizes a render from what renders were measured to hold) ---

// RecordHost keeps the MAX private and the MAX resident peak per key and counts the runs; HostPeak reads them back.
func TestFootprintsRecordHostPeaksMaxKeep(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "footprints.json"))
	if _, ok := f.HostPeak("krea2", "bf16", "image-gen"); ok {
		t.Fatal("a key nobody recorded has no host peak")
	}
	f.RecordHost("krea2", "bf16", "image-gen", 40.0, 30.0)
	f.RecordHost("krea2", "bf16", "image-gen", 57.7, 33.0) // higher private, lower resident than the next
	f.RecordHost("krea2", "bf16", "image-gen", 50.0, 40.7)
	got, ok := f.HostPeak("krea2", "bf16", "image-gen")
	if !ok || got.PrivateGiB != 57.7 || got.ResidentGiB != 40.7 || got.Runs != 3 {
		t.Fatalf("want private 57.7 (max), resident 40.7 (max, from another run), 3 runs; got %+v ok=%v", got, ok)
	}
	// A non-positive private peak is a sampling failure: dropped, and not counted as a run.
	f.RecordHost("krea2", "bf16", "image-gen", 0, 5)
	f.RecordHost("krea2", "bf16", "image-gen", -1, 5)
	if got, _ := f.HostPeak("krea2", "bf16", "image-gen"); got.Runs != 3 {
		t.Fatalf("a failed sample counted as a run: %+v", got)
	}
}

// The wire shape gains host_peak_gb and host_resident_peak_gb for an entry that has a VRAM peak and host peaks,
// and a store without host data is byte-identical to before (the keys are omitted, not zero).
func TestFootprintsWireCarriesHostPeaksOnlyWhenMeasured(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "fp.json"))
	f.Record("sdxl", "", "image-gen", 6.0)
	f.Record("krea2", "bf16", "image-gen", 15.0)
	f.RecordHost("krea2", "bf16", "image-gen", 57.7, 40.7)
	b, err := json.Marshal(f.Entries())
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, m := range raw {
		switch m["model_family"] {
		case "sdxl":
			if _, ok := m["host_peak_gb"]; ok {
				t.Errorf("an entry with no host data must not carry host keys: %v", m)
			}
		case "krea2":
			if m["host_peak_gb"] != 57.7 || m["host_resident_peak_gb"] != 40.7 {
				t.Errorf("host peaks missing from the wire entry: %v", m)
			}
		}
	}
}

// Host peaks survive a reopen, and the cross-process merge is a per-field max: another process's higher host peak
// and run count arrive, and neither process's VRAM peak or host peak is regressed by the other's write.
func TestFootprintsHostPeaksPersistAndMergeAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footprints.json")
	a := OpenFootprints(path)
	b := OpenFootprints(path) // another process, opened while the file was absent
	a.Record("krea2", "bf16", "image-gen", 15.0)
	a.RecordHost("krea2", "bf16", "image-gen", 50.0, 35.0)
	b.RecordHost("krea2", "bf16", "image-gen", 57.7, 30.0) // must not clobber a's VRAM peak, and must merge a's resident

	re := OpenFootprints(path)
	got, ok := re.HostPeak("krea2", "bf16", "image-gen")
	if !ok || got.PrivateGiB != 57.7 || got.ResidentGiB != 35.0 {
		t.Fatalf("a reopened store keeps the max of each host field across processes: %+v ok=%v", got, ok)
	}
	if e := re.Entries(); len(e) != 1 || e[0].VramPeakGiB != 15.0 {
		t.Fatalf("a concurrent host write clobbered the VRAM peak: %+v", e)
	}
	// An old store (no host fields) opens with none, rather than failing.
	old := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(old, []byte(`[{"model_family":"sdxl","vram_peak_gb":6,"observed_peak_gb":6,"samples":2,"updated":"2026-10-01T00:00:00Z"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := OpenFootprints(old).HostPeak("sdxl", "", ""); ok {
		t.Fatal("an old record has no host peak")
	}
}
