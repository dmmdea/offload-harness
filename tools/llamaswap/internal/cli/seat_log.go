// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command: the per-seat change chronology.
// pp:data-source local

package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
	"llamaswap-pp-cli/internal/store"
)

// seatLogEntry is one distinct state of a seat in the chronology.
type seatLogEntry struct {
	Source string `json:"source"`
	// Label is the FILENAME. It is a label an operator typed, nothing more.
	Label string `json:"label"`
	// LabelDate is the date in the filename; MTime is the truth.
	LabelDate         string `json:"label_date,omitempty"`
	LabelDateMismatch bool   `json:"label_date_mismatch,omitempty"`
	MTime             string `json:"mtime"`
	ConfigSha         string `json:"config_sha256"`
	CmdSha            string `json:"cmd_sha256"`
	// IsLive is a CONTENT-level claim: this state is byte-identical to what
	// the live config file holds right now. The timeline dedupes by content
	// hash, so the row carrying it may be a backup file that represents the
	// live content.
	IsLive bool `json:"is_live"`
	// Duplicates names other files with byte-identical CONTENT.
	Duplicates []string `json:"duplicate_sources,omitempty"`

	// Present is false for a state in which the seat did not exist at all.
	Present  bool                  `json:"present"`
	Cmd      string                `json:"cmd,omitempty"`
	SeatKind lsconfig.SeatKind     `json:"seat_kind,omitempty"`
	TTL      *int                  `json:"ttl,omitempty"`
	Aliases  []string              `json:"aliases,omitempty"`
	Deltas   []lsconfig.FlagDelta  `json:"flag_deltas,omitempty"`
	Fields   []lsconfig.FieldDelta `json:"field_deltas,omitempty"`
	// Comment is the seat's leading comment block IN THIS STATE, and Inline
	// are the trailing `# ...` notes on its own keys. Both are carried because
	// operators annotate in BOTH places: a header block for the seat's purpose,
	// and an inline note on the cmd line for the flag that just moved. Reading
	// only the header misses exactly the annotations that accompany a flag
	// change, which is the whole point of mining the file series over an API.
	Comment        string                   `json:"comment,omitempty"`
	Inline         []lsconfig.InlineComment `json:"inline_comments,omitempty"`
	CommentChanged bool                     `json:"comment_changed,omitempty"`
	// Change is: created | changed | unchanged | removed.
	Change string `json:"change"`
}

// corpusSummary carries the corpus-level facts the chronology rests on. They
// are reported alongside the timeline because a chronology is only as
// trustworthy as the discovery behind it.
type corpusSummary struct {
	Dir                   string                   `json:"dir"`
	HistoricalSources     int                      `json:"historical_sources"`
	FlatHistoricalFiles   int                      `json:"flat_historical_files"`
	DistinctContentStates int                      `json:"distinct_content_states_among_flat"`
	IdenticalPairs        []lsconfig.IdenticalPair `json:"byte_identical_pairs"`
	LabelDateMismatches   []labelMismatch          `json:"label_date_mismatches"`
	OrphanBackups         []string                 `json:"orphan_backups"`
	NonFlatSources        []string                 `json:"non_flat_sources,omitempty"`
	Skipped               []string                 `json:"skipped,omitempty"`
}

type labelMismatch struct {
	File      string `json:"file"`
	LabelDate string `json:"label_date"`
	MTimeDate string `json:"mtime_date"`
}

type seatLogReport struct {
	SchemaVersion string         `json:"schema_version"`
	Model         string         `json:"model"`
	MatchedBy     string         `json:"matched_by,omitempty"`
	ConfigPath    string         `json:"config_path"`
	Corpus        corpusSummary  `json:"corpus"`
	States        int            `json:"states"`
	Changes       int            `json:"changes"`
	Timeline      []seatLogEntry `json:"timeline"`
	Recorded      int            `json:"rows_recorded"`
	Notes         []string       `json:"notes,omitempty"`
}

const seatLogSchemaVersion = "seat-log/1"

func newNovelSeatLogCmd(flags *rootFlags) *cobra.Command {
	var (
		configPath string
		noRecord   bool
		corpusOnly bool
	)

	cmd := &cobra.Command{
		Use:   "log <model>",
		Short: "A per-model chronology of every flag change mined from the dated config-backup series, with the comment that landed alongside each one.",
		Long: "Reconstruct a seat's history from the config backups on disk.\n\n" +
			"llama-swap knows what a seat is running now. It has never known what the seat\n" +
			"ran last week, or why the context size moved, or which flag was added the day\n" +
			"the error rate dropped. That history exists — in the operator's backup series —\n" +
			"and nothing reads it.\n\n" +
			"This walks that corpus in mtime order, deduplicates by CONTENT HASH (two\n" +
			"byte-identical backups are one state, not two), and prints the flag deltas\n" +
			"between consecutive states together with the seat's comment block as it stood\n" +
			"in each one. When a flag changed and the comment changed with it, the comment\n" +
			"is the reason.\n\n" +
			"Discovery rules that make the result trustworthy:\n" +
			"  - recursive and extension-tolerant, so dated backup SUBDIRECTORIES and\n" +
			"    non-.yaml-suffixed copies (llama-swap.yaml.pre-matrix) are found, not just\n" +
			"    what a backup-*.yaml glob would catch;\n" +
			"  - FILENAMES ARE LABELS ONLY. A backup dated 2026-08-06 whose mtime is\n" +
			"    2026-08-05 is ordered by its mtime and flagged, never trusted;\n" +
			"  - byte-identical copies are reported as such rather than replayed as\n" +
			"    separate events.\n\n" +
			"Rows land in the local seat_config_history table so later commands can join\n" +
			"a benchmark or an error-rate shift to the exact seat configuration that\n" +
			"produced it.",
		Example: "  llamaswap-pp-cli seat log gemma-4-e4b\n" +
			"  llamaswap-pp-cli seat log bge-reranker-v2-m3 --json\n" +
			"  llamaswap-pp-cli seat log --corpus-only     # just the corpus audit",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d,%d", ExitModelNotFound, ExitConfigInvalid),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 0 && !corpusOnly {
				if dryRunOK(flags) {
					return writeDryRun(out, flags, "seat log <model>")
				}
				return cmd.Help()
			}
			model := ""
			if len(args) > 0 {
				model = args[0]
			}
			path, err := resolveConfigPath([]string{configPath}, 0)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, "seat log "+model)
			}
			rep, err := buildSeatLog(cmd.Context(), path, model, corpusOnly, noRecord || cliutilIsVerifyEnv())
			if err != nil {
				return err
			}
			if wantsJSON(out, flags) {
				return printJSONFiltered(out, rep, flags)
			}
			printSeatLogHuman(cmd, rep, corpusOnly)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config-file", "", "llama-swap YAML whose directory anchors the corpus (default: the live config)")
	cmd.Flags().BoolVar(&noRecord, "no-record", false, "do not write rows to the local seat_config_history table")
	cmd.Flags().BoolVar(&corpusOnly, "corpus-only", false, "print only the corpus audit (sources, distinct content states, identical pairs, label/mtime mismatches)")
	return cmd
}

func buildSeatLog(ctx context.Context, configPath, model string, corpusOnly, noRecord bool) (*seatLogReport, error) {
	live, err := loadConfigFile(configPath)
	if err != nil {
		return nil, err
	}
	corpus, err := lsconfig.DiscoverCorpus(configPath, lsconfig.DiscoverOptions{})
	if err != nil {
		return nil, err
	}

	rep := &seatLogReport{
		SchemaVersion: seatLogSchemaVersion,
		Model:         model,
		ConfigPath:    live.Path,
		Corpus: corpusSummary{
			Dir:                   corpus.Dir,
			HistoricalSources:     len(corpus.Historical),
			FlatHistoricalFiles:   len(corpus.FlatHistorical),
			DistinctContentStates: corpus.DistinctFlatStates,
			IdenticalPairs:        corpus.IdenticalPairs,
			OrphanBackups:         corpus.OrphanBackups,
			Skipped:               corpus.Skipped,
		},
	}
	for _, s := range corpus.LabelMismatches {
		rep.Corpus.LabelDateMismatches = append(rep.Corpus.LabelDateMismatches,
			labelMismatch{File: s.Rel, LabelDate: s.LabelDate, MTimeDate: s.MTimeDate})
	}
	for _, s := range corpus.Historical {
		if !s.Flat {
			rep.Corpus.NonFlatSources = append(rep.Corpus.NonFlatSources, s.Rel)
		}
	}
	if corpusOnly {
		return rep, nil
	}

	target, ok := live.Resolve(model)
	canonical := model
	if ok {
		canonical = target.ID
		rep.Model = canonical
		rep.MatchedBy = matchedBy(target, model)
	} else {
		// The seat may exist only in HISTORY — a removed seat is exactly the
		// case a chronology should be able to answer. Do not fail yet.
		rep.Notes = append(rep.Notes, fmt.Sprintf("%q is not in the current config; searching history for a seat that was removed", model))
	}

	var prev *lsconfig.Model
	everPresent := false
	for _, src := range corpus.Chronology() {
		f, perr := lsconfig.ParseBytes(src.Bytes(), src.Path, lsconfig.LoadOptions{})
		if perr != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: unparseable, skipped (%v)", src.Rel, perr))
			continue
		}
		m, present := f.ModelIndex[canonical]
		if !present {
			if alt, ok := f.Resolve(model); ok {
				m, present = alt, true
			}
		}
		// The timeline is deduplicated by CONTENT hash, so when the live
		// config is byte-identical to a backup the backup can be the
		// group's representative. is_live is therefore a content-level
		// claim: this state IS what the live file holds right now — not
		// "this path is the live file".
		isLiveState := src.IsLive || (corpus.Live != nil && corpus.Live.Sha256 == src.Sha256)
		entry := seatLogEntry{
			Source: src.Rel, Label: src.Label,
			LabelDate: src.LabelDate, LabelDateMismatch: src.LabelDateMismatch,
			MTime:     src.ModTime.Format(time.RFC3339),
			ConfigSha: src.Sha256, IsLive: isLiveState,
			Duplicates: corpus.DuplicatesOf(src),
			Present:    present,
		}
		switch {
		case present:
			everPresent = true
			entry.Cmd = m.CmdExpanded
			entry.CmdSha = shortSha(m.CmdExpanded)
			entry.SeatKind = m.Seat
			entry.TTL = m.TTL
			entry.Aliases = m.Aliases
			entry.Comment = m.HeaderComment
			entry.Inline = m.InlineComments
			if prev == nil {
				entry.Change = "created"
				rep.Changes++
			} else {
				entry.Deltas = lsconfig.DiffCmds(prev.CmdExpanded, m.CmdExpanded)
				entry.Fields = seatFieldDeltas(prev, m)
				entry.CommentChanged = seatAnnotation(prev) != seatAnnotation(m)
				if len(entry.Deltas) > 0 || len(entry.Fields) > 0 {
					entry.Change = "changed"
					rep.Changes++
				} else {
					entry.Change = "unchanged"
				}
			}
			prev = m
		case prev != nil:
			entry.Change = "removed"
			rep.Changes++
			prev = nil
		default:
			entry.Change = "absent"
		}
		// Only states where the seat existed, or where it changed, earn a row.
		if entry.Change != "absent" {
			rep.Timeline = append(rep.Timeline, entry)
		}
	}
	rep.States = len(rep.Timeline)

	if !everPresent {
		return nil, errModelNotFound(fmt.Errorf("no model or alias %q in %s or in any of the %d historical sources under %s",
			model, live.Path, len(corpus.Historical), corpus.Dir))
	}
	if !noRecord {
		n, err := recordSeatHistory(ctx, canonical, rep.Timeline)
		rep.Recorded = n
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("local history not recorded: %v", err))
		}
	}
	return rep, nil
}

func seatFieldDeltas(a, b *lsconfig.Model) []lsconfig.FieldDelta {
	var out []lsconfig.FieldDelta
	cmp := func(field, av, bv string) {
		if av != bv {
			out = append(out, lsconfig.FieldDelta{Field: field, From: av, To: bv})
		}
	}
	cmp("ttl", ttlDisplayRaw(a.TTL), ttlDisplayRaw(b.TTL))
	cmp("aliases", strings.Join(a.Aliases, ","), strings.Join(b.Aliases, ","))
	cmp("env", strings.Join(a.Env, ","), strings.Join(b.Env, ","))
	cmp("name", a.Name, b.Name)
	cmp("checkEndpoint", a.CheckPath, b.CheckPath)
	return out
}

func ttlDisplayRaw(t *int) string {
	if t == nil {
		return ""
	}
	return fmt.Sprint(*t)
}

// seatAnnotation is everything the operator wrote about this seat in this
// state: the leading comment block plus every inline note on its own keys,
// normalized for comparison. Both halves matter — in a config whose seats
// carry no header block, the inline note on the cmd line IS the reasoning.
func seatAnnotation(m *lsconfig.Model) string {
	parts := []string{normalizeCommentText(m.HeaderComment)}
	for _, ic := range m.InlineComments {
		parts = append(parts, ic.Key+": "+strings.TrimSpace(ic.Text))
	}
	return strings.Join(parts, "\n")
}

func normalizeCommentText(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// recordSeatHistory writes the chronology into seat_config_history. The table
// is UNIQUE(content_sha, model), so re-running the command is idempotent
// instead of duplicating the timeline.
func recordSeatHistory(ctx context.Context, model string, timeline []seatLogEntry) (int, error) {
	s, err := store.OpenWithContext(ctx, defaultDBPath("llamaswap-pp-cli"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = s.Close() }()
	if err := store.EnsureDomainSchema(ctx, s.DB()); err != nil {
		return 0, err
	}
	stmt, err := s.DB().PrepareContext(ctx, `
		INSERT INTO seat_config_history
			(source_file, content_sha, file_mtime, model, cmd_sha, full_cmd, comment_block, first_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(content_sha, model) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()
	now := time.Now().UTC().Format(time.RFC3339)
	n := 0
	for _, e := range timeline {
		if !e.Present {
			continue
		}
		res, err := stmt.ExecContext(ctx, e.Source, e.ConfigSha, e.MTime, model, e.CmdSha, e.Cmd, e.Comment, now)
		if err != nil {
			return n, err
		}
		if affected, err := res.RowsAffected(); err == nil && affected > 0 {
			n++
		}
	}
	return n, nil
}

func printSeatLogHuman(cmd *cobra.Command, rep *seatLogReport, corpusOnly bool) {
	out := cmd.OutOrStdout()
	c := rep.Corpus
	fmt.Fprintf(out, "%s\n", bold("CORPUS"))
	fmt.Fprintf(out, "  dir                      %s\n", c.Dir)
	fmt.Fprintf(out, "  historical sources       %d  (recursive, extension-tolerant)\n", c.HistoricalSources)
	fmt.Fprintf(out, "  flat historical files    %d\n", c.FlatHistoricalFiles)
	fmt.Fprintf(out, "  distinct content states  %d  (among the flat files)\n", c.DistinctContentStates)
	if len(c.NonFlatSources) > 0 {
		fmt.Fprintf(out, "  found outside the flat glob:\n")
		for _, s := range c.NonFlatSources {
			fmt.Fprintf(out, "    %s\n", s)
		}
	}
	if len(c.IdenticalPairs) > 0 {
		fmt.Fprintf(out, "  byte-identical groups    %d\n", len(c.IdenticalPairs))
		for _, p := range c.IdenticalPairs {
			fmt.Fprintf(out, "    %s  %s\n", p.Sha256[:10], strings.Join(p.Paths, " == "))
		}
	}
	if len(c.LabelDateMismatches) > 0 {
		fmt.Fprintf(out, "  %s %d (filename says one date, the file was written on another — mtime wins)\n",
			yellow("label/mtime mismatches"), len(c.LabelDateMismatches))
		for _, m := range c.LabelDateMismatches {
			fmt.Fprintf(out, "    %-58s label %s  mtime %s\n", m.File, m.LabelDate, m.MTimeDate)
		}
	}
	if len(c.OrphanBackups) > 0 {
		fmt.Fprintf(out, "  %s %s\n", yellow("orphan backup(s):"), strings.Join(c.OrphanBackups, ", "))
		fmt.Fprintf(out, "    byte-identical to the LIVE config — the change they were named for was never applied,\n")
		fmt.Fprintf(out, "    or the backup was taken after the change instead of before it.\n")
	}
	for _, s := range c.Skipped {
		fmt.Fprintf(out, "  skipped: %s\n", s)
	}
	if corpusOnly {
		return
	}

	fmt.Fprintf(out, "\n%s %s   %d state(s), %d change(s)\n", bold("SEAT"), bold(rep.Model), rep.States, rep.Changes)
	for _, e := range rep.Timeline {
		marker := " "
		switch e.Change {
		case "created":
			marker = green("+")
		case "changed":
			marker = yellow("~")
		case "removed":
			marker = red("-")
		}
		live := ""
		if e.IsLive {
			live = bold("  <- LIVE")
		}
		fmt.Fprintf(out, "\n%s %s  %s%s\n", marker, e.MTime[:19], e.Source, live)
		if e.LabelDateMismatch {
			fmt.Fprintf(out, "    %s filename says %s, file was written %s\n", yellow("label/mtime mismatch:"), e.LabelDate, e.MTime[:10])
		}
		if len(e.Duplicates) > 0 {
			fmt.Fprintf(out, "    byte-identical to: %s\n", strings.Join(e.Duplicates, ", "))
		}
		switch e.Change {
		case "created":
			fmt.Fprintf(out, "    first seen: %s\n", e.Cmd)
		case "removed":
			fmt.Fprintf(out, "    %s\n", red("seat REMOVED from the config in this state"))
		case "changed":
			for _, d := range e.Deltas {
				fmt.Fprintf(out, "    %s\n", d.String())
			}
			for _, fd := range e.Fields {
				fmt.Fprintf(out, "    ~ %s: %s -> %s\n", fd.Field, dashIfEmpty(fd.From), dashIfEmpty(fd.To))
			}
		default:
			fmt.Fprintf(out, "    (no change to this seat)\n")
		}
		if e.Change == "changed" && e.CommentChanged {
			fprintBlock(out, "  comment block that landed with this change", e.Comment)
			if len(e.Inline) > 0 {
				fmt.Fprintf(out, "\n%s\n", bold("  inline notes that landed with this change"))
				for _, ic := range e.Inline {
					fmt.Fprintf(out, "    %s (line %d): %s\n", ic.Key, ic.Line, ic.Text)
				}
			}
		}
	}
	if rep.Recorded > 0 {
		fmt.Fprintf(out, "\nrecorded %d new row(s) in seat_config_history\n", rep.Recorded)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(out, "note: %s\n", n)
	}
}
