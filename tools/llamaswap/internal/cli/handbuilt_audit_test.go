// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Wave D: the hand-built command audit, made permanent.
//
// Four waves added commands by hand. The conventions they must all honor are
// easy to get right once and easy to lose on the next edit, so they are
// asserted here rather than checked by eye:
//
//   - mcp:read-only is present on every non-mutating command, so an agent can
//     tell a safe tool from one that changes GPU state;
//   - Example is non-empty, because an agent reads the example before it reads
//     the flags;
//   - no cobra.MinimumNArgs / MarkFlagRequired, because both fire BEFORE RunE
//     and would defeat the --dry-run and PRINTING_PRESS_VERIFY short-circuits
//     every side-effecting command here depends on.

package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pflagFlag aliases the flag type so the VisitAll callback below reads clearly.
type pflagFlag = pflag.Flag

// handBuiltReadOnly is every hand-built command path, mapped to whether it is
// read-only. A command that changes GPU residency, spawns a process, cancels a
// request, or writes a file is NOT read-only, whatever else it does.
//
// New hand-built command? Add it here. A missing entry fails the test on
// purpose: an unclassified command is one an agent cannot reason about.
var handBuiltReadOnly = map[string]bool{
	// wave A — spine
	"ps":                true,
	"swaps":             true,
	"keepset":           true,
	"keepset status":    true,
	"keepset audit":     true,
	"models unload":     false,
	"models unload-all": false,
	"sync":              false, // writes the local mirror
	// wave B — config surface
	"config":              true,
	"config validate":     true,
	"config lint":         true,
	"config explain":      true,
	"config diff":         true,
	"config drift":        true,
	"config backup":       false, // creates backup files
	"config testinstance": false, // spawns a throwaway server
	"config apply":        true,  // dry-run only; prints, never writes
	"seat":                true,
	"seat log":            true,
	"seat show":           true,
	"seat try":            true, // plan-only
	"bind":                true,
	"bind check":          true,
	// wave C — measurement
	"gguf":         true,
	"vram":         true,
	"fit":          true,
	"ctx":          true,
	"build":        true,
	"build check":  true,
	"verify":       true,
	"bench":        false, // loads models
	"bench aux":    false,
	"gate":         false,
	"gate grammar": false,
	"gate tools":   false,
	"scratch":      false, // spawns a server
	// wave D — final glue
	"load":            false, // changes GPU residency, can evict
	"kill":            false, // cancels live requests
	"events":          true,
	"logs triage":     true,
	"captures export": true, // reads the API; the only write is the caller's own --out
	"upstream open":   true, // prints by default; --launch is opt-in and verify-gated
	"configure":       true,
	"service":         true,
	"service status":  true,
}

// walkCommands returns every command in the tree keyed by its path with the
// root name stripped.
func walkCommands(t *testing.T) map[string]*cobra.Command {
	t.Helper()
	root := RootCmd()
	out := map[string]*cobra.Command{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			path := strings.TrimSpace(strings.TrimPrefix(child.CommandPath(), root.Name()))
			out[path] = child
			walk(child)
		}
	}
	walk(root)
	return out
}

func TestHandBuiltCommandsAreRegistered(t *testing.T) {
	tree := walkCommands(t)
	var missing []string
	for path := range handBuiltReadOnly {
		if _, ok := tree[path]; !ok {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("hand-built commands absent from the tree (registration regression): %s", strings.Join(missing, ", "))
	}
}

func TestHandBuiltCommandsCarryCorrectMCPAnnotation(t *testing.T) {
	tree := walkCommands(t)
	for path, readOnly := range handBuiltReadOnly {
		cmd, ok := tree[path]
		if !ok {
			continue // reported by TestHandBuiltCommandsAreRegistered
		}
		got := cmd.Annotations["mcp:read-only"] == "true"
		if readOnly && !got {
			t.Errorf("%q is read-only but carries no mcp:read-only=true annotation; an agent cannot tell it is safe", path)
		}
		if !readOnly && got {
			t.Errorf("%q changes state but is annotated mcp:read-only=true; that is a lie an agent will act on", path)
		}
	}
}

func TestHandBuiltCommandsHaveRealExamples(t *testing.T) {
	tree := walkCommands(t)
	for path := range handBuiltReadOnly {
		cmd, ok := tree[path]
		if !ok {
			continue
		}
		example := strings.TrimSpace(cmd.Example)
		if example == "" {
			t.Errorf("%q has no Example; an agent reads the example before the flags", path)
			continue
		}
		// An example that does not name the binary is not runnable, and a
		// placeholder example is worse than none.
		if !strings.Contains(example, "llamaswap-pp-cli") {
			t.Errorf("%q example does not invoke the binary: %q", path, example)
		}
		for _, placeholder := range []string{"TODO", "<your", "example.com", "foo bar"} {
			if strings.Contains(example, placeholder) {
				t.Errorf("%q example contains the placeholder %q: %s", path, placeholder, example)
			}
		}
	}
}

func TestHandBuiltCommandsDoNotPreemptRunE(t *testing.T) {
	// cobra.MinimumNArgs and MarkFlagRequired both reject the invocation
	// BEFORE RunE runs. Every side-effecting command here short-circuits on
	// --dry-run and PRINTING_PRESS_VERIFY inside RunE, so a pre-RunE validator
	// would make a verify pass fail instead of printing its plan. Validation
	// therefore lives in RunE, and this asserts nobody reintroduced the
	// shortcut.
	tree := walkCommands(t)
	for path := range handBuiltReadOnly {
		cmd, ok := tree[path]
		if !ok {
			continue
		}
		cmd.Flags().VisitAll(func(f *pflagFlag) {
			if f.Annotations == nil {
				return
			}
			if _, required := f.Annotations[cobra.BashCompOneRequiredFlag]; required {
				t.Errorf("%q marks --%s required; that fires before RunE and breaks the verify/dry-run short-circuit", path, f.Name)
			}
		})
		if cmd.Args == nil {
			continue
		}
		// A zero-arg invocation must reach RunE. Commands that need a
		// positional report it themselves, with a typed exit code.
		if err := cmd.Args(cmd, nil); err != nil {
			t.Errorf("%q rejects zero args before RunE (%v); move the check into RunE so --dry-run still works", path, err)
		}
	}
}
