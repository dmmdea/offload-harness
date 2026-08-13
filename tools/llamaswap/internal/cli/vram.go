// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source live

package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/measure"
	"llamaswap-pp-cli/internal/store"
)

// vramCard is one card's reading. baseline/after/delta are always all three
// present, and delta_mib is null (not zero) when no baseline exists: a total
// reported as a usage is the exact misread that once fabricated a
// 15,924-vs-6,150 MiB constraint out of thin air.
type vramCard struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"`
	TotalMiB    int    `json:"total_mib"`
	AfterMiB    int    `json:"after_mib"`
	BaselineMiB *int   `json:"baseline_mib"`
	DeltaMiB    *int   `json:"delta_mib"`
	BaselineAt  string `json:"baseline_at,omitempty"`
	Note        string `json:"note,omitempty"`
}

type vramReport struct {
	SchemaVersion int        `json:"schema_version"`
	TakenAt       string     `json:"taken_at"`
	Mode          string     `json:"mode"`
	Cards         []vramCard `json:"cards"`
	RoleSource    string     `json:"role_source"`
	Warnings      []string   `json:"warnings,omitempty"`
}

func newMeasureVramCmd(flags *rootFlags) *cobra.Command {
	var flagBaseline bool

	cmd := &cobra.Command{
		Use:   "vram",
		Short: "Per-GPU-UUID VRAM snapshot with explicit baseline/after/delta",
		Long: `Reads memory.used and memory.total per GPU UUID (never per index - indices are
unstable across reboots and inverted relative to torch on this box) and records
the snapshot in the local store.

Deltas are always explicit. A plain snapshot compares against the most recent
recorded baseline; with no baseline on file the delta is reported as null and
the reading is labeled a TOTAL, never a model's usage.

  vram --baseline   record the current reading as the baseline to compare against
  vram              read the cards and difference them against that baseline`,
		Example: `  llamaswap-pp-cli vram --baseline
  llamaswap-pp-cli vram --json`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:typed-exit-codes":  "1=nvidia-smi unavailable",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "vram snapshot")
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()

			extras := mcLoadExtras(flags)
			gpus, err := measure.PerUUID(ctx, extras.GPURoles)
			if err != nil {
				return err
			}

			report := &vramReport{
				SchemaVersion: 1,
				TakenAt:       mcNow(),
				Mode:          "snapshot",
				RoleSource:    "cli config gpu_roles",
			}
			if flagBaseline {
				report.Mode = "baseline"
			}
			if len(extras.GPURoles) == 0 {
				report.RoleSource = "none configured - falling back to card name + short UUID"
				report.Warnings = append(report.Warnings,
					`no gpu_roles in the CLI config; add {"gpu_roles":{"GPU-<uuid>":"fast-card"}} to label cards by operator role`)
			}

			baselines := map[string]vramBaseline{}
			if !flagBaseline {
				baselines = vramLoadBaselines(ctx)
			}

			for _, g := range gpus {
				card := vramCard{
					UUID: g.UUID, Name: g.Name, Role: g.Label(),
					TotalMiB: g.TotalMiB, AfterMiB: g.UsedMiB,
				}
				if flagBaseline {
					used := g.UsedMiB
					card.BaselineMiB = &used
					zero := 0
					card.DeltaMiB = &zero
					card.Note = "recorded as the baseline for later deltas"
				} else if b, ok := baselines[g.UUID]; ok {
					base := b.UsedMiB
					d := g.UsedMiB - base
					card.BaselineMiB = &base
					card.DeltaMiB = &d
					card.BaselineAt = b.TakenAt
				} else {
					card.Note = "TOTAL card usage, not a model's usage: no baseline recorded yet (run `vram --baseline` first)"
				}
				report.Cards = append(report.Cards, card)
			}

			mcRecord(ctx, "vram snapshot", func(s *store.Store) error {
				return vramInsert(ctx, s, report)
			})

			return mcEmit(cmd, flags, report, func(w io.Writer) { vramPrint(w, report) })
		},
	}
	cmd.Flags().BoolVar(&flagBaseline, "baseline", false, "Record this reading as the baseline future snapshots difference against")
	return cmd
}

type vramBaseline struct {
	UsedMiB int
	TakenAt string
}

// vramBaselineMarker tags rows written by --baseline in the model column.
// An ordinary snapshot whose delta happens to be 0 must never be mistaken
// for a baseline, so the marker is explicit rather than inferred from the
// numbers.
const vramBaselineMarker = "__baseline__"

// vramLoadBaselines returns the most recent baseline row per UUID.
func vramLoadBaselines(ctx context.Context) map[string]vramBaseline {
	out := map[string]vramBaseline{}
	s, err := mcOpenDomainStore(ctx)
	if err != nil {
		return out
	}
	defer s.Close()
	rows, err := s.DB().QueryContext(ctx, `
		SELECT gpu_uuid, after_mib, ts FROM vram_snapshots
		WHERE id IN (
			SELECT MAX(id) FROM vram_snapshots
			WHERE model = ?
			GROUP BY gpu_uuid
		)`, vramBaselineMarker)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var uuid, ts string
		var used sql.NullInt64
		if err := rows.Scan(&uuid, &used, &ts); err != nil {
			return out
		}
		out[uuid] = vramBaseline{UsedMiB: int(used.Int64), TakenAt: ts}
	}
	return out
}

func vramInsert(ctx context.Context, s *store.Store, r *vramReport) error {
	var marker any
	if r.Mode == "baseline" {
		marker = vramBaselineMarker
	}
	for _, c := range r.Cards {
		var baseline, delta any
		if c.BaselineMiB != nil {
			baseline = *c.BaselineMiB
		}
		if c.DeltaMiB != nil {
			delta = *c.DeltaMiB
		}
		if _, err := s.DB().ExecContext(ctx,
			`INSERT INTO vram_snapshots (ts, gpu_uuid, gpu_role, model, ctx, baseline_mib, after_mib, delta_mib)
			 VALUES (?, ?, ?, ?, NULL, ?, ?, ?)`,
			r.TakenAt, c.UUID, c.Role, marker, baseline, c.AfterMiB, delta); err != nil {
			return err
		}
	}
	return nil
}

// vramRecordDeltas persists per-UUID deltas measured around an action
// (bench, gate, scratch). Model and ctx are attributed so a later join
// answers "what did THIS seat cost at THIS context".
func vramRecordDeltas(ctx context.Context, model string, ctxTokens int, ds []measure.Delta) {
	if len(ds) == 0 {
		return
	}
	ts := mcNow()
	mcRecord(ctx, "vram delta", func(s *store.Store) error {
		for _, d := range ds {
			var ctxVal any
			if ctxTokens > 0 {
				ctxVal = ctxTokens
			}
			role := d.Role
			if role == "" {
				role = measure.GPU{UUID: d.UUID, Name: d.Name}.Label()
			}
			if _, err := s.DB().ExecContext(ctx,
				`INSERT INTO vram_snapshots (ts, gpu_uuid, gpu_role, model, ctx, baseline_mib, after_mib, delta_mib)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				ts, d.UUID, role, model, ctxVal, d.BaselineMiB, d.AfterMiB, d.DeltaMiB); err != nil {
				return err
			}
		}
		return nil
	})
}

func vramPrint(w io.Writer, r *vramReport) {
	fmt.Fprintf(w, "%s  (%s)\n", bold("VRAM by GPU UUID"), r.TakenAt)
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "ROLE\tUUID\tCARD\tBASELINE\tAFTER\tDELTA\tTOTAL")
	for _, c := range r.Cards {
		baseline, delta := "-", "n/a"
		if c.BaselineMiB != nil {
			baseline = fmt.Sprintf("%d MiB", *c.BaselineMiB)
		}
		if c.DeltaMiB != nil {
			delta = fmt.Sprintf("%+d MiB", *c.DeltaMiB)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d MiB\t%s\t%d MiB\n",
			c.Role, measure.ShortUUID(c.UUID), c.Name, baseline, c.AfterMiB, delta, c.TotalMiB)
	}
	tw.Flush()
	for _, c := range r.Cards {
		if c.Note != "" {
			fmt.Fprintf(w, "  %s: %s\n", measure.ShortUUID(c.UUID), c.Note)
		}
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "  %s %s\n", yellow("warning:"), warn)
	}
}
