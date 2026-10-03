//go:build windows

package comfyinst

import (
	"os"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// terminate stops pid. Windows has no gentler signal for a console process than
// TerminateProcess, which is what Process.Kill calls; ComfyUI is a single python process, and
// the render layer's own replace path (comfy-lifecycle.mjs stopComfy) does the same.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// processStartUnixMs is when pid began, in Unix milliseconds. On Windows the creation time
// gpulease records is exactly that.
func processStartUnixMs(pid int) (int64, bool) { return gpulease.ProcessStart(pid) }
