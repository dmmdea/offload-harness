// Registration for the ComfyUI domain commands (Phase 3 P1+P2).
//
// NOT generated — markerless on purpose, so `printing-press generate --force` preserves it.
// Do not add the generated-file marker.
//
// WHY THIS FILE EXISTS RATHER THAN AN EDIT TO root.go. root.go carries the DO-NOT-EDIT marker
// and is rewritten on every regeneration. It exposes registerNovelCommand precisely so
// hand-authored commands can attach without touching it: the generated newRootCmd walks
// novelCommandHooks after building the endpoint-mirror tree. Registering here means the source
// AND the wiring both survive a regeneration, with no reliance on the regen-merge re-injecting
// a lost AddCommand call.
//
// Only PARENTS are registered. nodes / models / jobs / exp attach their own leaves.
//
// addNovelCommandIfAbsent (generated) is used rather than AddCommand so that if a future spec
// revision promotes an endpoint command with one of these names, the generated one wins and
// this registration silently yields instead of producing a duplicate command tree.
//
// Each constructor is INVOKED here rather than passed as a bare function value. Both shapes
// build the same tree at the same moment (the slice literal is evaluated inside the hook), but
// static wiring analysis — printing-press `dogfood`'s command-tree check among them — looks for
// a call site, and a bare `newFooCmd,` reference is not one. Keeping the call explicit means
// the registration is visible to tooling as well as to the compiler.
package cli

import "github.com/spf13/cobra"

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		for _, cmd := range []*cobra.Command{
			// Answers the server cannot give
			newNodesCmd(flags),      // COMBO option VALUES, both spec shapes
			newModelsCmd(flags),     // four-way model visibility triage
			newProvenanceCmd(flags), // output file -> the run that made it
			newOutputsCmd(flags),    // recorded outputs, ffprobe-enriched

			// Submit and lifecycle
			newSubmitCmd(flags),      // idempotent attach lease
			newAttachCmd(flags),      // attach without submitting
			newJobsCmd(flags),        // queue + history + durable local record
			newStatusCmd(flags),      // one job, honest duration
			newWaitCmd(flags),        // no artificial local timeout
			newSyncHistoryCmd(flags), // capture RAM history before a restart drops it
			newTimingCmd(flags),      // durations vs the same shape's distribution

			// Reproducible edits
			newSlotsCmd(flags),    // stable <node_id>.<input> addresses
			newSetCmd(flags),      // guarded patching, dry-run by default
			newValidateCmd(flags), // offline preflight (no validate-only endpoint exists)
			newStageCmd(flags),    // content-addressed input identity

			// Experiments
			newComfyExpCmd(flags),    // multi-arm sweeps that survive a restart
			newComfyReplayCmd(flags), // replay with an attributed delta
		} {
			addNovelCommandIfAbsent(root, cmd)
		}
	})
}
