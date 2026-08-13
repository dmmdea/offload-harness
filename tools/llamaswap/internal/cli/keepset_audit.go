// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command (wave A spine).
// pp:data-source local
// Supported strategies: auto, local, live, or computed. `local` is deliberate:
// no endpoint reports residency HISTORY, so the answer can only come from the
// mirrored tables.

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

// spineResidencyWindow is an interval a keep-set member was observed DOWN.
//
// Only observed intervals are reported. The period before the first mirrored
// event for a model is not a window: residency then is unknown, and calling
// unknown "up" would inflate custody while calling it "down" would invent an
// outage.
type spineResidencyWindow struct {
	Model        string `json:"model"`
	From         string `json:"from"`
	To           string `json:"to,omitempty"`
	Open         bool   `json:"open"`
	DurationSecs int64  `json:"duration_secs"`
	// Attribution names the cause of the eviction that opened this window.
	Attribution string `json:"attribution"`
	Detail      string `json:"detail,omitempty"`
	// EmbedRequests / RerankRequests count memory-stack traffic that arrived
	// while the seat was down — the requests that actually paid for the gap.
	EmbedRequests  int `json:"embed_requests_in_window"`
	RerankRequests int `json:"rerank_requests_in_window"`
}

type spineKeepsetMemberAudit struct {
	Model            string                 `json:"model"`
	Aliases          []string               `json:"aliases,omitempty"`
	Origin           string                 `json:"origin"`
	ObservedFrom     string                 `json:"observed_from,omitempty"`
	ObservedTo       string                 `json:"observed_to,omitempty"`
	Evictions        int                    `json:"evictions"`
	DownSecs         int64                  `json:"down_secs_observed"`
	DegradedWindows  []spineResidencyWindow `json:"degraded_windows,omitempty"`
	UnattributedEvic int                    `json:"unattributed_evictions"`
	Note             string                 `json:"note,omitempty"`
}

type spineKeepsetAuditReport struct {
	SchemaVersion int                       `json:"schema_version"`
	Since         string                    `json:"since,omitempty"`
	Sampled       bool                      `json:"sampled"`
	Sources       []string                  `json:"keep_set_sources"`
	Members       []spineKeepsetMemberAudit `json:"members"`
	Coverage      spineAuditCoverage        `json:"coverage"`
	Notes         []string                  `json:"notes"`
	Warnings      []string                  `json:"warnings,omitempty"`
}

// spineAuditCoverage is the honesty header. A ledger with holes is not custody,
// and the holes are named here rather than smoothed over.
type spineAuditCoverage struct {
	SwapEvents        int      `json:"swap_events"`
	ProvenanceRows    int      `json:"unload_provenance_rows"`
	LifecycleRows     int      `json:"service_lifecycle_rows"`
	RequestRows       int      `json:"request_rows"`
	CensoredRows      int      `json:"censored_rows"`
	Epochs            int      `json:"epochs"`
	EpochsUnknownLoss int      `json:"epochs_with_unknown_loss"`
	KnownLostRequests *int64   `json:"known_lost_requests"`
	Holes             []string `json:"holes"`
}

func newNovelKeepsetAuditCmd(flags *rootFlags) *cobra.Command {
	var flagSince string
	var flagDB string
	var flagKeepset []string
	var flagAttrWindow time.Duration

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "See when the memory-stack models were actually resident, every eviction attributed to its cause.",
		Long: strings.Trim(`
Joins mirrored swap events, unload provenance, service lifecycle, and
request rows into a residency timeline per keep-set member: when it was
down, who took it down, and how much memory-stack traffic arrived while
it was gone.

THIS OUTPUT IS SAMPLED, and says so in every rendering. It covers what the
mirror captured. Three named classes of hole are reported rather than
hidden: requests evicted from the ring before a sync, epochs whose id
density broke (loss unknown, never zero), and requests censored by a
restart. Residency before a member's first mirrored event is UNKNOWN, not
"up" — periods with no evidence produce no claim.

An eviction with no matching provenance row and no restart nearby is
labelled unattributed. That is the honest label: something unloaded the
seat and this CLI did not see who.`, "\n"),
		Example: strings.Trim(`
  # Custody over the last week
  llamaswap-pp-cli keepset audit --since 7d

  # Machine-readable, for a nightly report
  llamaswap-pp-cli keepset audit --since 24h --json

  # Widen how far back an eviction may look for its cause
  llamaswap-pp-cli keepset audit --attribution-window 5m
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "keepset audit")
			}
			var since time.Time
			if strings.TrimSpace(flagSince) != "" {
				t, err := parseSinceDuration(flagSince)
				if err != nil {
					return usageErr(fmt.Errorf("invalid --since value %q: %w", flagSince, err))
				}
				since = t
			}
			ctx := cmd.Context()
			db, err := spineOpenDB(ctx, flagDB)
			if err != nil {
				return err
			}
			defer db.Close()

			keep := mirror.LoadKeepSet(mirror.KeepSetOptions{Extra: flagKeepset})
			rep, err := spineBuildKeepsetAudit(ctx, db, keep, since, flagSince, flagAttrWindow)
			if err != nil {
				return err
			}
			return spineWriteKeepsetAudit(cmd, flags, rep)
		},
	}
	cmd.Flags().StringVar(&flagSince, "since", "", "Only audit mirrored history newer than this (e.g. 7d, 24h).")
	cmd.Flags().StringVar(&flagDB, "db", "", "SQLite database file path (default: resolved data directory data.db).")
	cmd.Flags().StringSliceVar(&flagKeepset, "keepset", nil, "Extra keep-set names for this invocation.")
	cmd.Flags().DurationVar(&flagAttrWindow, "attribution-window", 2*time.Minute, "How far back an eviction may look for a provenance row or a restart to blame.")
	return cmd
}

type spineProvenanceRow struct {
	TS      time.Time
	Model   string
	Caller  string
	Drained *int64
	Forced  bool
	Result  string
}

type spineLifecycleRow struct {
	TS      time.Time
	Event   string
	Details string
}

func spineBuildKeepsetAudit(ctx context.Context, db *store.Store, keep *mirror.KeepSet, since time.Time, sinceText string, attrWindow time.Duration) (*spineKeepsetAuditReport, error) {
	if attrWindow <= 0 {
		attrWindow = 2 * time.Minute
	}
	rep := &spineKeepsetAuditReport{
		SchemaVersion: spineSchemaVersion,
		Since:         sinceText,
		Sampled:       true,
		Sources:       keep.Sources,
		Warnings:      keep.Warnings,
	}
	if keep.Empty() {
		rep.Warnings = append(rep.Warnings,
			"keep-set is EMPTY: nothing to audit. Point "+mirror.EnvYAMLPath+" at the llama-swap config or add keep_set to the CLI config.")
	}

	events, err := spineLoadSwapEvents(ctx, db, since)
	if err != nil {
		return nil, err
	}
	prov, err := spineLoadProvenance(ctx, db, since)
	if err != nil {
		return nil, err
	}
	life, err := spineLoadLifecycle(ctx, db, since)
	if err != nil {
		return nil, err
	}
	rep.Coverage.SwapEvents = len(events)
	rep.Coverage.ProvenanceRows = len(prov)
	rep.Coverage.LifecycleRows = len(life)

	byModel := map[string][]spineSwapEvent{}
	for _, e := range events {
		byModel[e.Model] = append(byModel[e.Model], e)
	}

	now := time.Now().UTC()
	for _, m := range keep.Members {
		audit := spineKeepsetMemberAudit{Model: m.ID, Aliases: m.Aliases, Origin: m.Origin}
		evs := byModel[m.ID]
		if len(evs) == 0 {
			audit.Note = "no mirrored swap events for this seat: residency history is UNKNOWN, not clean. Run 'sync' regularly to build it."
			rep.Members = append(rep.Members, audit)
			continue
		}
		audit.ObservedFrom = evs[0].TS.UTC().Format(time.RFC3339)
		audit.ObservedTo = evs[len(evs)-1].TS.UTC().Format(time.RFC3339)

		var open *spineResidencyWindow
		for _, e := range evs {
			switch e.Event {
			case "unloading", "unloaded", "failed":
				if open != nil {
					continue
				}
				attribution, detail := spineAttributeEviction(e, prov, life, attrWindow)
				open = &spineResidencyWindow{
					Model: m.ID, From: e.TS.UTC().Format(time.RFC3339), Open: true,
					Attribution: attribution, Detail: detail,
				}
				audit.Evictions++
				if attribution == "unattributed" {
					audit.UnattributedEvic++
				}
			case "ready":
				if open == nil {
					continue
				}
				open.To = e.TS.UTC().Format(time.RFC3339)
				open.Open = false
				from, _ := time.Parse(time.RFC3339, open.From)
				open.DurationSecs = int64(e.TS.Sub(from).Seconds())
				audit.DegradedWindows = append(audit.DegradedWindows, *open)
				open = nil
			}
		}
		if open != nil {
			from, _ := time.Parse(time.RFC3339, open.From)
			open.DurationSecs = int64(now.Sub(from).Seconds())
			audit.DegradedWindows = append(audit.DegradedWindows, *open)
		}

		for i := range audit.DegradedWindows {
			win := &audit.DegradedWindows[i]
			embeds, reranks, err := spineTrafficInWindow(ctx, db, win.From, win.To)
			if err != nil {
				return nil, err
			}
			win.EmbedRequests = embeds
			win.RerankRequests = reranks
			audit.DownSecs += win.DurationSecs
		}
		rep.Members = append(rep.Members, audit)
	}

	if err := spineFillAuditCoverage(ctx, db, rep); err != nil {
		return nil, err
	}
	rep.Notes = append(rep.Notes,
		"SAMPLED: this is a ledger of what the mirror captured, not a complete custody record.",
		"residency before a seat's first mirrored event is UNKNOWN and is not counted as uptime.",
		"an eviction with no provenance row and no nearby restart is labelled unattributed rather than guessed.",
	)
	return rep, nil
}

// spineAttributeEviction blames an eviction on the closest preceding evidence:
// an unload_provenance row written by this CLI, or a detected service restart.
func spineAttributeEviction(e spineSwapEvent, prov []spineProvenanceRow, life []spineLifecycleRow, window time.Duration) (string, string) {
	best := time.Duration(1<<62 - 1)
	attribution, detail := "", ""
	for _, p := range prov {
		if p.Model != e.Model {
			continue
		}
		d := absDuration(e.TS.Sub(p.TS))
		if d <= window && d < best {
			best = d
			attribution = "cli:" + p.Caller
			forced := ""
			if p.Forced {
				forced = " (--force-keepset)"
			}
			detail = fmt.Sprintf("unload_provenance row at %s, result=%s%s", p.TS.UTC().Format(time.RFC3339), p.Result, forced)
		}
	}
	for _, l := range life {
		d := absDuration(e.TS.Sub(l.TS))
		if d <= window && d < best {
			best = d
			attribution = "service:" + l.Event
			detail = l.Details
		}
	}
	if attribution == "" {
		return "unattributed", "no unload_provenance row and no service lifecycle event within the attribution window; something outside this CLI evicted the seat"
	}
	return attribution, detail
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func spineLoadProvenance(ctx context.Context, db *store.Store, since time.Time) ([]spineProvenanceRow, error) {
	query := `SELECT ts, model, caller, drained, forced, COALESCE(result,'') FROM unload_provenance`
	args := []any{}
	if !since.IsZero() {
		query += ` WHERE ts >= ?`
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	query += ` ORDER BY ts`
	rows, err := db.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read unload_provenance: %w", err)
	}
	defer rows.Close()
	var out []spineProvenanceRow
	for rows.Next() {
		var ts, model, caller, result string
		var drained *int64
		var forced int
		if err := rows.Scan(&ts, &model, &caller, &drained, &forced, &result); err != nil {
			return nil, err
		}
		t, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			continue
		}
		out = append(out, spineProvenanceRow{TS: t, Model: model, Caller: caller, Drained: drained, Forced: forced != 0, Result: result})
	}
	return out, rows.Err()
}

func spineLoadLifecycle(ctx context.Context, db *store.Store, since time.Time) ([]spineLifecycleRow, error) {
	query := `SELECT ts, event, COALESCE(details,'') FROM service_lifecycle`
	args := []any{}
	if !since.IsZero() {
		query += ` WHERE ts >= ?`
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	query += ` ORDER BY ts`
	rows, err := db.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read service_lifecycle: %w", err)
	}
	defer rows.Close()
	var out []spineLifecycleRow
	for rows.Next() {
		var ts, event, details string
		if err := rows.Scan(&ts, &event, &details); err != nil {
			return nil, err
		}
		t, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			continue
		}
		out = append(out, spineLifecycleRow{TS: t, Event: event, Details: details})
	}
	return out, rows.Err()
}

// spineTrafficInWindow counts memory-stack requests that landed while a seat was
// down. Counted from MIRRORED rows only, so it is a lower bound.
func spineTrafficInWindow(ctx context.Context, db *store.Store, from, to string) (int, int, error) {
	upper := to
	if upper == "" {
		upper = time.Now().UTC().Format(time.RFC3339)
	}
	var embeds, reranks int
	err := db.DB().QueryRowContext(ctx, `
		SELECT
			SUM(CASE WHEN req_path LIKE '%embedding%' THEN 1 ELSE 0 END),
			SUM(CASE WHEN req_path LIKE '%rerank%' THEN 1 ELSE 0 END)
		  FROM requests WHERE ts >= ? AND ts <= ?`, from, upper).Scan(&embeds, &reranks)
	if err != nil {
		// SUM over an empty set yields NULL; treat that as zero observed
		// traffic rather than an error.
		return 0, 0, nil
	}
	return embeds, reranks, nil
}

func spineFillAuditCoverage(ctx context.Context, db *store.Store, rep *spineKeepsetAuditReport) error {
	c := &rep.Coverage
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&c.RequestRows); err != nil {
		return err
	}
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE censored=1`).Scan(&c.CensoredRows); err != nil {
		return err
	}
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM epochs`).Scan(&c.Epochs); err != nil {
		return err
	}
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM epochs WHERE loss_evicted IS NULL OR ids_dense=0`).Scan(&c.EpochsUnknownLoss); err != nil {
		return err
	}
	var known *int64
	if err := db.DB().QueryRowContext(ctx,
		`SELECT SUM(COALESCE(loss_evicted,0) + COALESCE(loss_prepoll,0)) FROM epochs WHERE loss_evicted IS NOT NULL`).Scan(&known); err == nil {
		c.KnownLostRequests = known
	}
	if c.EpochsUnknownLoss > 0 {
		c.Holes = append(c.Holes, fmt.Sprintf("%d epoch(s) have UNKNOWN eviction loss (id density violated); known_lost_requests excludes them entirely", c.EpochsUnknownLoss))
	}
	if c.CensoredRows > 0 {
		c.Holes = append(c.Holes, fmt.Sprintf("%d request(s) censored by a restart: in flight, outcome unobservable, biased toward long requests", c.CensoredRows))
	}
	c.Holes = append(c.Holes, "requests served after the last poll and before a restart are permanently unknowable and are counted nowhere")
	if c.Epochs > 1 {
		c.Holes = append(c.Holes, fmt.Sprintf("%d epochs known, which is a LOWER bound on restarts in the window", c.Epochs))
	}
	return nil
}

func spineWriteKeepsetAudit(cmd *cobra.Command, flags *rootFlags, rep *spineKeepsetAuditReport) error {
	if flags != nil && (flags.asJSON || !isTerminal(cmd.OutOrStdout())) {
		if err := printJSONFiltered(cmd.OutOrStdout(), rep, flags); err != nil {
			return err
		}
		for _, warn := range rep.Warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
		}
		return nil
	}
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "keepset audit — SAMPLED (a ledger with holes is not custody; holes are listed below)")
	if len(rep.Members) == 0 {
		fmt.Fprintln(w, "no keep-set members to audit")
	} else {
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "MODEL\tEVICTIONS\tUNATTRIBUTED\tDOWN-SECS(OBSERVED)\tWINDOWS\tNOTE")
		sorted := append([]spineKeepsetMemberAudit(nil), rep.Members...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Model < sorted[j].Model })
		for _, m := range sorted {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\n",
				m.Model, m.Evictions, m.UnattributedEvic, m.DownSecs, len(m.DegradedWindows), m.Note)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, m := range rep.Members {
			if len(m.DegradedWindows) == 0 {
				continue
			}
			fmt.Fprintf(w, "\n%s degraded windows:\n", m.Model)
			wt := newTabWriter(w)
			fmt.Fprintln(wt, "  FROM\tTO\tSECS\tATTRIBUTION\tEMBED-REQS\tRERANK-REQS")
			for _, win := range m.DegradedWindows {
				to := win.To
				if win.Open {
					to = "(still down)"
				}
				fmt.Fprintf(wt, "  %s\t%s\t%d\t%s\t%d\t%d\n",
					win.From, to, win.DurationSecs, win.Attribution, win.EmbedRequests, win.RerankRequests)
			}
			if err := wt.Flush(); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(w, "\ncoverage: %d swap events, %d provenance rows, %d lifecycle rows, %d request rows, %d epoch(s)\n",
		rep.Coverage.SwapEvents, rep.Coverage.ProvenanceRows, rep.Coverage.LifecycleRows,
		rep.Coverage.RequestRows, rep.Coverage.Epochs)
	for _, h := range rep.Coverage.Holes {
		fmt.Fprintf(w, "  hole: %s\n", h)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
	}
	return nil
}
