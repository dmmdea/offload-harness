// Package writedoor turns "what did the seat change under this directory" into
// a unified diff the CALLER reviews and applies — the harness never applies
// one (register D-06). It is the delegation lane's whole write-back mechanism:
// a delegated implementation leg runs against the node's own throwaway copy of
// the context docs, and the only thing that crosses back is the text of the
// change plus the list of paths it touched.
//
// Why a before/after TREE snapshot rather than an effect ledger of the tool
// calls: the ledger records that write_file ran, not what the bytes became, and
// a seat that writes a file it was never asked to touch is invisible in it. The
// snapshot is authoritative about the world — it sees every write however it
// arrived, including one the model made and then partly undid.
package writedoor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SnapshotMaxFileBytes bounds ONE file read into a snapshot. A file larger than
// this is recorded as an opaque size marker rather than content: it cannot be
// line-diffed usefully anyway, and reading it would put the node's memory at
// the mercy of whatever the seat wrote. The marker still makes the file show up
// as CHANGED, which is the property that matters — a cap must never hide a
// mutation.
const SnapshotMaxFileBytes = 1 << 20

// Snapshot is one directory tree's content, keyed by slash-separated path
// relative to the snapshot root. A nil Snapshot is the empty tree.
type Snapshot map[string]string

// opaque renders the marker recorded for a file that could not be read as
// content (too large, or unreadable). It is deliberately content-derived, so
// two different oversize versions of a file compare UNEQUAL.
func opaque(size int64, why string) string {
	return fmt.Sprintf("\x00writedoor-opaque %d %s", size, why)
}

// IsOpaque reports whether a snapshot entry is a marker rather than real bytes.
func IsOpaque(s string) bool { return strings.HasPrefix(s, "\x00writedoor-opaque ") }

// Take walks root and records every regular file under it. Directories,
// symlinks and every other non-regular entry are SKIPPED, not followed: a
// symlink is a pointer out of the tree, and a write door that diffed through
// one would publish — and invite the caller to apply — a change to a file
// outside the root it was confined to.
//
// A missing root is the empty snapshot, not an error: the write root is created
// on demand, so "not there yet" is a legitimate before-state.
func Take(root string) (Snapshot, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	snap := Snapshot{}
	if _, serr := os.Stat(abs); os.IsNotExist(serr) {
		return snap, nil
	}
	err = filepath.Walk(abs, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		// Mode().IsRegular() is false for symlinks, devices, sockets and pipes.
		// Walk uses Lstat, so a symlink arrives here unfollowed and is dropped.
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(abs, path)
		if rerr != nil {
			return rerr
		}
		key := filepath.ToSlash(rel)
		if info.Size() > SnapshotMaxFileBytes {
			snap[key] = opaque(info.Size(), "too large to diff")
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			snap[key] = opaque(info.Size(), "unreadable")
			return nil //nolint:nilerr // an unreadable file is recorded as changed, never dropped
		}
		data, derr := io.ReadAll(io.LimitReader(f, SnapshotMaxFileBytes+1))
		f.Close()
		if derr != nil {
			snap[key] = opaque(info.Size(), "unreadable")
			return nil
		}
		snap[key] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// Change is one path's fate between two snapshots.
type Change struct {
	Path string // slash-separated, relative to the snapshot root
	Kind string // "added", "modified", "deleted"
	Before string
	After  string
}

// Changes returns every path whose content differs, sorted by path so the diff
// and the touched-path list are deterministic — a diff whose hunk order depends
// on map iteration is not reviewable and not comparable between runs.
func Changes(before, after Snapshot) []Change {
	seen := make(map[string]bool, len(before)+len(after))
	for p := range before {
		seen[p] = true
	}
	for p := range after {
		seen[p] = true
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]Change, 0, len(paths))
	for _, p := range paths {
		b, hadB := before[p]
		a, hadA := after[p]
		if hadB && hadA {
			if b == a {
				continue
			}
			out = append(out, Change{Path: p, Kind: "modified", Before: b, After: a})
			continue
		}
		if hadA {
			out = append(out, Change{Path: p, Kind: "added", After: a})
			continue
		}
		out = append(out, Change{Path: p, Kind: "deleted", Before: b})
	}
	return out
}

// Unified renders the changes as one git-style unified diff and returns the
// touched paths beside it. Paths are prefixed a/ and b/ so the output applies
// with `git apply -p1` / `patch -p1`, the two things a reviewer actually reaches
// for. An empty change list renders as the empty string, never as a diff with
// no hunks — "nothing changed" must be distinguishable at a glance.
func Unified(changes []Change) (diff string, files []string) {
	if len(changes) == 0 {
		return "", nil
	}
	var b strings.Builder
	files = make([]string, 0, len(changes))
	for _, c := range changes {
		files = append(files, c.Path)
		oldName, newName := "a/"+c.Path, "b/"+c.Path
		switch c.Kind {
		case "added":
			oldName = "/dev/null"
		case "deleted":
			newName = "/dev/null"
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", c.Path, c.Path)
		// git needs the mode line to read /dev/null as a creation or deletion
		// rather than as a literal path (with -p1 it strips the leading
		// component and looks for "dev/null", which does not exist).
		switch c.Kind {
		case "added":
			b.WriteString("new file mode 100644\n")
		case "deleted":
			b.WriteString("deleted file mode 100644\n")
		}
		if IsOpaque(c.Before) || IsOpaque(c.After) {
			// An opaque side cannot be line-diffed. Say so in the diff itself
			// rather than emitting a hunk built from a marker string, which
			// would apply as garbage.
			fmt.Fprintf(&b, "--- %s\n+++ %s\n", oldName, newName)
			fmt.Fprintf(&b, "Binary or oversize file %s differs (not rendered; %s)\n", c.Path, c.Kind)
			continue
		}
		fmt.Fprintf(&b, "--- %s\n+++ %s\n", oldName, newName)
		b.WriteString(hunks(c.Before, c.After))
	}
	return b.String(), files
}
