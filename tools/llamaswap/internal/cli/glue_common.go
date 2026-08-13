// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored shared plumbing for the final-glue command family
// (load, kill, events, logs triage, captures export, upstream open,
// configure, service status). Not a command: no pp:data-source marker.

package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/mirror"
)

// init wires the glue family onto the command tree.
//
// Two shapes here. New top-level commands go on the root through the generated
// novel-command hook. Subcommands of GENERATED parents (logs, captures,
// upstream) are attached to the parent that root.go already added — the hooks
// run after those AddCommand calls, so the parents exist by now. Attaching this
// way is what keeps `logs`, `captures`, and `upstream` unmodified generated
// files: nothing is restructured, only extended.
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newGlueLoadCmd(flags))
		addNovelCommandIfAbsent(root, newGlueKillCmd(flags))
		addNovelCommandIfAbsent(root, newGlueEventsCmd(flags))
		addNovelCommandIfAbsent(root, newGlueConfigureCmd(flags))
		addNovelCommandIfAbsent(root, newGlueServiceCmd(flags))

		if logs := glueFindChild(root, "logs"); logs != nil {
			addNovelCommandIfAbsent(logs, newGlueLogsTriageCmd(flags))
		}
		if captures := glueFindChild(root, "captures"); captures != nil {
			addNovelCommandIfAbsent(captures, newGlueCapturesExportCmd(flags))
		}
		if upstream := glueFindChild(root, "upstream"); upstream != nil {
			addNovelCommandIfAbsent(upstream, newGlueUpstreamOpenCmd(flags))
		}
	})
}

// glueFindChild returns a direct child command by name or alias, or nil.
func glueFindChild(parent *cobra.Command, name string) *cobra.Command {
	if parent == nil {
		return nil
	}
	for _, c := range parent.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return c
		}
	}
	return nil
}

// glueSchemaVersion stamps every glue-command JSON envelope so a consumer can
// detect a shape change without diffing fields.
const glueSchemaVersion = 1

// glueClient builds the typed llama-swap client, reusing the spine's base-URL
// resolution (loopback normalization included) rather than re-deriving it.
func glueClient(flags *rootFlags) (*mirror.Client, error) {
	return spineClient(flags)
}

// glueClientWithTimeout is glueClient with a floor on the request deadline, for
// calls that can legitimately wait on a multi-GB model load.
func glueClientWithTimeout(flags *rootFlags, floor time.Duration) (*mirror.Client, error) {
	base, err := spineBaseURL(flags)
	if err != nil {
		return nil, err
	}
	timeout := floor
	if flags != nil && flags.timeout > floor {
		timeout = flags.timeout
	}
	return mirror.NewClient(base, timeout), nil
}

// glueResolve maps a model id or alias to its canonical roster id.
//
// Alias-aware by construction: on this deployment the memory stack is routinely
// addressed as text-embedding / local-embed / reranker-v2-m3 / v0.12-reranker,
// and an id-only lookup answers "not found" for every one of them.
func glueResolve(ctx context.Context, c *mirror.Client, name string) (mirror.RosterEntry, error) {
	roster, err := c.Models(ctx)
	if err != nil {
		return mirror.RosterEntry{}, spineExitErr(ExitServerUnreachable, fmt.Errorf("read roster from /v1/models: %w", err))
	}
	for _, e := range roster {
		if strings.EqualFold(e.ID, name) {
			return e, nil
		}
	}
	for _, e := range roster {
		for _, a := range e.Aliases {
			if strings.EqualFold(a, name) {
				return e, nil
			}
		}
	}
	ids := make([]string, 0, len(roster))
	for _, e := range roster {
		ids = append(ids, e.ID)
	}
	return mirror.RosterEntry{}, spineExitErr(ExitModelNotFound,
		fmt.Errorf("no model or alias %q in the roster (ids: %s)", name, strings.Join(ids, ", ")))
}

// glueIsRunning reports whether a canonical id currently holds VRAM.
func glueIsRunning(running []mirror.RunningEntry, id string) bool {
	for _, r := range running {
		if strings.EqualFold(r.Model, id) {
			return true
		}
	}
	return false
}

// Output rendering is NOT re-implemented here: the glue commands call the
// measurement family's mcEmit (measure_common.go), which already routes machine
// callers through printJSONFiltered and interactive humans through a prose
// renderer. One renderer for every hand-built command keeps --select, --compact,
// and --csv behaving identically across the tree.

// glueUsageErrf is a shorthand for a formatted usage error (exit 2). Argument
// validation lives in RunE rather than in cobra.Args or MarkFlagRequired so a
// --dry-run or verify pass can short-circuit before any validation fires.
func glueUsageErrf(format string, a ...any) error {
	return usageErr(fmt.Errorf(format, a...))
}
