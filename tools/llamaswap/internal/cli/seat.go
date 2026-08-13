// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command family: per-seat history and experiments.
// pp:data-source auto
// Supported strategies: auto, local, live, or computed. `seat log` is local
// (the backup corpus on disk), `seat show` is live+local (a live cmd vs the
// file), `seat try` is computed (it plans a change and never applies it).

package cli

import (
	"github.com/spf13/cobra"
)

func newNovelSeatCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "seat",
		Short: "Per-seat history and experiments: log, show, try.",
		Long: "Everything about ONE model seat that the llama-swap API cannot tell you.\n\n" +
			"  log   the seat's change history, mined from the config-backup series on disk\n" +
			"  show  the seat's live command line vs what the file says today\n" +
			"  try   plan a flag change: the would-be command, the diff, the restart, the probe\n\n" +
			"None of these writes the config.",
		Example: "  llamaswap-pp-cli seat log gemma-4-31b\n" +
			"  llamaswap-pp-cli seat show embeddinggemma --json\n" +
			"  llamaswap-pp-cli seat try gemma-4-e2b --set \"--ctx-size 65536\"",
		Annotations: map[string]string{"mcp:read-only": "true", "pp:parent-group": "true"},
		RunE:        parentNoSubcommandRunE(flags),
	}
	addNovelCommandIfAbsent(cmd, newNovelSeatLogCmd(flags))
	addNovelCommandIfAbsent(cmd, newSeatShowCmd(flags))
	addNovelCommandIfAbsent(cmd, newSeatTryCmd(flags))
	return cmd
}
