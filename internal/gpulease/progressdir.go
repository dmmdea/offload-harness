package gpulease

import (
	"os"
	"path/filepath"
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
		if e.IsDir() || !strings.HasPrefix(name, MintedProgressPrefix) || !strings.HasSuffix(name, MintedProgressSuffix) {
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
