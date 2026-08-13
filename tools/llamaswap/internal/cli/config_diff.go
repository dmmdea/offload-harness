// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source local

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

func newConfigDiffCmd(flags *rootFlags) *cobra.Command {
	var (
		showComments bool
		unified      bool
		contextLines int
	)

	cmd := &cobra.Command{
		Use:   "diff <a> [b]",
		Short: "Semantic diff between two configs: per-model added/removed/changed flags, with the comment blocks that changed alongside.",
		Long: "Diff two llama-swap configs by MEANING, not by text.\n\n" +
			"A textual diff of a config that is 60% comments buries the one flag that\n" +
			"moved under thirty lines of reflowed prose. This compares expanded command\n" +
			"lines flag by flag, plus ttl/aliases/env/name, plus the top-level knobs and\n" +
			"macros — and separately NOTES when a model's comment block changed, because\n" +
			"that block is where the operator wrote down why.\n\n" +
			"Port values are normalized on both sides: the runtime-assigned port is the\n" +
			"one field llama-swap is supposed to rewrite, so it never reads as a change.\n\n" +
			"[b] defaults to the live config, so `config diff backup-x.yaml` answers\n" +
			"\"what changed since that backup?\".",
		Example: "  llamaswap-pp-cli config diff backup-2026-08-11-pre-b10356.yaml\n" +
			"  llamaswap-pp-cli config diff old.yaml new.yaml --json\n" +
			"  llamaswap-pp-cli config diff old.yaml --unified",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d", ExitConfigInvalid),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				if dryRunOK(flags) {
					return writeDryRun(out, flags, "config diff <a> [b]")
				}
				return cmd.Help()
			}
			aPath := args[0]
			bPath, err := resolveConfigPath(args, 1)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, fmt.Sprintf("config diff %s %s", aPath, bPath))
			}
			a, err := loadConfigFile(aPath)
			if err != nil {
				return err
			}
			b, err := loadConfigFile(bPath)
			if err != nil {
				return err
			}
			d := lsconfig.DiffConfigs(a, b)
			if wantsJSON(out, flags) {
				return printJSONFiltered(out, d, flags)
			}
			printConfigDiffHuman(cmd, d, a, b, showComments, unified, contextLines)
			return nil
		},
	}
	cmd.Flags().BoolVar(&showComments, "comments", false, "print the before/after comment blocks for models whose comments changed")
	cmd.Flags().BoolVar(&unified, "unified", false, "also print a standard unified text diff of the two files")
	cmd.Flags().IntVar(&contextLines, "context", 3, "context lines for --unified")
	return cmd
}

func printConfigDiffHuman(cmd *cobra.Command, d *lsconfig.ConfigDiff, a, b *lsconfig.File, showComments, unified bool, context int) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n  a %s  %s\n  b %s  %s\n", bold("config diff"), d.ASha[:16], d.APath, d.BSha[:16], d.BPath)
	if d.Identical {
		fmt.Fprintf(out, "\n%s\n", green("byte-identical (same sha256) — nothing to compare"))
		return
	}

	if len(d.TopLevel) > 0 {
		fmt.Fprintf(out, "\n%s\n", bold("TOP LEVEL"))
		w := newTabWriter(out)
		fmt.Fprintln(w, "  FIELD\tA\tB")
		for _, fd := range d.TopLevel {
			fmt.Fprintf(w, "  %s\t%s\t%s\n", fd.Field, dashIfEmpty(fd.From), dashIfEmpty(fd.To))
		}
		_ = w.Flush()
	}

	changed := 0
	for _, md := range d.Models {
		if !md.Changed() && !md.CommentChange {
			continue
		}
		changed++
		label := md.Status
		switch md.Status {
		case "added":
			label = green("+ added")
		case "removed":
			label = red("- removed")
		default:
			label = yellow("~ changed")
		}
		fmt.Fprintf(out, "\n%s  %s\n", label, bold(md.Model))
		for _, fl := range md.FlagDeltas {
			fmt.Fprintf(out, "    %s\n", fl.String())
		}
		for _, fd := range md.FieldDeltas {
			fmt.Fprintf(out, "    ~ %s: %s -> %s\n", fd.Field, dashIfEmpty(fd.From), dashIfEmpty(fd.To))
		}
		if md.CommentChange {
			if !showComments {
				fmt.Fprintf(out, "    %s (pass --comments to see them)\n", yellow("comment block changed"))
			} else {
				fprintBlock(out, "  comment block (a)", md.CommentFrom)
				fprintBlock(out, "  comment block (b)", md.CommentTo)
			}
		}
	}
	if changed == 0 && len(d.TopLevel) == 0 {
		fmt.Fprintf(out, "\n%s\n", green("no semantic differences (the files differ only in text: whitespace, comment reflow, or key order)"))
	} else {
		fmt.Fprintf(out, "\n%d model(s) differ\n", changed)
	}

	if unified {
		text := lsconfig.UnifiedDiff(d.APath, d.BPath, configFileLines(a), configFileLines(b), context)
		if strings.TrimSpace(text) != "" {
			fmt.Fprintf(out, "\n%s\n%s", bold("UNIFIED TEXT DIFF"), text)
		}
	}
}

func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
