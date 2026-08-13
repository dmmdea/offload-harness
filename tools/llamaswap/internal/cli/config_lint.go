// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source local

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

func newConfigLintCmd(flags *rootFlags) *cobra.Command {
	var (
		noListenerCheck bool
		minSeverity     string
	)

	cmd := &cobra.Command{
		Use:   "lint [file]",
		Short: "Semantic checks llama-swap's own boot validation does not make: macros, aliases, ports, ttl semantics, missing files, routing coherence.",
		Long: "Lint a llama-swap config for the failures that pass schema validation and\n" +
			"still break a running service.\n\n" +
			"Check families: macros (reserved/undefined/unused/cycles, ${env.VAR} unset),\n" +
			"aliases (duplicates, alias-shadows-a-model-id), ports (hardcoded --port,\n" +
			"startPort-span collisions, a LIVE listener probe on this host, proxy targets\n" +
			"outside the span), ttl semantics (ttl:0 vs globalTTL, keep-resident seats),\n" +
			"missing files behind -m/--mmproj/--model-draft, per-seat binary existence and\n" +
			"build drift, routing coherence (matrix vars and sets resolve, evict_costs keyed\n" +
			"by VAR id not model id, exactly one of groups/matrix), profile and selector\n" +
			"integrity, apiKeys hygiene (counted, never printed), and store.path.\n\n" +
			"THE ESCAPE HATCH: a seat whose cmd runs something other than llama-server\n" +
			"(whisper-server, a wrapper) is classified non-llama-server, and every\n" +
			"llama-server-specific check SKIPS it with an explicit note. It is never\n" +
			"reported as missing -ngl or a context size it has no concept of. One noisy\n" +
			"false positive on a legitimate seat and lint stops getting run.\n\n" +
			"Exit 0 when clean or warnings-only; " + fmt.Sprint(ExitConfigInvalid) + " when any error-severity finding fires.",
		Example: "  llamaswap-pp-cli config lint\n" +
			"  llamaswap-pp-cli config lint --json\n" +
			"  llamaswap-pp-cli config lint ./candidate.yaml --min-severity warning",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d", ExitConfigInvalid),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			path, err := resolveConfigPath(args, 0)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, "config lint "+path)
			}
			f, err := loadConfigFile(path)
			if err != nil {
				return err
			}
			rep := lsconfig.Lint(f, lsconfig.LintOptions{
				// The listener probe touches only 127.0.0.1 and only connects;
				// it never binds and never sends a byte.
				CheckListeners: !noListenerCheck && !cliutilIsVerifyEnv(),
			})
			rep.Findings = filterFindings(rep.Findings, minSeverity)

			if wantsJSON(out, flags) {
				if err := printJSONFiltered(out, rep, flags); err != nil {
					return err
				}
			} else {
				printLintHuman(cmd, rep, f)
			}
			if rep.Errors > 0 {
				return errConfigInvalid(fmt.Errorf("%s: %d error-severity finding(s)", path, rep.Errors))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noListenerCheck, "no-listener-check", false, "skip the live 127.0.0.1 listener probe for hardcoded ports")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "skipped", "lowest severity to report: error, warning, info, skipped")
	return cmd
}

func filterFindings(in []lsconfig.Finding, min string) []lsconfig.Finding {
	rank := map[string]int{"error": 0, "warning": 1, "info": 2, "skipped": 3}
	limit, ok := rank[strings.ToLower(strings.TrimSpace(min))]
	if !ok {
		limit = 3
	}
	out := make([]lsconfig.Finding, 0, len(in))
	for _, f := range in {
		if r, ok := rank[string(f.Severity)]; ok && r <= limit {
			out = append(out, f)
		}
	}
	return out
}

func printLintHuman(cmd *cobra.Command, rep *lsconfig.LintReport, f *lsconfig.File) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n", bold(rep.Path))
	fmt.Fprintf(out, "  sha256 %s  models %d\n", rep.Sha256[:16], rep.Models)
	if len(rep.NonLlamaServerSeats) > 0 {
		fmt.Fprintf(out, "  non-llama-server seats (llama-server checks skipped, by design): %s\n",
			strings.Join(rep.NonLlamaServerSeats, ", "))
	}
	fmt.Fprintln(out)
	if len(rep.Findings) == 0 {
		fmt.Fprintf(out, "%s\n", green("clean — no findings"))
		return
	}
	w := newTabWriter(out)
	fmt.Fprintln(w, "SEVERITY\tLINE\tCHECK\tMODEL\tMESSAGE")
	for _, fd := range rep.Findings {
		line := ""
		if fd.Line > 0 {
			line = fmt.Sprint(fd.Line)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", severityMark(fd.Severity), line, fd.Check, fd.Model, fd.Message)
	}
	_ = w.Flush()

	for _, fd := range rep.Findings {
		if strings.TrimSpace(fd.Detail) == "" {
			continue
		}
		fmt.Fprintf(out, "\n%s (%s)\n  %s\n", fd.Check, fd.Severity, fd.Detail)
	}
	fmt.Fprintf(out, "\n%d error  %d warning  %d info  %d skipped\n", rep.Errors, rep.Warnings, rep.Infos, rep.Skipped)
	if rep.Errors == 0 {
		fmt.Fprintf(out, "%s\n", green("no error-severity findings"))
	}
}
