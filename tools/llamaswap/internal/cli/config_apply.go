// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source local

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

// applyPlan is the machine-readable shape of `config apply`. Every field is a
// PLAN. Nothing in this command executes anything.
type applyPlan struct {
	SchemaVersion string `json:"schema_version"`
	Mode          string `json:"mode"`
	Candidate     string `json:"candidate"`
	CandidateSha  string `json:"candidate_sha256"`
	Live          string `json:"live"`
	LiveSha       string `json:"live_sha256"`
	Identical     bool   `json:"identical"`

	Validation  *lsconfig.ValidationResult `json:"validation"`
	Lint        *lsconfig.LintReport       `json:"lint"`
	Semantic    *lsconfig.ConfigDiff       `json:"semantic_diff"`
	UnifiedDiff string                     `json:"unified_diff,omitempty"`

	BackupPlan     *backupResult `json:"backup_plan"`
	RestartCommand []string      `json:"restart_command"`
	RestartSource  string        `json:"restart_command_source"`
	VerifyPlan     []string      `json:"post_restart_verify_plan"`
	Blockers       []string      `json:"blockers,omitempty"`
	Notes          []string      `json:"notes,omitempty"`
}

const applySchemaVersion = "apply/1"

func newConfigApplyCmd(flags *rootFlags) *cobra.Command {
	var (
		livePath     string
		contextLines int
		write        bool
	)

	cmd := &cobra.Command{
		Use:   "apply <file>",
		Short: "Plan a config change: unified diff vs live, a content-addressed backup, the exact elevated restart command, and the post-restart verify plan. Never writes.",
		Long: "Plan the application of a candidate config. DRY-RUN IS THE ONLY MODE.\n\n" +
			"What it does:\n" +
			"  1. validates and lints the candidate;\n" +
			"  2. diffs it against the live config, semantically and as unified text;\n" +
			"  3. plans a content-addressed backup of the LIVE file;\n" +
			"  4. PRINTS the exact elevated restart command for a human to run;\n" +
			"  5. PRINTS the post-restart verification plan.\n\n" +
			"What it will never do: write the config, or run the restart.\n\n" +
			"The restart is surfaced rather than executed because the service runs under a\n" +
			"SYSTEM-principal scheduled task: restarting it needs elevation this process\n" +
			"does not have and should not acquire silently, and a restart drops every\n" +
			"resident model — including the memory-stack seats other services depend on.\n" +
			"That is a decision for a human at a keyboard.\n\n" +
			"The splice-and-assert write engine (byte-range splices that refuse to run if\n" +
			"the comment count changes or an untouched region moves) is deliberately NOT\n" +
			"in this build. Writing this file is a capability that gets earned.",
		Example: "  llamaswap-pp-cli config apply ./candidate.yaml\n" +
			"  llamaswap-pp-cli config apply ./candidate.yaml --json\n" +
			"  llamaswap-pp-cli config apply ./candidate.yaml --context 6",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d", ExitConfigInvalid),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if write {
				return usageErr(fmt.Errorf(
					"--write is not implemented, deliberately.\n\n%s\n\nTo apply this change: run `config apply` without --write, take the backup it plans,\nedit the live file yourself, validate it with `config testinstance`, then run the\nrestart command it prints",
					theTrustContract))
			}
			if len(args) == 0 {
				if dryRunOK(flags) {
					return writeDryRun(out, flags, "config apply <file>")
				}
				return cmd.Help()
			}
			candidate := args[0]
			live, err := resolveConfigPath([]string{livePath}, 0)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, fmt.Sprintf("config apply %s (plan only; this command never writes)", candidate))
			}
			plan, err := buildApplyPlan(candidate, live, contextLines)
			if err != nil {
				return err
			}
			if wantsJSON(out, flags) {
				if err := printJSONFiltered(out, plan, flags); err != nil {
					return err
				}
			} else {
				printApplyHuman(cmd, plan)
			}
			if len(plan.Blockers) > 0 {
				return errConfigInvalid(fmt.Errorf("candidate %s has %d blocker(s); not safe to apply", candidate, len(plan.Blockers)))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&livePath, "live", "", "the live config to compare against (default: discovered)")
	cmd.Flags().IntVar(&contextLines, "context", 3, "context lines in the unified diff")
	cmd.Flags().BoolVar(&write, "write", false, "NOT IMPLEMENTED — errors with an explanation of the trust contract")
	return cmd
}

func buildApplyPlan(candidate, live string, context int) (*applyPlan, error) {
	cf, err := loadConfigFile(candidate)
	if err != nil {
		return nil, err
	}
	lf, err := loadConfigFile(live)
	if err != nil {
		return nil, err
	}
	plan := &applyPlan{
		SchemaVersion: applySchemaVersion,
		Mode:          "dry-run (the only mode)",
		Candidate:     cf.Path, CandidateSha: cf.Sha256,
		Live: lf.Path, LiveSha: lf.Sha256,
		Identical: cf.Sha256 == lf.Sha256,
	}

	if plan.Validation, err = lsconfig.Validate(cf); err != nil {
		return nil, err
	}
	for _, is := range plan.Validation.Issues {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("schema: %s %s", is.Pointer, is.Message))
	}
	plan.Lint = lsconfig.Lint(cf, lsconfig.LintOptions{CheckListeners: false})
	for _, fd := range plan.Lint.Findings {
		if fd.Severity == lsconfig.SevError {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("lint %s (%s): %s", fd.Check, fd.Model, fd.Message))
		}
	}

	plan.Semantic = lsconfig.DiffConfigs(lf, cf)
	plan.UnifiedDiff = lsconfig.UnifiedDiff(lf.Path, cf.Path, configFileLines(lf), configFileLines(cf), context)
	if plan.Identical {
		plan.Notes = append(plan.Notes, "candidate is byte-identical to the live config — there is nothing to apply")
	}

	// Backup of the LIVE file, planned but never written here.
	if bp, err := runBackup(lf.Path, "", "pre-apply", false); err == nil {
		plan.BackupPlan = bp
		plan.Notes = append(plan.Notes, bp.Notes...)
	} else {
		plan.Notes = append(plan.Notes, fmt.Sprintf("backup plan unavailable: %v", err))
	}

	plan.RestartCommand, plan.RestartSource = restartCommand(lf.Path)
	plan.VerifyPlan = postRestartVerifyPlan(cf)
	return plan, nil
}

// restartCommand derives the elevated restart for the llama-swap service. The
// registration script next to the config is the authority: it records the task
// name, the principal, and how the operator actually cycles the service. When
// it is unreadable, the schtasks form is emitted with that stated plainly, so
// the caller knows whether the command was READ or ASSUMED.
func restartCommand(configPath string) ([]string, string) {
	script := filepath.Join(filepath.Dir(configPath), "register-task.ps1")
	raw, err := os.ReadFile(script)
	if err != nil {
		return []string{
			"schtasks /End /TN llama-swap",
			"schtasks /Run /TN llama-swap",
		}, fmt.Sprintf("ASSUMED default (could not read %s: %v) — verify the task name before running", script, err)
	}
	text := string(raw)
	taskName := "llama-swap"
	if m := taskNamePattern.FindStringSubmatch(text); m != nil {
		taskName = strings.Trim(m[1], `"'`)
	}
	cmds := []string{
		"# Run from an ELEVATED shell. " + taskName + " runs as a SYSTEM-principal scheduled task,",
		"# so an unelevated process cannot stop or start it.",
		"schtasks /End /TN " + taskName,
		"schtasks /Run /TN " + taskName,
	}
	// register-task.ps1 stops the process directly rather than ending the
	// task; carry its exact form so an operator whose task is in a state
	// schtasks /End will not touch has the pattern that is known to work here.
	if strings.Contains(text, "Stop-Process") && strings.Contains(text, "Start-ScheduledTask") {
		cmds = append(cmds,
			"",
			"# Equivalent PowerShell form, verbatim from "+filepath.Base(script)+":",
			"Get-Process llama-swap -ErrorAction SilentlyContinue | Stop-Process -Force",
			"Start-Sleep -Seconds 2",
			"Start-ScheduledTask -TaskName "+taskName,
		)
	}
	return cmds, "read from " + script
}

// taskNamePattern reads the scheduled-task name out of the registration
// script so the printed restart command names the operator's actual task
// rather than a guessed one.
var taskNamePattern = regexp.MustCompile(`(?i)-TaskName\s+("[^"]+"|'[^']+'|[A-Za-z0-9_.\-]+)`)

func postRestartVerifyPlan(cf *lsconfig.File) []string {
	var keep []string
	for _, m := range cf.Models {
		if m.TTL != nil && *m.TTL < 0 {
			keep = append(keep, m.ID)
		}
	}
	plan := []string{
		fmt.Sprintf("llamaswap-pp-cli verify --expect-models %d", len(cf.Models)),
	}
	if len(keep) > 0 {
		plan = append(plan,
			fmt.Sprintf("llamaswap-pp-cli verify --keepset %s --probe-each", strings.Join(keep, ",")),
			"# keep-set seats must ANSWER, not merely appear in /v1/models — a listed-but-degraded",
			"# embedder is exactly the silent failure this step exists to catch",
		)
	}
	plan = append(plan,
		"llamaswap-pp-cli config drift        # every loaded seat's argv must match the new file",
		"llamaswap-pp-cli ping                # proxy liveness",
	)
	return plan
}

func printApplyHuman(cmd *cobra.Command, p *applyPlan) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s  %s\n", bold("config apply"), yellow("[DRY RUN — this command never writes the config or runs the restart]"))
	fmt.Fprintf(out, "  candidate %s (sha %s)\n", p.Candidate, p.CandidateSha[:16])
	fmt.Fprintf(out, "  live      %s (sha %s)\n", p.Live, p.LiveSha[:16])

	fmt.Fprintf(out, "\n%s\n", bold("1. VALIDATE"))
	if p.Validation.Valid {
		fmt.Fprintf(out, "  %s\n", green("schema OK"))
	} else {
		for _, is := range p.Validation.Issues {
			fmt.Fprintf(out, "  %s %s %s\n", red("FAIL"), is.Pointer, is.Message)
		}
	}

	fmt.Fprintf(out, "\n%s\n", bold("2. LINT"))
	fmt.Fprintf(out, "  %d error  %d warning  %d info  %d skipped\n", p.Lint.Errors, p.Lint.Warnings, p.Lint.Infos, p.Lint.Skipped)
	for _, fd := range p.Lint.Findings {
		if fd.Severity == lsconfig.SevError {
			fmt.Fprintf(out, "  %s %s %s\n", red("ERROR"), fd.Check, fd.Message)
		}
	}

	fmt.Fprintf(out, "\n%s\n", bold("3. DIFF vs LIVE"))
	if p.Identical {
		fmt.Fprintf(out, "  %s\n", green("byte-identical — nothing to apply"))
	} else {
		changed := 0
		for _, md := range p.Semantic.Models {
			if !md.Changed() && !md.CommentChange {
				continue
			}
			changed++
			fmt.Fprintf(out, "  %s %s\n", md.Status, bold(md.Model))
			for _, d := range md.FlagDeltas {
				fmt.Fprintf(out, "      %s\n", d.String())
			}
			for _, fd := range md.FieldDeltas {
				fmt.Fprintf(out, "      ~ %s: %s -> %s\n", fd.Field, dashIfEmpty(fd.From), dashIfEmpty(fd.To))
			}
			if md.CommentChange {
				fmt.Fprintf(out, "      %s\n", yellow("comment block changed"))
			}
		}
		for _, fd := range p.Semantic.TopLevel {
			fmt.Fprintf(out, "  top-level ~ %s: %s -> %s\n", fd.Field, dashIfEmpty(fd.From), dashIfEmpty(fd.To))
		}
		if changed == 0 && len(p.Semantic.TopLevel) == 0 {
			fmt.Fprintf(out, "  no semantic change (text differs: whitespace, comments, or key order)\n")
		}
		if strings.TrimSpace(p.UnifiedDiff) != "" {
			fmt.Fprintf(out, "\n%s\n%s", bold("UNIFIED DIFF"), p.UnifiedDiff)
		}
	}

	fmt.Fprintf(out, "\n%s\n", bold("4. BACKUP PLAN (of the LIVE file, not written by this command)"))
	if p.BackupPlan != nil {
		fmt.Fprintf(out, "  would write %s\n  would index %s\n", p.BackupPlan.File, p.BackupPlan.IndexFile)
		fmt.Fprintf(out, "  run it: llamaswap-pp-cli config backup --label pre-apply\n")
	}

	fmt.Fprintf(out, "\n%s\n", bold("5. RESTART COMMAND — PRINTED, NOT RUN"))
	fmt.Fprintf(out, "  source: %s\n\n", p.RestartSource)
	for _, l := range p.RestartCommand {
		fmt.Fprintf(out, "    %s\n", l)
	}

	fmt.Fprintf(out, "\n%s\n", bold("6. POST-RESTART VERIFY PLAN"))
	for _, l := range p.VerifyPlan {
		fmt.Fprintf(out, "    %s\n", l)
	}

	for _, n := range p.Notes {
		fmt.Fprintf(out, "\n  note: %s\n", n)
	}
	if len(p.Blockers) > 0 {
		fmt.Fprintf(out, "\n%s\n", red(fmt.Sprintf("%d BLOCKER(S) — do not apply this candidate", len(p.Blockers))))
		for _, b := range p.Blockers {
			fmt.Fprintf(out, "  %s\n", b)
		}
	}
}
