// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source local

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

// explainResult is the machine-readable shape of `config explain`.
type explainResult struct {
	ConfigPath   string                   `json:"config_path"`
	ConfigSha    string                   `json:"config_sha256"`
	Model        string                   `json:"model"`
	MatchedBy    string                   `json:"matched_by"`
	SeatKind     lsconfig.SeatKind        `json:"seat_kind"`
	Binary       string                   `json:"binary"`
	Name         string                   `json:"name,omitempty"`
	Description  string                   `json:"description,omitempty"`
	Aliases      []string                 `json:"aliases,omitempty"`
	TTL          *int                     `json:"ttl,omitempty"`
	TTLMeaning   string                   `json:"ttl_meaning,omitempty"`
	Env          []string                 `json:"env,omitempty"`
	Filters      map[string]any           `json:"filters,omitempty"`
	CheckPath    string                   `json:"check_endpoint,omitempty"`
	CmdRaw       string                   `json:"cmd_raw"`
	CmdExpanded  string                   `json:"cmd_expanded"`
	Flags        []lsconfig.Flag          `json:"flags"`
	Expansions   []lsconfig.Expansion     `json:"expansions,omitempty"`
	Lines        [2]int                   `json:"lines"`
	Header       string                   `json:"header_comment,omitempty"`
	Inline       []lsconfig.InlineComment `json:"inline_comments,omitempty"`
	RawBlock     string                   `json:"raw_block"`
	MatrixVar    string                   `json:"matrix_var,omitempty"`
	MatrixSets   []string                 `json:"matrix_sets,omitempty"`
	SkippedNotes []string                 `json:"skipped_checks,omitempty"`
}

func newConfigExplainCmd(flags *rootFlags) *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "explain <model>",
		Short: "The fully resolved view of one seat: raw block with comments, macro-expanded cmd, aliases, ttl, env, seat kind.",
		Long: "Explain what a seat ACTUALLY runs.\n\n" +
			"The config shows a stanza with ${macros} in it; llama-swap's UI shows the same\n" +
			"stanza. Neither shows the effective command. This joins them: the raw source\n" +
			"block (comments included, copied verbatim), the macro-expanded command line\n" +
			"broken into flags, and which macro expanded to what.\n\n" +
			"<model> resolves by model id OR by alias.",
		Example: "  llamaswap-pp-cli config explain gemma-4-e4b\n" +
			"  llamaswap-pp-cli config explain bge-reranker-v2-m3 --json\n" +
			"  llamaswap-pp-cli config explain offload-e4b   # alias resolves too",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d,%d", ExitModelNotFound, ExitConfigInvalid),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			// Verify-friendly: no cobra Args validator, so --dry-run reaches
			// the guard below instead of being rejected before RunE runs.
			if len(args) == 0 {
				if dryRunOK(flags) {
					return writeDryRun(out, flags, "config explain <model>")
				}
				return cmd.Help()
			}
			path, err := resolveConfigPath([]string{configPath}, 0)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, "config explain "+args[0])
			}
			f, err := loadConfigFile(path)
			if err != nil {
				return err
			}
			m, ok := f.Resolve(args[0])
			if !ok {
				return errModelNotFound(fmt.Errorf("no model or alias %q in %s (known ids: %s)",
					args[0], path, strings.Join(modelIDs(f), ", ")))
			}
			res := buildExplain(f, m, args[0])
			if wantsJSON(out, flags) {
				return printJSONFiltered(out, res, flags)
			}
			printExplainHuman(cmd, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config-file", "", "llama-swap YAML to read (default: the live config)")
	return cmd
}

func modelIDs(f *lsconfig.File) []string {
	out := make([]string, 0, len(f.Models))
	for _, m := range f.Models {
		out = append(out, m.ID)
	}
	return out
}

func buildExplain(f *lsconfig.File, m *lsconfig.Model, query string) *explainResult {
	spec := lsconfig.ParseCmd(m.CmdExpanded)
	res := &explainResult{
		ConfigPath: f.Path, ConfigSha: f.Sha256,
		Model: m.ID, MatchedBy: matchedBy(m, query),
		SeatKind: m.Seat, Binary: m.Binary,
		Name: m.Name, Description: m.Description,
		Aliases: m.Aliases, TTL: m.TTL, Env: m.Env, Filters: m.Filters,
		CheckPath:   m.CheckPath,
		CmdRaw:      m.CmdRaw,
		CmdExpanded: m.CmdExpanded,
		Flags:       spec.Flags,
		Expansions:  m.Expansions,
		Lines:       [2]int{m.StartLine, m.EndLine},
		Header:      m.HeaderComment,
		Inline:      m.InlineComments,
		RawBlock:    m.RawBlock(),
	}
	res.TTLMeaning = ttlMeaning(m.TTL, f.GlobalTTL)
	if f.Matrix != nil {
		for v, target := range f.Matrix.Vars {
			if target != m.ID {
				continue
			}
			res.MatrixVar = v
			for _, setName := range lsconfig.SortedKeys(f.Matrix.Sets) {
				if setMentionsVar(f.Matrix.Sets[setName], v) {
					res.MatrixSets = append(res.MatrixSets, setName)
				}
			}
		}
	}
	if m.Seat != lsconfig.SeatLlamaServer {
		res.SkippedNotes = append(res.SkippedNotes, fmt.Sprintf(
			"seat kind %s: GGUF, context-window and llama-server flag checks are skipped for this seat everywhere in this CLI (binary is %s)",
			m.Seat, lsconfig.BinaryBase(m.Binary)))
	}
	return res
}

func matchedBy(m *lsconfig.Model, query string) string {
	if m.ID == query {
		return "id"
	}
	for _, a := range m.Aliases {
		if a == query {
			return "alias:" + a
		}
	}
	return "id"
}

func ttlMeaning(ttl *int, global *int) string {
	switch {
	case ttl == nil && global == nil:
		return "unset and no globalTTL — the seat is never auto-unloaded"
	case ttl == nil:
		return fmt.Sprintf("unset — inherits globalTTL %d seconds", *global)
	case *ttl < 0:
		return "keep-resident (never auto-unload). NOTE: the live API reports ttl:0 for this seat, so a keep-set must be read from the config, never from the server"
	case *ttl == 0:
		return "0 means no TTL — never auto-unloaded (it does NOT mean unload immediately, and it does NOT inherit globalTTL)"
	default:
		return fmt.Sprintf("auto-unload after %d seconds idle", *ttl)
	}
}

func setMentionsVar(expr, v string) bool {
	for _, ref := range strings.FieldsFunc(expr, func(r rune) bool {
		return !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		if ref == v {
			return true
		}
	}
	return false
}

func printExplainHuman(cmd *cobra.Command, r *explainResult) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s", bold(r.Model))
	if r.MatchedBy != "id" {
		fmt.Fprintf(out, "  (matched by %s)", r.MatchedBy)
	}
	fmt.Fprintln(out)
	if r.Name != "" {
		fmt.Fprintf(out, "  %s\n", r.Name)
	}
	fmt.Fprintf(out, "  seat kind   %s (binary %s)\n", r.SeatKind, r.Binary)
	fmt.Fprintf(out, "  source      %s lines %d-%d\n", r.ConfigPath, r.Lines[0], r.Lines[1])
	if len(r.Aliases) > 0 {
		fmt.Fprintf(out, "  aliases     %s\n", strings.Join(r.Aliases, ", "))
	}
	fmt.Fprintf(out, "  ttl         %s — %s\n", ttlDisplay(r.TTL), r.TTLMeaning)
	for _, e := range r.Env {
		fmt.Fprintf(out, "  env         %s\n", e)
	}
	if r.CheckPath != "" {
		fmt.Fprintf(out, "  health      %s\n", r.CheckPath)
	}
	if r.MatrixVar != "" {
		fmt.Fprintf(out, "  matrix      var %q in set(s): %s\n", r.MatrixVar, strings.Join(r.MatrixSets, ", "))
	}

	fprintBlock(out, "RAW BLOCK (verbatim from the file, comments are the decision record)", r.RawBlock)

	fmt.Fprintf(out, "\n%s\n", bold("EXPANDED COMMAND"))
	fmt.Fprintf(out, "  %s\n", r.CmdExpanded)

	if len(r.Flags) > 0 {
		fmt.Fprintf(out, "\n%s\n", bold("FLAGS"))
		w := newTabWriter(out)
		fmt.Fprintln(w, "  FLAG\tVALUE")
		for _, fl := range r.Flags {
			fmt.Fprintf(w, "  %s\t%s\n", fl.Name, strings.Join(fl.Values, " "))
		}
		_ = w.Flush()
	}

	if len(r.Expansions) > 0 {
		fmt.Fprintf(out, "\n%s\n", bold("MACRO EXPANSION"))
		w := newTabWriter(out)
		fmt.Fprintln(w, "  TOKEN\tKIND\tVALUE")
		for _, e := range r.Expansions {
			val := e.Value
			switch e.Kind {
			case "reserved":
				val = "(left symbolic — llama-swap substitutes it at spawn time)"
			case "env-unset":
				val = "(unset in this process environment)"
			case "undefined":
				val = "(NOT DECLARED in macros:)"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\n", e.Token, e.Kind, val)
		}
		_ = w.Flush()
	}

	for _, n := range r.SkippedNotes {
		fmt.Fprintf(out, "\nnote: %s\n", n)
	}
}

func ttlDisplay(t *int) string {
	if t == nil {
		return "(unset)"
	}
	return fmt.Sprint(*t)
}
