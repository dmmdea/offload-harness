// intake.go is the DELEGATOR-side contract intake (roast delta 6): both
// surfaces (MCP agent_delegate, the CLI verb) accept per-subtask
// `context_paths` beside the wire-contract fields, and the DELEGATOR reads and
// inlines those files so the calling session's context never pays for them —
// while the wire contract stays inline-docs and self-contained (the remote
// node never reaches back into the delegator's filesystem).
//
// The context_paths extension lives HERE, on an intake type, and NOT on
// core.AgentContract: paths are a surface convenience, and putting them on the
// wire type would tempt a future reader into resolving them node-side — the
// exact capability the self-contained contract exists to deny.

package delegate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// contextPathMaxBytes caps ONE inlined file at 128 KiB. The contract's total
// context cap (core.AgentContextMaxBytes, 256 KiB) still applies post-inline
// via Validate — this per-file bound exists so a single oversized file gets a
// rejection NAMING the file instead of a total-cap error naming nothing.
const contextPathMaxBytes = 128 << 10

// SubtaskSpec is one surface-level subtask: the wire contract's own fields
// plus the delegator-side context_paths extension. The embedded contract
// decodes/encodes under its normal JSON tags, so a CLI contract file and the
// MCP subtask object share one vocabulary.
type SubtaskSpec struct {
	core.AgentContract
	ContextPaths []string `json:"context_paths,omitempty"`
}

// PrepareContract turns a SubtaskSpec into a dispatchable core.AgentContract:
// it MINTS schema_version and depth (the surface IS the origin — a caller
// claim of either is overwritten, never trusted), applies the same
// MaxSteps/TimeoutSec ceilings DecodeAgentContract applies on the wire (so a
// locally-run contract obeys the same rules as a dispatched one), inlines
// context_paths under readRoot confinement, then runs the full Validate —
// fail before the network, exactly like the wire decoder's posture.
func PrepareContract(spec SubtaskSpec, readRoot string) (core.AgentContract, error) {
	c := spec.AgentContract
	c.SchemaVersion = core.AgentWireSchemaVersion
	c.Depth = 0
	// Same clamp rule as DecodeAgentContract: an over-ask is an intent
	// mismatch, not a malformed contract — the answer is "you get the cap".
	if c.MaxSteps <= 0 {
		c.MaxSteps = core.AgentMaxStepsDefault
	} else if c.MaxSteps > core.AgentMaxStepsCap {
		c.MaxSteps = core.AgentMaxStepsCap
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = core.AgentTimeoutSecDefault
	} else if c.TimeoutSec > core.AgentTimeoutSecCap {
		c.TimeoutSec = core.AgentTimeoutSecCap
	}

	if len(spec.ContextPaths) > 0 {
		docs, err := inlineContextPaths(spec.ContextPaths, readRoot)
		if err != nil {
			return core.AgentContract{}, err
		}
		c.Context = append(c.Context, docs...)
	}

	if err := c.Validate(); err != nil {
		return core.AgentContract{}, err
	}
	return c, nil
}

// inlineContextPaths reads each path into a ContextDoc named by its base name.
// Containment follows the house idiom (internal/agent ReadOnlyTools):
// OS-ENFORCED via os.Root — `..`, absolute escapes, and reparse points
// (symlinks / Windows junctions) are rejected by the kernel-level traversal,
// failing CLOSED. An absolute path is first re-rooted via filepath.Rel so a
// caller may name files either way; anything that lands outside readRoot is a
// hard error, never a silent skip.
func inlineContextPaths(paths []string, readRoot string) ([]core.ContextDoc, error) {
	if strings.TrimSpace(readRoot) == "" {
		// Mirror handleAgentRun's read_root default: the process working dir.
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("context_paths: cannot determine working dir for read_root: %w", err)
		}
		readRoot = wd
	}
	absRoot, err := filepath.Abs(readRoot)
	if err != nil {
		return nil, fmt.Errorf("context_paths: bad read_root: %w", err)
	}
	root, err := os.OpenRoot(absRoot)
	if err != nil {
		return nil, fmt.Errorf("context_paths: opening read_root %s: %w", absRoot, err)
	}
	defer root.Close()

	docs := make([]core.ContextDoc, 0, len(paths))
	for _, p := range paths {
		rel := p
		if filepath.IsAbs(p) {
			r, rerr := filepath.Rel(absRoot, p)
			if rerr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("context_paths: %q is outside read_root %s", p, absRoot)
			}
			rel = r
		}
		text, err := readBoundedInRoot(root, rel)
		if err != nil {
			return nil, fmt.Errorf("context_paths: %q: %w", p, err)
		}
		docs = append(docs, core.ContextDoc{Name: filepath.Base(rel), Text: text})
	}
	return docs, nil
}

// readBoundedInRoot reads one file through the os.Root handle, refusing
// anything over contextPathMaxBytes. The cap is checked by reading cap+1
// bytes rather than trusting Stat's size — a size that grows between stat and
// read (or a file Stat can't size) must still be bounded.
func readBoundedInRoot(root *os.Root, rel string) (string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return "", err // includes the os.Root escape rejection, verbatim
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, contextPathMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > contextPathMaxBytes {
		return "", errors.New(humanSizeCapError())
	}
	return string(b), nil
}

// humanSizeCapError names the per-file cap in the unit the operator thinks in.
func humanSizeCapError() string {
	return fmt.Sprintf("file exceeds the %d KiB per-file context cap", contextPathMaxBytes>>10)
}
