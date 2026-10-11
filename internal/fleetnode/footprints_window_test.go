package fleetnode

// F56: the host-RAM calibration took the all-time MAX of the resident peak of a key's renders. One inflated run (a kept
// ComfyUI that was not under the sampled runner the first time and was the second, a driver charging card memory to the
// process, a moment of desktop pressure) raised the family's declaration for good, until footprints.json was edited by hand,
// and a declaration above what the box can ever admit made the lane read "can never admit" for every later call, with
// `gpu reserve --ram` no remedy for a pipeline call. The store now keeps the last hostRecentRuns host peaks of each key
// and HostPeak answers with a HIGH PERCENTILE of them (nearest rank, hostPeakPercentile) so one outlier among ten or more
// runs is not the answer, and a bad run leaves the window after hostRecentRuns more. The wire fields stay the all-time max
// ("a peak is a peak"): only the calibration reads the percentile.

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLegacyHostStore(t *testing.T, path string) {
	t.Helper()
	body := `[{"model_family":"krea2","quant":"bf16","task_type":"image-gen","vram_peak_gb":12,"observed_peak_gb":12,"samples":5,` +
		`"updated":"2026-10-01T00:00:00Z","host_peak_gb":57.7,"host_resident_peak_gb":40.7,"host_runs":5}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func record(f *Footprints, n int, private, resident float64) {
	for i := 0; i < n; i++ {
		f.RecordHost("krea2", "bf16", "image-gen", private, resident)
	}
}

func TestHostPeakIsAHighPercentileOfTheRecentRunsNotTheAllTimeMax(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "footprints.json"))
	f.RecordHost("krea2", "bf16", "image-gen", 200, 90) // the outlier, first
	record(f, 11, 50, 32)
	got, ok := f.HostPeak("krea2", "bf16", "image-gen")
	if !ok || got.Runs != 12 {
		t.Fatalf("HostPeak = %+v ok=%v, want 12 runs", got, ok)
	}
	if got.ResidentGiB != 32 || got.PrivateGiB != 50 {
		t.Errorf("one outlier among twelve runs is not the calibration figure: resident %.1f private %.1f, want 32 and 50", got.ResidentGiB, got.PrivateGiB)
	}
	if got.MaxResidentGiB != 90 || got.MaxPrivateGiB != 200 {
		t.Errorf("the all-time peaks are still reported beside it: %+v", got)
	}
	// The wire keeps the peak: a dispatcher reading host_resident_peak_gb sees the largest run, as it always did.
	es := f.Entries()
	if len(es) != 0 {
		t.Errorf("host peaks alone make no VRAM entry (vram_peak_gb must be > 0): %+v", es)
	}
	f.Record("krea2", "bf16", "image-gen", 12)
	es = f.Entries()
	if len(es) != 1 || es[0].HostResidentPeakGiB != 90 || es[0].HostPeakGiB != 200 {
		t.Errorf("the wire fields stay the all-time max: %+v", es)
	}
}

// Under ten runs the 90th percentile of the window IS its maximum (nearest rank): outlier rejection starts where there is
// something to reject it against, and below that the calibration errs toward declaring more, as it always did.
func TestWithFewerThanTenRunsThePercentileIsTheMaximum(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "footprints.json"))
	f.RecordHost("krea2", "bf16", "image-gen", 200, 90)
	record(f, 8, 50, 32) // nine runs
	got, _ := f.HostPeak("krea2", "bf16", "image-gen")
	if got.ResidentGiB != 90 {
		t.Errorf("nine runs: the percentile is the maximum, 90, got %.1f", got.ResidentGiB)
	}
	record(f, 1, 50, 32) // the tenth
	got, _ = f.HostPeak("krea2", "bf16", "image-gen")
	if got.ResidentGiB != 32 {
		t.Errorf("ten runs: the outlier is above the 90th percentile, got %.1f", got.ResidentGiB)
	}
}

// A bad run leaves the window after hostRecentRuns more runs, even when the percentile never rejected it.
func TestAnOutlierAgesOutOfTheWindow(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "footprints.json"))
	f.RecordHost("krea2", "bf16", "image-gen", 200, 90)
	record(f, 3, 50, 32) // four runs: the outlier is the maximum and the answer
	if got, _ := f.HostPeak("krea2", "bf16", "image-gen"); got.ResidentGiB != 90 {
		t.Fatalf("control: four runs, the outlier is the maximum: %.1f", got.ResidentGiB)
	}
	record(f, hostRecentRuns, 50, 32)
	got, _ := f.HostPeak("krea2", "bf16", "image-gen")
	if got.ResidentGiB != 32 || got.Window != hostRecentRuns {
		t.Errorf("after %d more runs the outlier has left the window: %+v", hostRecentRuns, got)
	}
	if got.Runs != 4+hostRecentRuns {
		t.Errorf("Runs counts every run ever recorded, not the window: %d", got.Runs)
	}
	if got.MaxResidentGiB != 90 {
		t.Errorf("the all-time peak is kept for the wire: %.1f", got.MaxResidentGiB)
	}
}

// Runs that left a full window are forgotten, not stood in for by the all-time maximum (that is for the runs a legacy store never
// held): twenty-four ascending runs leave the last twenty, and their 90th percentile is the 18th of them, not the 19th a
// twenty-first, stood-in sample would make it.
func TestRunsThatLeftAFullWindowAreForgottenNotStoodInFor(t *testing.T) {
	f := OpenFootprints(filepath.Join(t.TempDir(), "footprints.json"))
	for i := 1; i <= 24; i++ {
		f.RecordHost("krea2", "bf16", "image-gen", float64(i)+10, float64(i))
	}
	got, _ := f.HostPeak("krea2", "bf16", "image-gen")
	if got.Window != 20 || got.ResidentGiB != 22 || got.PrivateGiB != 32 {
		t.Errorf("window 5..24, nearest-rank 90th percentile = the 18th = 22 (private 32): %+v", got)
	}
	if got.MaxResidentGiB != 24 || got.Runs != 24 {
		t.Errorf("the all-time figures are kept for the wire: %+v", got)
	}
}

// The window survives a restart, and a process that opened the store before another wrote to it does not drop that
// process's runs when it records its own (the merge comes before the fold).
func TestTheWindowIsPersistedAndMergedBeforeARunIsFolded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footprints.json")
	a, b := OpenFootprints(path), OpenFootprints(path)
	record(a, 5, 50, 32)
	b.RecordHost("krea2", "bf16", "image-gen", 60, 33) // b opened first and has seen none of a's runs
	re := OpenFootprints(path)
	got, ok := re.HostPeak("krea2", "bf16", "image-gen")
	if !ok || got.Window != 6 || got.Runs != 6 {
		t.Fatalf("the reopened store holds a's five runs and b's: %+v ok=%v, want a window of 6 over 6 runs", got, ok)
	}
}

// A store written before the window existed has the maxima and a run count and no recent list: the calibration reads the
// maxima, as it did, until runs fill the window.
func TestAStoreWithoutAWindowFallsBackToItsMaxima(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footprints.json")
	writeLegacyHostStore(t, path)
	f := OpenFootprints(path)
	got, ok := f.HostPeak("krea2", "bf16", "image-gen")
	if !ok || got.ResidentGiB != 40.7 || got.PrivateGiB != 57.7 || got.Runs != 5 || got.Window != 0 {
		t.Fatalf("a legacy record answers with its maxima over its runs: %+v ok=%v", got, ok)
	}
	f.RecordHost("krea2", "bf16", "image-gen", 10, 8)
	got, _ = f.HostPeak("krea2", "bf16", "image-gen")
	if got.Window != 1 || got.Runs != 6 || got.ResidentGiB != 40.7 {
		t.Errorf("one new run starts the window; until it holds ten runs the legacy maximum still speaks: %+v", got)
	}
}
