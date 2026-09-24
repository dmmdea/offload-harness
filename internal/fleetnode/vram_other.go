//go:build !windows

package fleetnode

import "fmt"

// TreeDedicatedGiB is the non-Windows stub for the PDH per-process tree
// sampler (vram_windows.go): the "\GPU Process Memory" counter set is a WDDM
// facility, so off-Windows it always errors and callers fall back to the
// nvidia-smi global-delta source. Present so portable callers (the pipeline's
// sampler composition) compile everywhere; sampler mode "auto" never selects
// the tree source off-Windows anyway.
func TreeDedicatedGiB(rootPid int) (float64, error) {
	return 0, fmt.Errorf("per-process GPU memory sampling requires Windows PDH counters (pid %d)", rootPid)
}

// TreeDedicatedPlusSharedGiB (fleet_sampler "pdh-shared", J3) is likewise a
// WDDM/PDH facility — off-Windows it always errors.
func TreeDedicatedPlusSharedGiB(rootPid int) (float64, error) {
	return 0, fmt.Errorf("per-process GPU memory sampling requires Windows PDH counters (pid %d)", rootPid)
}

// GenericWindowsProbe (the windows-generic health source, J3) has no
// off-Windows implementation: the stub returns a probe that always errors, so
// ResolveProvider's fallback fails honestly and the gate reports "no working
// GPU memory source" instead of pretending.
func GenericWindowsProbe(uma bool) MemProbe {
	return func() (float64, float64, error) {
		return 0, 0, fmt.Errorf("windows-generic GPU memory source requires WDDM (uma=%v)", uma)
	}
}

// DedicatedVramTotalGiB is a WDDM registry read — off-Windows it always
// errors (the UMA heuristic in fleet-serve then simply doesn't fire).
func DedicatedVramTotalGiB() (float64, error) {
	return 0, fmt.Errorf("dedicated VRAM capacity read requires the Windows display-class registry")
}

// AllProcessDedicatedMiB is the non-Windows stub for the foreign-GPU-holder
// read (D-1xx-4, 2026-09-23): the "\GPU Process Memory" counter set is a WDDM
// facility. Off-Windows the caller (internal/gpuactivity) falls back to
// nvidia-smi's compute-apps query, which DOES report real per-process names
// and memory on Linux — the gap this exists to close is Windows-only (on
// WDDM, nvidia-smi's own per-process memory is `[N/A]`).
func AllProcessDedicatedMiB() ([]ProcessDedicated, error) {
	return nil, fmt.Errorf("per-process GPU memory sampling requires Windows PDH counters")
}

// ProcessNames is the non-Windows stub; the caller falls back to
// nvidia-smi's own process_name column, which needs no separate lookup.
func ProcessNames() (map[int]string, error) {
	return nil, fmt.Errorf("process name enumeration requires Windows CreateToolhelp32Snapshot")
}
