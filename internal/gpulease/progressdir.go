package gpulease

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProgressDir is where `gpu reserve` mints the progress file of a lease it declares a contract for:
// <state root>/progress, beside owners/ and gpu/. Derived from the state root (Manager.Root) and not from
// the lease directory, which a gpu_lock_path override can put anywhere. The wrapper records the file's
// absolute path on the lease, so no reader needs to derive this directory again.
func ProgressDir(root string) string { return filepath.Join(root, "progress") }

// MintedProgressPrefix and MintedProgressSuffix frame the file name of a progress file the wrapper minted
// (wrap-<pid>-<unix nanos>.jsonl). SweepMintedProgress deletes only names of this shape, so a file the
// caller keeps under a directory of its own choosing is never touched.
const (
	MintedProgressPrefix = "wrap-"
	MintedProgressSuffix = ".jsonl"
)

// IsMintedProgressFile reports whether path is a progress file of the wrapper's own minted shape
// (wrap-<pid>-<unix nanos>.jsonl). Such a file exists only between the start of the wrapped command and its
// end: a reader that finds it missing is looking at a lease that is queued, draining, unloading the seat
// or warming it back, not at a job that forgot to write.
func IsMintedProgressFile(path string) bool {
	return isMintedName(filepath.Base(path))
}

func isMintedName(name string) bool {
	return strings.HasPrefix(name, MintedProgressPrefix) && strings.HasSuffix(name, MintedProgressSuffix)
}

// SweepMintedProgress removes the minted progress files in dir that nothing has written to for maxAge. A
// wrapper removes its own file when its command ends, so what is left here was left by a wrapper that was
// killed (a closed console, a taskkill of the tree) before it could. It runs inline in the next
// `gpu reserve` that mints one: no timer, no watcher. It returns how many files it removed; every error
// is swallowed, because a directory that cannot be swept costs disk, never a lease.
func SweepMintedProgress(dir string, maxAge time.Duration, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !isMintedName(name) {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) < maxAge {
			continue
		}
		if os.Remove(filepath.Join(dir, name)) == nil {
			removed++
		}
	}
	return removed
}

// heartbeatHelpers are the render scripts that move a lease's progress file: every runner that calls
// pollOutputs (render/comfy-submit.mjs), and comfy-generate.mjs, whose children do. The wrapper declares a
// progress contract for a command that runs one (gpu_progress.go), and a status reading uses the same list
// to say what a lease with no contract was probably doing. TestHeartbeatHelpersPinTheRenderDirectory keeps
// it equal to what render/ contains: a helper missing here runs under a lease with no contract (the F8
// defect), one listed that does not beat would be labelled stalled.
var heartbeatHelpers = map[string]bool{
	"comfy-generate.mjs":  true,
	"comfy-render.mjs":    true,
	"comfy-edit.mjs":      true,
	"comfy-inpaint.mjs":   true,
	"comfy-video.mjs":     true,
	"comfy-animate.mjs":   true,
	"comfy-music.mjs":     true,
	"comfy-upscale.mjs":   true,
	"comfy-run-graph.mjs": true,
}

// HeartbeatHelperNames is the list, sorted.
func HeartbeatHelperNames() []string {
	out := make([]string, 0, len(heartbeatHelpers))
	for n := range heartbeatHelpers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HeartbeatHelperIn finds a heartbeating helper among the arguments of a command (behind an interpreter,
// its path in either separator style) and returns its file name.
func HeartbeatHelperIn(args []string) (string, bool) {
	for _, a := range args {
		base := strings.ToLower(filepath.Base(strings.ReplaceAll(a, `\`, "/")))
		if heartbeatHelpers[base] {
			return base, true
		}
	}
	return "", false
}

// HeartbeatHelperInCommand is HeartbeatHelperIn for the command line a lease records (its arguments joined
// by single spaces).
func HeartbeatHelperInCommand(command string) (string, bool) {
	return HeartbeatHelperIn(strings.Fields(command))
}
