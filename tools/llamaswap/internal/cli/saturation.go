// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command (wave D): per-seat error-and-load pressure from the local mirror.
// pp:data-source local
//
// What this deliberately does NOT report: in-flight concurrency / queue depth.
// llama-swap stamps activity in whole seconds, so an overlap sweep of
// (ts, duration_ms) on a sub-second workload manufactures depth out of timestamp
// collisions — measured: a slots=1 seat "showed" depth 6. That number would be a
// clock artifact, not capacity, so it is not printed. What IS ground truth at
// second resolution — rejection counts, error rates, request volume, and when
// the load actually lands — is what this verb reports.

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/store"
)

type saturationSeatStat struct {
	Model       string  `json:"model"`
	Requests    int     `json:"requests"`
	Errors      int     `json:"errors"`
	Rejections  int     `json:"rejections_429"`
	ServerErr   int     `json:"server_errors_5xx"`
	ErrorRate   float64 `json:"error_rate"`
	RejectRate  float64 `json:"reject_rate_429"`
	P95DurMS    int64   `json:"p95_duration_ms"`
	MaxDurMS    int64   `json:"max_duration_ms"`
	BusiestHour string  `json:"busiest_hour_utc,omitempty"`
	BusiestN    int     `json:"busiest_hour_requests"`
}

type saturationReport struct {
	SchemaVersion int                   `json:"schema_version"`
	Since         string                `json:"since,omitempty"`
	Seats         []saturationSeatStat  `json:"seats"`
	HourlyLoad    []saturationHourBucket `json:"hourly_load"`
	Coverage      analyticsCoverage     `json:"coverage"`
	Notes         []string              `json:"notes"`
}

type saturationHourBucket struct {
	HourUTC  string `json:"hour_utc"`
	Requests int    `json:"requests"`
	Errors   int    `json:"errors"`
}

func newNovelSaturationCmd(flags *rootFlags) *cobra.Command {
	var flagSince string
	var flagDB string

	cmd := &cobra.Command{
		Use:   "saturation",
		Short: "Per-seat error and load pressure: rejections, error rates, and when the load actually lands.",
		Long: strings.Trim(`
Reads the local mirror only. Answers "which seats are under pressure, how
often are they rejecting or erroring, and when does the load arrive" — the
question behind a 429 storm or a "is this seat overloaded" hunch.

It does NOT report in-flight concurrency. llama-swap records activity at
whole-second resolution, so reconstructing simultaneous request depth from
(timestamp, duration) invents overlaps from same-second collisions — a
measured slots=1 seat "showed" depth 6. Rejection counts, error rates,
request volume, and the hourly load curve are all real at second resolution;
concurrency depth is not, so it is omitted rather than faked.

This command PRINTS. It never edits groups, ttl, or model placement.`, "\n"),
		Example: strings.Trim(`
  # Pressure per seat over everything mirrored
  llamaswap-pp-cli saturation

  # Last 24h, machine-readable
  llamaswap-pp-cli saturation --since 24h --json
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "saturation")
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

			rep, err := buildSaturationReport(ctx, db, since, flagSince)
			if err != nil {
				return err
			}
			return writeSaturationReport(cmd, flags, rep)
		},
	}
	cmd.Flags().StringVar(&flagSince, "since", "", "Only consider mirrored requests newer than this (e.g. 7d, 24h, 90m).")
	cmd.Flags().StringVar(&flagDB, "db", "", "SQLite database file path (default: resolved data directory data.db).")
	return cmd
}

func buildSaturationReport(ctx context.Context, db *store.Store, since time.Time, sinceText string) (*saturationReport, error) {
	reqs, err := loadMirrorRequests(ctx, db, since, "")
	if err != nil {
		return nil, err
	}
	rep := &saturationReport{SchemaVersion: analyticsSchemaVersion, Since: sinceText}

	type acc struct {
		n, err429, err5xx, errOther int
		durs                        []int64
		byHour                      map[string]int
	}
	seats := map[string]*acc{}
	hourly := map[string]*saturationHourBucket{}
	for _, r := range reqs {
		a := seats[r.Model]
		if a == nil {
			a = &acc{byHour: map[string]int{}}
			seats[r.Model] = a
		}
		a.n++
		a.durs = append(a.durs, r.Duration.Milliseconds())
		hour := r.TS.UTC().Format("2006-01-02T15")
		a.byHour[hour]++
		isErr := r.Status == 429 || r.Status >= 500
		if r.Status == 429 {
			a.err429++
		} else if r.Status >= 500 {
			a.err5xx++
		} else if r.Status >= 400 {
			a.errOther++
			isErr = true
		}
		hb := hourly[hour]
		if hb == nil {
			hb = &saturationHourBucket{HourUTC: hour}
			hourly[hour] = hb
		}
		hb.Requests++
		if isErr {
			hb.Errors++
		}
	}

	names := make([]string, 0, len(seats))
	for m := range seats {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		a := seats[m]
		sort.Slice(a.durs, func(i, j int) bool { return a.durs[i] < a.durs[j] })
		totalErr := a.err429 + a.err5xx + a.errOther
		st := saturationSeatStat{
			Model:      m,
			Requests:   a.n,
			Errors:     totalErr,
			Rejections: a.err429,
			ServerErr:  a.err5xx,
		}
		if a.n > 0 {
			st.ErrorRate = round2(float64(totalErr) / float64(a.n))
			st.RejectRate = round2(float64(a.err429) / float64(a.n))
		}
		if len(a.durs) > 0 {
			st.P95DurMS = analyticsPercentile(a.durs, 0.95)
			st.MaxDurMS = a.durs[len(a.durs)-1]
		}
		// Busiest hour for this seat.
		var bh string
		var bn int
		hoursSorted := make([]string, 0, len(a.byHour))
		for h := range a.byHour {
			hoursSorted = append(hoursSorted, h)
		}
		sort.Strings(hoursSorted)
		for _, h := range hoursSorted {
			if a.byHour[h] > bn {
				bn = a.byHour[h]
				bh = h
			}
		}
		st.BusiestHour = bh
		st.BusiestN = bn
		rep.Seats = append(rep.Seats, st)
	}
	// Sort seats by request volume desc, then name — the busy seats first.
	sort.SliceStable(rep.Seats, func(i, j int) bool {
		if rep.Seats[i].Requests != rep.Seats[j].Requests {
			return rep.Seats[i].Requests > rep.Seats[j].Requests
		}
		return rep.Seats[i].Model < rep.Seats[j].Model
	})

	hkeys := make([]string, 0, len(hourly))
	for h := range hourly {
		hkeys = append(hkeys, h)
	}
	sort.Strings(hkeys)
	for _, h := range hkeys {
		rep.HourlyLoad = append(rep.HourlyLoad, *hourly[h])
	}

	cov, err := fillAnalyticsCoverage(ctx, db, reqs)
	if err != nil {
		return nil, err
	}
	rep.Coverage = cov
	rep.Notes = append(rep.Notes,
		"rejections_429 and server_errors_5xx are exact from status codes; error_rate = errors/requests.",
		"in-flight concurrency is intentionally omitted (unreconstructable at second-resolution timestamps).",
		"prints only: never edits groups, ttl, or model placement.")
	return rep, nil
}

func writeSaturationReport(cmd *cobra.Command, flags *rootFlags, rep *saturationReport) error {
	w := cmd.OutOrStdout()
	if flags != nil && (flags.asJSON || !isTerminal(w)) {
		return printJSONFiltered(w, rep, flags)
	}
	if len(rep.Seats) == 0 {
		fmt.Fprintln(w, "no requests mirrored yet — run 'llamaswap-pp-cli sync' first")
	} else {
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "MODEL\tREQUESTS\t429\t5XX\tERR-RATE\tP95-MS\tMAX-MS\tBUSIEST-HOUR")
		for _, s := range rep.Seats {
			bh := s.BusiestHour
			if bh == "" {
				bh = "-"
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%.3f\t%d\t%d\t%s\n",
				s.Model, s.Requests, s.Rejections, s.ServerErr, s.ErrorRate, s.P95DurMS, s.MaxDurMS, bh)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "coverage: %d request rows, %d epoch(s), %d prepoll-loss (SAMPLED, second-resolution)\n",
		rep.Coverage.RequestRows, rep.Coverage.Epochs, rep.Coverage.PrepollLoss)
	for _, c := range rep.Coverage.Caveats {
		fmt.Fprintf(w, "  caveat: %s\n", c)
	}
	return nil
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
