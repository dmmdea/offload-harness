// The delegation WRITE door (register D-06), node side. A contract that names
// a write_root gets create+overwrite inside ONE directory of the node's own
// throwaway copy of its context docs, capped, with no delete / shell / run /
// fetch / github — and the only thing that crosses back is a unified diff the
// CALLER reviews and applies. The harness applies nothing.
//
// Why the door lives here and not in internal/agent: agent.Build already knows
// how to grant the write tools; what it does not know is that this particular
// run must be diffed, capped as a whole, and refused outright on a node that
// has not opted in. That is delegation policy, and it belongs beside the rest
// of the contract's execution.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/writedoor"
)

// writeDoor is one run's open write door: where it may write, what was there
// before, and the live tool-level budget.
type writeDoor struct {
	root   string // ABSOLUTE, inside the read root
	rel    string // the contract's write_root as given
	before writedoor.Snapshot
	limit  *agent.WriteLimit
}

// openWriteDoor resolves contract.WriteRoot under readRoot, creates it, and
// snapshots what is there. It returns (nil, nil) for a read-only contract —
// the far more common case, and the one that must cost nothing.
//
// Containment is os.Root (the house idiom: kernel-enforced, fail-closed on
// "..", absolute paths and reparse points) PLUS a resolved-path check. Both,
// because the two answer different questions: os.Root proves the CREATE could
// not escape, and the resolved-path check proves the absolute directory handed
// to the write tools — which open their own os.Root on it — is itself inside
// the read root. A symlinked write root would pass the first and fail the
// second, and the write tools would then be confined to the WRONG tree.
func openWriteDoor(readRoot string, contract core.AgentContract) (*writeDoor, error) {
	if contract.WriteRoot == "" {
		return nil, nil
	}
	// Belt and braces: Validate ran at both doors, but this function hands a
	// directory to the write tools and must not depend on someone else's call.
	if err := core.ValidateWriteRoot(contract.WriteRoot); err != nil {
		return nil, err
	}
	absRead, err := filepath.Abs(readRoot)
	if err != nil {
		return nil, fmt.Errorf("write door: bad read root: %w", err)
	}
	rel := filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(filepath.Clean(contract.WriteRoot)), "./"))

	root, err := os.OpenRoot(absRead)
	if err != nil {
		return nil, fmt.Errorf("write door: opening read root: %w", err)
	}
	defer root.Close()
	if rel != "." {
		// os.Root.MkdirAll refuses to traverse out of the root, including
		// through a symlink or a Windows junction, and fails closed.
		if err := root.MkdirAll(rel, 0o755); err != nil {
			return nil, fmt.Errorf("write door: creating write_root %q: %w", contract.WriteRoot, err)
		}
	}

	if rel != "." {
		if verr := verifyWriteRootInside(root, rel); verr != nil {
			return nil, fmt.Errorf("write door: write_root %q %w", contract.WriteRoot, verr)
		}
	}
	abs := filepath.Join(absRead, rel)
	snap, err := writedoor.Take(abs)
	if err != nil {
		return nil, fmt.Errorf("write door: snapshotting write_root %q: %w", contract.WriteRoot, err)
	}
	return &writeDoor{
		root:   abs,
		rel:    filepath.ToSlash(rel),
		before: snap,
		limit:  agent.NewWriteLimit(core.AgentWriteMaxFiles, core.AgentWriteMaxBytes),
	}, nil
}

// verifyWriteRootInside confirms that rel, opened THROUGH the read root's
// os.Root handle, is a real directory inside it. os.Root fails closed on "..",
// on absolute paths, and on reparse points — symlinks AND Windows junctions —
// so a write root reached through one is refused HERE, before the absolute path
// is handed to the write tools, which would otherwise open their own os.Root on
// a tree OUTSIDE the read root and confine the run to the wrong place.
//
// Deliberately os.Root and NOT filepath.EvalSymlinks + filepath.Rel. That
// resolved-path comparison is the obvious implementation and it is wrong on
// half the fleet: Go reports a Windows junction as an ordinary directory, so
// EvalSymlinks does not follow it and the comparison passes an escape it was
// written to catch. One mechanism that behaves the same on both operating
// systems beats two that disagree — the same rule the repo's cross-platform
// lint enforces elsewhere.
func verifyWriteRootInside(root *os.Root, rel string) error {
	f, err := root.Open(rel)
	if err != nil {
		return fmt.Errorf("does not resolve inside the read root (%w) — a symlink or junction points out of the tree", err)
	}
	defer f.Close()
	fi, serr := f.Stat()
	if serr != nil {
		return fmt.Errorf("cannot be inspected: %w", serr)
	}
	if !fi.IsDir() {
		return fmt.Errorf("is not a directory")
	}
	return nil
}

// closeWriteDoor renders the finished write set. It returns the unified diff,
// the touched paths, a human note, and — when a cap was broken — an error.
//
// A cap breach publishes NO diff. Truncating it would hand the caller a patch
// that applies as silent damage, and publishing it whole would make the cap a
// suggestion. The counts go in the note so the caller knows exactly what the
// seat did and by how much it overran.
func closeWriteDoor(d *writeDoor) (diff string, files []string, note string, err error) {
	if d == nil {
		return "", nil, "", nil
	}
	after, terr := writedoor.Take(d.root)
	if terr != nil {
		return "", nil, "", fmt.Errorf("write door: snapshotting write_root %q after the run: %w", d.rel, terr)
	}
	changes := writedoor.Changes(d.before, after)
	if len(changes) == 0 {
		return "", nil, fmt.Sprintf("write door open on %s: the seat wrote nothing", d.rel), nil
	}
	if len(changes) > core.AgentWriteMaxFiles {
		return "", nil, "", fmt.Errorf("write door: the run changed %d files under %s, over the %d-file cap — no diff is published (a partial patch applies as silent damage)",
			len(changes), d.rel, core.AgentWriteMaxFiles)
	}
	diff, files = writedoor.Unified(changes)
	if len(diff) > core.AgentWriteDiffMaxBytes {
		return "", nil, "", fmt.Errorf("write door: the diff of %d changed file(s) under %s is %d bytes, over the %d-byte cap — no diff is published (a truncated patch applies as silent damage)",
			len(changes), d.rel, len(diff), core.AgentWriteDiffMaxBytes)
	}
	return diff, files, fmt.Sprintf("write door open on %s: %d file(s) changed, %d bytes of diff — NOT applied; review and apply it yourself",
		d.rel, len(files), len(diff)), nil
}
