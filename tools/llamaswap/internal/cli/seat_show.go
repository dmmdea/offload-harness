// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source live

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

type seatShowResult struct {
	SchemaVersion string               `json:"schema_version"`
	Model         string               `json:"model"`
	MatchedBy     string               `json:"matched_by,omitempty"`
	ConfigPath    string               `json:"config_path"`
	ConfigSha     string               `json:"config_sha256"`
	SeatKind      lsconfig.SeatKind    `json:"seat_kind"`
	Running       bool                 `json:"running"`
	State         string               `json:"state,omitempty"`
	Proxy         string               `json:"proxy,omitempty"`
	LiveCmd       string               `json:"live_cmd,omitempty"`
	FileCmd       string               `json:"file_cmd"`
	Deltas        []lsconfig.FlagDelta `json:"flag_deltas,omitempty"`
	Drift         bool                 `json:"drift"`
	YamlBlock     string               `json:"yaml_block,omitempty"`
	Note          string               `json:"note,omitempty"`
}

const seatShowSchemaVersion = "seat-show/1"

func newSeatShowCmd(flags *rootFlags) *cobra.Command {
	var (
		configPath string
		diffYaml   bool
	)

	cmd := &cobra.Command{
		Use:   "show <model>",
		Short: "One seat's live command line vs the file, flag by flag.",
		Long: "Compare a single seat's RUNNING command line against what the config says.\n\n" +
			"`config drift` sweeps every loaded seat; this answers the same question for\n" +
			"one, which is what you want mid-investigation. The live cmd comes from\n" +
			"GET /running — the process's own argv, after macro expansion and port\n" +
			"assignment — so it is ground truth, not a re-derivation.\n\n" +
			"Exits " + fmt.Sprint(ExitDrift) + " when the seat's live flags diverge from the file. That is a\n" +
			"finding, not an error.\n\n" +
			"A seat that is not currently loaded has no live cmd. It is reported as such,\n" +
			"never inferred: loading it to check would evict whatever is resident.",
		Example: "  llamaswap-pp-cli seat show bge-reranker-v2-m3\n" +
			"  llamaswap-pp-cli seat show gemma-4-e4b --diff-yaml\n" +
			"  llamaswap-pp-cli seat show embeddinggemma --json",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d,%d,%d", ExitModelNotFound, ExitServerUnreachable, ExitDrift),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				if dryRunOK(flags) {
					return writeDryRun(out, flags, "seat show <model>")
				}
				return cmd.Help()
			}
			path, err := resolveConfigPath([]string{configPath}, 0)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, "seat show "+args[0])
			}
			f, err := loadConfigFile(path)
			if err != nil {
				return err
			}
			m, ok := f.Resolve(args[0])
			if !ok {
				return errModelNotFound(fmt.Errorf("no model or alias %q in %s", args[0], path))
			}
			running, err := fetchRunning(cmd.Context(), flags)
			if err != nil {
				return err
			}
			res := &seatShowResult{
				SchemaVersion: seatShowSchemaVersion,
				Model:         m.ID, MatchedBy: matchedBy(m, args[0]),
				ConfigPath: f.Path, ConfigSha: f.Sha256,
				SeatKind: m.Seat, FileCmd: m.CmdExpanded,
			}
			if diffYaml {
				res.YamlBlock = m.RawBlock()
			}
			for _, r := range running {
				if r.Model != m.ID {
					continue
				}
				res.Running = true
				res.State = r.State
				res.Proxy = r.Proxy
				res.LiveCmd = r.Cmd
				res.Deltas = lsconfig.DiffCmds(m.CmdExpanded, r.Cmd)
				res.Drift = len(res.Deltas) > 0
			}
			if !res.Running {
				res.Note = "seat is not loaded — there is no live command line to compare. Drift is unknowable without starting it, and starting it would evict whatever is resident."
			}
			if wantsJSON(out, flags) {
				if err := printJSONFiltered(out, res, flags); err != nil {
					return err
				}
			} else {
				printSeatShowHuman(cmd, res)
			}
			if res.Drift {
				return errDrift(fmt.Errorf("seat %s diverges from %s in %d flag(s)", m.ID, path, len(res.Deltas)))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config-file", "", "llama-swap YAML to compare against (default: the live config)")
	cmd.Flags().BoolVar(&diffYaml, "diff-yaml", false, "also print the seat's raw YAML block, comments included")
	return cmd
}

func printSeatShowHuman(cmd *cobra.Command, r *seatShowResult) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s  (%s)\n", bold(r.Model), r.SeatKind)
	fmt.Fprintf(out, "  config %s (sha %s)\n", r.ConfigPath, r.ConfigSha[:16])
	if r.Running {
		fmt.Fprintf(out, "  state  %s via %s\n", r.State, r.Proxy)
	} else {
		fmt.Fprintf(out, "  state  %s\n", yellow("not loaded"))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  file: %s\n", r.FileCmd)
	if r.LiveCmd != "" {
		fmt.Fprintf(out, "  live: %s\n", r.LiveCmd)
	}
	fmt.Fprintln(out)
	switch {
	case !r.Running:
		fmt.Fprintf(out, "%s\n", r.Note)
	case r.Drift:
		fmt.Fprintf(out, "%s\n", red(fmt.Sprintf("DRIFT — %d flag(s) differ", len(r.Deltas))))
		for _, d := range r.Deltas {
			fmt.Fprintf(out, "  %s\n", d.String())
		}
	default:
		fmt.Fprintf(out, "%s\n", green("MATCH — the running process's flags are exactly what the file says"))
	}
	if strings.TrimSpace(r.YamlBlock) != "" {
		fprintBlock(out, "YAML BLOCK (verbatim)", r.YamlBlock)
	}
}
