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
