// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command: harness-to-roster binding audit.
// pp:data-source live

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/store"
)

// harnessModelKeys are the config keys whose VALUE is a model name the proxy
// must be able to resolve. Enumerated explicitly rather than pattern-matched
// on "*_model": a pattern also catches keys naming a cloud model or an
// upscaler checkpoint, and reporting those as dangling would be a false
// positive that teaches the operator to ignore this command.
var harnessModelKeys = []string{
	"model",
	"agent_model",
	"triage_model",
	"escalation_model",
	"reasoning_model",
	"vision_model",
	"ocr_model",
	"stt_model",
	"stt_model_hq",
	"embed_model",
	"embedding_model",
	"rerank_model",
	"reranker_model",
}

// harnessModelListKeys are keys whose value is a LIST of model names.
var harnessModelListKeys = []string{"memory_stack"}

// nonRosterKeys are model-shaped keys deliberately NOT checked, with the
// reason. Printed in the report so their absence is visibly intentional
// rather than an oversight.
var nonRosterKeys = map[string]string{
	"nim_model":              "names a remote cloud model, not a local llama-swap seat",
	"videogen_upscale_model": "names an upscaler checkpoint file, not a llama-swap seat",
}

type binding struct {
	Key string `json:"key"`
	// Index is set for list-valued keys (memory_stack[0], ...).
	Index    int    `json:"index,omitempty"`
	Value    string `json:"value"`
	Resolved string `json:"resolved_to,omitempty"`
	// Via is id | alias | (empty when dangling).
	Via  string `json:"via,omitempty"`
	OK   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
}

type bindCheckReport struct {
	SchemaVersion string    `json:"schema_version"`
	HarnessConfig string    `json:"harness_config"`
	HarnessSha    string    `json:"harness_config_sha256"`
	Endpoint      string    `json:"endpoint"`
	RosterSize    int       `json:"roster_size"`
	Bindings      []binding `json:"bindings"`
	Dangling      []string  `json:"dangling"`
	// Unbound are roster seats no harness key points at. Not an error — a seat
	// can exist for interactive use — but a seat nobody binds is a seat whose
	// regressions nobody notices.
	Unbound      []string          `json:"unbound_roster_seats"`
	MissingKeys  []string          `json:"absent_keys"`
	NotChecked   map[string]string `json:"not_checked"`
	RowsRecorded int               `json:"rows_recorded"`
	Notes        []string          `json:"notes,omitempty"`
}

const bindCheckSchemaVersion = "bind-check/1"

// DefaultHarnessConfigRelPath is where the local-offload harness keeps its
// config relative to the user's home directory.
var DefaultHarnessConfigRelPath = filepath.Join(".local-offload", "config.json")

func newBindCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bind",
		Short: "Audit which model names other tools are bound to: check.",
		Example: "  llamaswap-pp-cli bind check\n" +
			"  llamaswap-pp-cli bind check --json\n" +
			"  llamaswap-pp-cli bind check --harness-config ~/.local-offload/config.json",
		Annotations: map[string]string{"mcp:read-only": "true", "pp:parent-group": "true"},
		RunE:        parentNoSubcommandRunE(flags),
	}
	cmd.AddCommand(newBindCheckCmd(flags))
	return cmd
}

func newBindCheckCmd(flags *rootFlags) *cobra.Command {
	var (
		harnessConfig string
		noRecord      bool
	)

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Every model name a consuming tool is configured with must resolve in the live roster; report dangling bindings and unbound seats.",
		Long: "Audit a consuming tool's model bindings against the live llama-swap roster.\n\n" +
			"A harness config names seats by id or alias. A seat renamed, retired, or\n" +
			"typo'd in either file breaks a role silently: the tool keeps starting, the\n" +
			"request 404s at call time, and the failure surfaces as \"the vision tier is\n" +
			"broken\" days later. This resolves every model-naming key against\n" +
			"GET /v1/models, alias-aware.\n\n" +
			"It reports both directions:\n" +
			"  dangling — a configured name that resolves to nothing;\n" +
			"  unbound  — a roster seat nothing points at. Not an error, but a seat\n" +
			"             nobody binds is a seat whose regressions nobody notices.\n\n" +
			"Keys naming something that is NOT a llama-swap seat (a cloud model, an\n" +
			"upscaler checkpoint) are listed as explicitly not-checked rather than\n" +
			"silently ignored or falsely reported.\n\n" +
			"Results land in the local bindings_audit table.",
		Example: "  llamaswap-pp-cli bind check\n" +
			"  llamaswap-pp-cli bind check --harness-config ~/.local-offload/config.json --json",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d,%d", ExitModelNotFound, ExitServerUnreachable),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			path, err := resolveHarnessConfig(harnessConfig)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, "bind check against "+path)
			}
			rep, err := runBindCheck(cmd.Context(), flags, path, noRecord || cliutilIsVerifyEnv())
			if err != nil {
				return err
			}
			if wantsJSON(out, flags) {
				if err := printJSONFiltered(out, rep, flags); err != nil {
					return err
				}
			} else {
				printBindCheckHuman(cmd, rep)
			}
			if len(rep.Dangling) > 0 {
				return errModelNotFound(fmt.Errorf("%d binding(s) in %s resolve to no model or alias: %s",
					len(rep.Dangling), path, strings.Join(rep.Dangling, ", ")))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&harnessConfig, "harness-config", "", "path to the consuming tool's JSON config (default: ~/"+filepath.ToSlash(DefaultHarnessConfigRelPath)+")")
	cmd.Flags().BoolVar(&noRecord, "no-record", false, "do not write rows to the local bindings_audit table")
	return cmd
}

func resolveHarnessConfig(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", usageErr(fmt.Errorf("cannot resolve home directory; pass --harness-config: %w", err))
	}
	return filepath.Join(home, DefaultHarnessConfigRelPath), nil
}

func runBindCheck(ctx context.Context, flags *rootFlags, path string, noRecord bool) (*bindCheckReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, usageErr(fmt.Errorf("read harness config %s: %w", path, err))
	}
	sum := sha256.Sum256(raw)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, usageErr(fmt.Errorf("parse harness config %s: %w", path, err))
	}
	roster, resolve, err := fetchRosterAliases(ctx, flags)
	if err != nil {
		return nil, err
	}
	c, cerr := newLoopbackClient(flags)
	endpoint := ""
	if cerr == nil {
		endpoint = c.BaseURL
	}

	rep := &bindCheckReport{
		SchemaVersion: bindCheckSchemaVersion,
		HarnessConfig: path,
		HarnessSha:    hex.EncodeToString(sum[:]),
		Endpoint:      endpoint,
		RosterSize:    len(roster),
		NotChecked:    map[string]string{},
	}
	for k, why := range nonRosterKeys {
		if _, present := cfg[k]; present {
			rep.NotChecked[k] = why
		}
	}

	bound := map[string]bool{}
	appendBinding := func(b binding) {
		if b.OK {
			bound[b.Resolved] = true
		} else {
			label := b.Key
			if b.Index > 0 || strings.HasSuffix(b.Key, "]") {
				label = b.Key
			}
			rep.Dangling = append(rep.Dangling, fmt.Sprintf("%s=%q", label, b.Value))
		}
		rep.Bindings = append(rep.Bindings, b)
	}

	for _, key := range harnessModelKeys {
		v, present := cfg[key]
		if !present {
			rep.MissingKeys = append(rep.MissingKeys, key)
			continue
		}
		name, ok := v.(string)
		if !ok {
			appendBinding(binding{Key: key, Value: fmt.Sprint(v), OK: false, Note: "value is not a string"})
			continue
		}
		appendBinding(resolveBinding(key, 0, name, resolve))
	}

	for _, key := range harnessModelListKeys {
		v, present := cfg[key]
		if !present {
			rep.MissingKeys = append(rep.MissingKeys, key)
			continue
		}
		list, ok := v.([]any)
		if !ok {
			appendBinding(binding{Key: key, Value: fmt.Sprint(v), OK: false, Note: "value is not a list"})
			continue
		}
		for i, item := range list {
			name, ok := item.(string)
			if !ok {
				appendBinding(binding{Key: fmt.Sprintf("%s[%d]", key, i), Index: i, Value: fmt.Sprint(item), OK: false, Note: "element is not a string"})
				continue
			}
			appendBinding(resolveBinding(fmt.Sprintf("%s[%d]", key, i), i, name, resolve))
		}
	}

	for _, m := range roster {
		if !bound[m.ID] {
			rep.Unbound = append(rep.Unbound, m.ID)
		}
	}
	sort.Strings(rep.Unbound)
	sort.Strings(rep.MissingKeys)

	if !noRecord {
		n, err := recordBindings(ctx, rep)
		rep.RowsRecorded = n
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("bindings_audit not written: %v", err))
		}
	}
	return rep, nil
}

func resolveBinding(key string, idx int, name string, resolve map[string]string) binding {
	b := binding{Key: key, Index: idx, Value: name}
	if strings.TrimSpace(name) == "" {
		b.Note = "empty value — the role is unconfigured, which fails at call time rather than at startup"
		return b
	}
	target, ok := resolve[name]
	if !ok {
		b.Note = "resolves to no model id and no alias in the live roster"
		return b
	}
	b.OK = true
	b.Resolved = target
	if target == name {
		b.Via = "id"
	} else {
		b.Via = "alias"
	}
	return b
}

func recordBindings(ctx context.Context, rep *bindCheckReport) (int, error) {
	s, err := store.OpenWithContext(ctx, defaultDBPath("llamaswap-pp-cli"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = s.Close() }()
	if err := store.EnsureDomainSchema(ctx, s.DB()); err != nil {
		return 0, err
	}
	stmt, err := s.DB().PrepareContext(ctx, `
		INSERT INTO bindings_audit (ts, harness_config_sha, key, model, resolved_ok, note)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()
	ts := time.Now().UTC().Format(time.RFC3339)
	n := 0
	for _, b := range rep.Bindings {
		ok := 0
		if b.OK {
			ok = 1
		}
		if _, err := stmt.ExecContext(ctx, ts, rep.HarnessSha, b.Key, b.Value, ok, b.Note); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func printBindCheckHuman(cmd *cobra.Command, rep *bindCheckReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n", bold("bind check"))
	fmt.Fprintf(out, "  harness  %s (sha %s)\n", rep.HarnessConfig, rep.HarnessSha[:16])
	fmt.Fprintf(out, "  endpoint %s (%d roster seats)\n\n", rep.Endpoint, rep.RosterSize)

	w := newTabWriter(out)
	fmt.Fprintln(w, "  STATUS\tKEY\tVALUE\tRESOLVES TO\tVIA")
	for _, b := range rep.Bindings {
		status := green("OK")
		if !b.OK {
			status = red("DANGLING")
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", status, b.Key, b.Value, dashIfEmpty(b.Resolved), dashIfEmpty(b.Via))
	}
	_ = w.Flush()

	for _, b := range rep.Bindings {
		if !b.OK && b.Note != "" {
			fmt.Fprintf(out, "\n  %s %s=%q: %s\n", red("DANGLING"), b.Key, b.Value, b.Note)
		}
	}

	if len(rep.MissingKeys) > 0 {
		fmt.Fprintf(out, "\n%s\n  %s\n", bold("KEYS NOT PRESENT in the harness config"), strings.Join(rep.MissingKeys, ", "))
		fmt.Fprintf(out, "  (absent is fine when the tool has no such role; it is a bug when the role exists and nothing configures it)\n")
	}
	if len(rep.NotChecked) > 0 {
		fmt.Fprintf(out, "\n%s\n", bold("MODEL-SHAPED KEYS DELIBERATELY NOT CHECKED"))
		for _, k := range sortedMapKeys(rep.NotChecked) {
			fmt.Fprintf(out, "  %-24s %s\n", k, rep.NotChecked[k])
		}
	}
	if len(rep.Unbound) > 0 {
		fmt.Fprintf(out, "\n%s\n  %s\n", bold("UNBOUND ROSTER SEATS (configured in llama-swap, no harness key points at them)"), strings.Join(rep.Unbound, ", "))
		fmt.Fprintf(out, "  Not an error — a seat can exist for interactive use. But a seat nothing binds\n  is a seat whose regressions nobody notices.\n")
	}
	fmt.Fprintln(out)
	if len(rep.Dangling) == 0 {
		fmt.Fprintf(out, "%s\n", green(fmt.Sprintf("all %d binding(s) resolve", len(rep.Bindings))))
	} else {
		fmt.Fprintf(out, "%s\n", red(fmt.Sprintf("%d of %d binding(s) resolve to nothing", len(rep.Dangling), len(rep.Bindings))))
	}
	if rep.RowsRecorded > 0 {
		fmt.Fprintf(out, "recorded %d row(s) in bindings_audit\n", rep.RowsRecorded)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(out, "note: %s\n", n)
	}
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
