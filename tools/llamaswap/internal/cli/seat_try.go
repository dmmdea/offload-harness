// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source computed

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

type seatTryPlan struct {
	SchemaVersion string               `json:"schema_version"`
	Mode          string               `json:"mode"`
	Model         string               `json:"model"`
	SeatKind      lsconfig.SeatKind    `json:"seat_kind"`
	ConfigPath    string               `json:"config_path"`
	ConfigSha     string               `json:"config_sha256"`
	Sets          []string             `json:"sets"`
	Unsets        []string             `json:"unsets,omitempty"`
	CurrentCmd    string               `json:"current_cmd"`
	ProposedCmd   string               `json:"proposed_cmd"`
	Deltas        []lsconfig.FlagDelta `json:"flag_deltas"`
	UnifiedDiff   string               `json:"unified_diff"`
	// ProposedBlock is the seat's YAML block with the cmd line rewritten. It
	// is TEXT FOR A HUMAN TO PASTE, computed in memory. It is never written.
	ProposedBlock   string   `json:"proposed_yaml_block"`
	BackupCommand   string   `json:"backup_command"`
	RestartCommand  []string `json:"restart_command"`
	RestartSource   string   `json:"restart_command_source"`
	AcceptanceProbe []string `json:"acceptance_probe"`
	Warnings        []string `json:"warnings,omitempty"`
}

const seatTrySchemaVersion = "seat-try/1"

func newSeatTryCmd(flags *rootFlags) *cobra.Command {
	var (
		configPath string
		sets       []string
		unsets     []string
		verifyCmd  string
	)

	cmd := &cobra.Command{
		Use:   "try <model> --set \"--flag value\"",
		Short: "PLAN a seat flag change: the would-be command, a unified diff, the restart command, and an acceptance probe. Never writes.",
		Long: "Plan a flag experiment on one seat.\n\n" +
			"Computes the command line the seat WOULD run with the change applied, shows\n" +
			"the per-flag delta and a unified diff of the YAML block, and prints the\n" +
			"backup command, the restart command, and a suggested acceptance probe.\n\n" +
			"It does not touch the config. Not with a backup first. The proposed YAML\n" +
			"block is text for a human to paste, because the surrounding comments are the\n" +
			"record of why every other flag on that line is there, and a program that\n" +
			"rewrites the line is a program that can drop them.\n\n" +
			"--set takes a whole flag with its value: --set \"--ctx-size 65536\". Repeat it\n" +
			"for several. --unset removes a flag by name. A flag already present is\n" +
			"replaced in place, so the rest of the command line keeps its order.\n\n" +
			"The acceptance probe matters more than the diff: the rerun IS the test. A\n" +
			"flag change with no way to tell whether it helped is a change you cannot\n" +
			"keep or revert on evidence.",
		Example: "  llamaswap-pp-cli seat try bge-reranker-v2-m3 --set \"--n-gpu-layers 99\"\n" +
			"  llamaswap-pp-cli seat try gemma-4-e4b --set \"-c 65536\" --json\n" +
			"  llamaswap-pp-cli seat try gemma-4-e4b --unset --reasoning --verify \"llamaswap-pp-cli bench gemma-4-e4b --runs 3\"",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d", ExitModelNotFound),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				if dryRunOK(flags) {
					return writeDryRun(out, flags, "seat try <model> --set \"--flag value\"")
				}
				return cmd.Help()
			}
			path, err := resolveConfigPath([]string{configPath}, 0)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, "seat try "+args[0]+" (plan only; never writes)")
			}
			if len(sets) == 0 && len(unsets) == 0 {
				return usageErr(fmt.Errorf("nothing to try: pass at least one --set \"--flag value\" or --unset --flag"))
			}
			f, err := loadConfigFile(path)
			if err != nil {
				return err
			}
			m, ok := f.Resolve(args[0])
			if !ok {
				return errModelNotFound(fmt.Errorf("no model or alias %q in %s", args[0], path))
			}
			plan, err := buildSeatTryPlan(f, m, sets, unsets, verifyCmd)
			if err != nil {
				return err
			}
			if wantsJSON(out, flags) {
				return printJSONFiltered(out, plan, flags)
			}
			printSeatTryHuman(cmd, plan)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config-file", "", "llama-swap YAML to plan against (default: the live config)")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "flag to set, whole: --set \"--ctx-size 65536\" (repeatable)")
	cmd.Flags().StringArrayVar(&unsets, "unset", nil, "flag to remove by name: --unset --reasoning (repeatable)")
	cmd.Flags().StringVar(&verifyCmd, "verify", "", "acceptance command to run after the restart; printed into the plan")
	return cmd
}

func buildSeatTryPlan(f *lsconfig.File, m *lsconfig.Model, sets, unsets []string, verifyCmd string) (*seatTryPlan, error) {
	plan := &seatTryPlan{
		SchemaVersion: seatTrySchemaVersion,
		Mode:          "plan only (this command never writes the config)",
		Model:         m.ID, SeatKind: m.Seat,
		ConfigPath: f.Path, ConfigSha: f.Sha256,
		Sets: sets, Unsets: unsets,
		CurrentCmd: m.CmdExpanded,
	}
	proposedRaw, err := applyFlagEdits(m.CmdRaw, sets, unsets)
	if err != nil {
		return nil, usageErr(err)
	}
	exp := lsconfig.NewExpander(f.Macros, nil)
	proposedExpanded, _, _ := exp.Expand(proposedRaw)
	plan.ProposedCmd = proposedExpanded
	plan.Deltas = lsconfig.DiffCmds(m.CmdExpanded, proposedExpanded)
	if len(plan.Deltas) == 0 {
		plan.Warnings = append(plan.Warnings, "the proposed command is identical to the current one — the flag is already set to that value")
	}

	before := strings.Split(m.RawBlock(), "\n")
	after := make([]string, len(before))
	copy(after, before)
	replaced := false
	for i, line := range after {
		if strings.Contains(line, m.CmdRaw) {
			after[i] = strings.Replace(line, m.CmdRaw, proposedRaw, 1)
			replaced = true
			break
		}
	}
	if !replaced {
		plan.Warnings = append(plan.Warnings,
			"could not locate the cmd string verbatim in the seat's source block (it is probably a multi-line block scalar); the proposed YAML below shows the command only, not the whole stanza")
		after = append(after, "", "# proposed cmd:", "    cmd: \""+proposedRaw+"\"")
	}
	plan.ProposedBlock = strings.Join(after, "\n")
	plan.UnifiedDiff = lsconfig.UnifiedDiff(m.ID+" (current)", m.ID+" (proposed)", before, after, 3)

	if m.Seat != lsconfig.SeatLlamaServer {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"this seat runs %s, not llama-server — llama-server flag semantics do not apply and no flag validation was performed on the change",
			lsconfig.BinaryBase(m.Binary)))
	}
	if m.TTL != nil && *m.TTL < 0 {
		plan.Warnings = append(plan.Warnings,
			"this is a keep-resident seat (ttl < 0). Restarting the service to apply the change drops it, and anything depending on it degrades silently until it reloads — schedule accordingly")
	}

	plan.BackupCommand = fmt.Sprintf("llamaswap-pp-cli config backup --label pre-%s-try", m.ID)
	plan.RestartCommand, plan.RestartSource = restartCommand(f.Path)
	plan.AcceptanceProbe = acceptanceProbe(m, verifyCmd)
	return plan, nil
}

// applyFlagEdits rewrites a command string with flags set or removed, keeping
// the surrounding token order intact. Replacement is in place so a diff of the
// result shows only the flag that moved.
func applyFlagEdits(cmd string, sets, unsets []string) (string, error) {
	tokens := lsconfig.TokenizeCmd(cmd)
	for _, raw := range unsets {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if !lsconfig.IsFlagToken(name) {
			return "", fmt.Errorf("--unset %q does not look like a flag (expected a leading dash)", raw)
		}
		tokens = removeFlagTokens(tokens, name)
	}
	for _, raw := range sets {
		parts := lsconfig.TokenizeCmd(strings.TrimSpace(raw))
		if len(parts) == 0 {
			continue
		}
		if !lsconfig.IsFlagToken(parts[0]) {
			return "", fmt.Errorf("--set %q does not start with a flag (expected e.g. --set \"--ctx-size 65536\")", raw)
		}
		tokens = setFlagTokens(tokens, parts[0], parts[1:])
	}
	return strings.Join(tokens, " "), nil
}

func removeFlagTokens(tokens []string, name string) []string {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		if tokens[i] != name {
			out = append(out, tokens[i])
			continue
		}
		for i+1 < len(tokens) && !lsconfig.IsFlagToken(tokens[i+1]) {
			i++
		}
	}
	return out
}

func setFlagTokens(tokens []string, name string, values []string) []string {
	out := make([]string, 0, len(tokens)+len(values)+1)
	found := false
	for i := 0; i < len(tokens); i++ {
		if tokens[i] != name {
			out = append(out, tokens[i])
			continue
		}
		found = true
		out = append(out, name)
		out = append(out, values...)
		for i+1 < len(tokens) && !lsconfig.IsFlagToken(tokens[i+1]) {
			i++
		}
	}
	if !found {
		out = append(out, name)
		out = append(out, values...)
	}
	return out
}

// acceptanceProbe suggests a falsifiable check for the seat's role. A flag
// change without one cannot be kept or reverted on evidence.
func acceptanceProbe(m *lsconfig.Model, verifyCmd string) []string {
	if strings.TrimSpace(verifyCmd) != "" {
		return []string{verifyCmd, "# rerun this BEFORE the change too — a post-only number is not a comparison"}
	}
	spec := lsconfig.ParseCmd(m.CmdExpanded)
	switch {
	case hasFlag(spec, "--reranking"):
		return []string{
			fmt.Sprintf("llamaswap-pp-cli rerank %s --query \"<fixed query>\" --docs \"<doc a>\" \"<doc b>\"", m.ID),
			"# the raw logits are what downstream thresholds are calibrated to: compare SCORES, not just ordering",
		}
	case hasFlag(spec, "--embeddings"):
		return []string{
			fmt.Sprintf("llamaswap-pp-cli embed %s --input \"<fixed calibration string>\"", m.ID),
			"# compare the vector against the stored baseline; a dropped --pooling flag changes the embedding",
			"# without changing the roster, which is invisible to a model-count check",
		}
	case m.Seat != lsconfig.SeatLlamaServer:
		return []string{
			fmt.Sprintf("llamaswap-pp-cli transcribe %s <a fixed audio clip>", m.ID),
			"# compare the transcript against the same clip's previous output word for word",
		}
	default:
		return []string{
			fmt.Sprintf("llamaswap-pp-cli bench %s --runs 3", m.ID),
			fmt.Sprintf("llamaswap-pp-cli ctx %s", m.ID),
			"# bench from a clean state (nothing else loaded) or the number measures contention, not the flag",
		}
	}
}

func hasFlag(spec lsconfig.CmdSpec, name string) bool {
	_, ok := spec.Get(name)
	return ok
}

func printSeatTryHuman(cmd *cobra.Command, p *seatTryPlan) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s %s  %s\n", bold("seat try"), bold(p.Model), yellow("[PLAN ONLY — nothing is written]"))
	fmt.Fprintf(out, "  config %s (sha %s)\n", p.ConfigPath, p.ConfigSha[:16])
	fmt.Fprintf(out, "  seat   %s\n\n", p.SeatKind)

	fmt.Fprintf(out, "%s\n", bold("FLAG DELTA"))
	if len(p.Deltas) == 0 {
		fmt.Fprintf(out, "  (none — the proposed command equals the current one)\n")
	}
	for _, d := range p.Deltas {
		fmt.Fprintf(out, "  %s\n", d.String())
	}

	fmt.Fprintf(out, "\n%s\n  %s\n", bold("PROPOSED COMMAND"), p.ProposedCmd)

	if strings.TrimSpace(p.UnifiedDiff) != "" {
		fmt.Fprintf(out, "\n%s\n%s", bold("YAML BLOCK DIFF (for you to apply by hand)"), p.UnifiedDiff)
	}

	fmt.Fprintf(out, "\n%s\n  %s\n", bold("1. BACK UP FIRST"), p.BackupCommand)
	fmt.Fprintf(out, "\n%s\n  edit %s by hand, then:\n  llamaswap-pp-cli config testinstance %s\n", bold("2. EDIT AND VALIDATE"), p.ConfigPath, p.ConfigPath)
	fmt.Fprintf(out, "\n%s (source: %s)\n", bold("3. RESTART — PRINTED, NOT RUN"), p.RestartSource)
	for _, l := range p.RestartCommand {
		fmt.Fprintf(out, "  %s\n", l)
	}
	fmt.Fprintf(out, "\n%s\n", bold("4. ACCEPTANCE PROBE"))
	for _, l := range p.AcceptanceProbe {
		fmt.Fprintf(out, "  %s\n", l)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(out, "\n%s %s\n", yellow("warning:"), w)
	}
}
