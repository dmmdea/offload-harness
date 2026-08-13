// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave A spine): the generated mirror for POST /api/models/unload
// called the non-selective bulk route directly, which on this box takes the mem0
// memory stack down. This version unloads per model and excludes the keep-set.
// pp:data-source live

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/mirror"
)

func newModelsUnloadAllCmd(flags *rootFlags) *cobra.Command {
	var flagDrain bool
	var flagDrainTimeout time.Duration
	var flagForceKeepset bool
	var flagKeepset []string
	var flagDB string

	cmd := &cobra.Command{
		Use:   "unload-all",
		Short: "Unload every running model EXCEPT the keep-set.",
		Long: strings.Trim(`
Frees VRAM without touching the protected seats. Implemented as one
per-model unload per running model rather than the bulk route, because
POST /api/models/unload and the legacy GET /unload are NOT selective:
they take the keep-set down with everything else. On this box that means
the mem0 memory stack.

Fallback behavior when the per-model route is missing (older builds):
the bulk route is used ONLY if no keep-set member is currently resident,
or if --force-keepset was passed. Otherwise the command refuses and says
why, rather than quietly widening the blast radius.

Exit codes: 20 keep-set refusal, 21 drain timeout, 22 drain unobservable,
4 server unreachable.`, "\n"),
		Example: strings.Trim(`
  # Free VRAM, keep embeddinggemma and bge-reranker-v2-m3 resident
  llamaswap-pp-cli models unload-all

  # Wait for in-flight generation on each target first
  llamaswap-pp-cli models unload-all --drain --drain-timeout 45s --json
`, "\n"),
		Annotations: map[string]string{
			"pp:endpoint":          "models.unload_all",
			"pp:method":            "POST",
			"pp:path":              "/api/models/unload",
			"pp:typed-exit-codes":  "20,21,22",
			"mcp:destructive-hint": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "models unload-all")
			}
			return spineRunUnloadAll(cmd, flags, spineUnloadOptions{
				Drain:        flagDrain,
				DrainTimeout: flagDrainTimeout,
				Force:        flagForceKeepset,
				ExtraKeepSet: flagKeepset,
				DBPath:       flagDB,
			})
		},
	}
	cmd.Flags().BoolVar(&flagDrain, "drain", false, "Wait for in-flight requests on each target before unloading it. Fails closed when slot state is unreadable.")
	cmd.Flags().DurationVar(&flagDrainTimeout, "drain-timeout", 30*time.Second, "Per-target drain budget before giving up (exit 21, nothing unloaded).")
	cmd.Flags().BoolVar(&flagForceKeepset, "force-keepset", false, "Include keep-set members. Loud, recorded, and capable of taking the memory stack down.")
	cmd.Flags().StringSliceVar(&flagKeepset, "keepset", nil, "Extra keep-set names for this invocation.")
	cmd.Flags().StringVar(&flagDB, "db", "", "SQLite database file path for the provenance ledger.")

	return cmd
}

func spineRunUnloadAll(cmd *cobra.Command, flags *rootFlags, opts spineUnloadOptions) error {
	ctx := cmd.Context()
	base, _ := spineBaseURL(flags)
	keep := mirror.LoadKeepSet(mirror.KeepSetOptions{Extra: opts.ExtraKeepSet})
	rep := &spineUnloadReport{
		SchemaVersion: spineSchemaVersion,
		Action:        "unload-all",
		BaseURL:       base,
		KeepSet:       keep.Names(),
		KeepSetSource: keep.Sources,
		Warnings:      keep.Warnings,
	}
	if cliutil.IsVerifyEnv() {
		rep.Results = append(rep.Results, spineUnloadResult{Requested: "*", Action: "would_unload",
			Notes: []string{"PRINTING_PRESS_VERIFY=1: no request sent"}})
		return spineWriteUnloadReport(cmd, flags, rep, nil)
	}

	c, err := spineClient(flags)
	if err != nil {
		return err
	}
	roster, err := spineLoadRoster(ctx, c)
	if err != nil {
		return err
	}
	running, err := c.Running(ctx)
	if err != nil {
		return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /running: %w", err))
	}

	var targets []mirror.RosterEntry
	var protectedResident []string
	for _, r := range running {
		entry, ok := roster.Resolve(r.Model)
		if !ok {
			entry = mirror.RosterEntry{ID: r.Model}
		}
		if m, isKeep := keep.MatchAny(spineNamesOf(entry)...); isKeep {
			protectedResident = append(protectedResident, m.ID)
			if !opts.Force {
				rep.Results = append(rep.Results, spineUnloadResult{
					Requested: entry.ID, Model: entry.ID, Aliases: entry.Aliases,
					Action: "skipped_keepset",
					Notes:  []string{"keep-set member (" + m.Origin + "); left resident"},
				})
				continue
			}
		}
		targets = append(targets, entry)
	}

	if len(targets) == 0 {
		return spineWriteUnloadReport(cmd, flags, rep, nil)
	}

	// Drain every target BEFORE unloading any of them. Draining and unloading
	// one at a time would leave a partial unload behind when a later target
	// turns out to be unobservable — the fail-closed guarantee has to cover the
	// whole batch, not each element.
	drainedFlags := map[string]*bool{}
	if opts.Drain {
		for _, t := range targets {
			outcome, derr := spineDrain(ctx, c, t.ID, true, opts.DrainTimeout)
			if derr != nil {
				rep.Results = append(rep.Results, spineUnloadResult{
					Requested: t.ID, Model: t.ID, Action: "refused_drain",
					DrainMethod: outcome.Method, Drained: boolPtr(false),
					Notes: append(outcome.Notes, derr.Error(), "no model in this batch was unloaded"),
				})
				return spineWriteUnloadReport(cmd, flags, rep, derr)
			}
			drainedFlags[t.ID] = boolPtr(true)
		}
	}

	db, dbWarn := spineOpenProvenance(ctx, opts.DBPath)
	if dbWarn != "" {
		rep.Warnings = append(rep.Warnings, dbWarn)
	}
	if db != nil {
		defer db.Close()
	}

	perModelRouteMissing := false
	for _, t := range targets {
		status, uerr := c.UnloadModel(ctx, t.ID)
		res := spineUnloadResult{
			Requested: t.ID, Model: t.ID, Aliases: t.Aliases,
			Route: "POST /api/models/unload/" + t.ID, Status: status,
			Drained: drainedFlags[t.ID], Forced: opts.Force,
		}
		switch {
		case uerr != nil:
			res.Action = "error"
			res.Notes = append(res.Notes, uerr.Error())
			spineRecordProvenance(ctx, db, t.ID, res.Drained, opts.Force, "error: "+uerr.Error())
			rep.Results = append(rep.Results, res)
			return spineWriteUnloadReport(cmd, flags, rep, spineExitErr(ExitServerUnreachable, uerr))
		case status == 404:
			perModelRouteMissing = true
		case status >= 200 && status < 300:
			res.Action = "unloaded"
			spineRecordProvenance(ctx, db, t.ID, res.Drained, opts.Force, "unloaded")
			rep.Results = append(rep.Results, res)
			continue
		default:
			res.Action = "error"
			spineRecordProvenance(ctx, db, t.ID, res.Drained, opts.Force, fmt.Sprintf("http_%d", status))
			rep.Results = append(rep.Results, res)
			return spineWriteUnloadReport(cmd, flags, rep, spineExitErr(ExitUpstream5xx,
				fmt.Errorf("unload %s: HTTP %d", t.ID, status)))
		}
		if perModelRouteMissing {
			break
		}
	}

	if !perModelRouteMissing {
		return spineWriteUnloadReport(cmd, flags, rep, nil)
	}

	// Version-drift fallback. Both bulk routes unload EVERYTHING, so the
	// keep-set can only be honored by refusing to use them while a protected
	// model is resident.
	if len(protectedResident) > 0 && !opts.Force {
		rep.Refused = true
		rep.Results = append(rep.Results, spineUnloadResult{
			Requested: "*", Action: "refused_keepset",
			Notes: []string{
				"per-model unload route returned 404 on this build",
				"the remaining routes (POST /api/models/unload, legacy GET /unload) are NOT selective and would also unload: " + strings.Join(protectedResident, ", "),
				"nothing was unloaded. Re-run with --force-keepset only if taking those seats down is acceptable.",
			},
		})
		return spineWriteUnloadReport(cmd, flags, rep,
			spineExitErr(ExitKeepsetRefusal, fmt.Errorf(
				"bulk unload would hit keep-set member(s) %s and this build has no per-model route", strings.Join(protectedResident, ", "))))
	}

	status, uerr := c.UnloadAll(ctx)
	route := "POST /api/models/unload"
	if uerr == nil && status == 404 {
		status, uerr = c.LegacyUnloadAll(ctx)
		route = "GET /unload (legacy fallback)"
	}
	res := spineUnloadResult{Requested: "*", Action: "unloaded_bulk", Route: route, Status: status, Forced: opts.Force,
		Notes: []string{"per-model route absent on this build; used the non-selective bulk route"}}
	switch {
	case uerr != nil:
		res.Action = "error"
		res.Notes = append(res.Notes, uerr.Error())
		rep.Results = append(rep.Results, res)
		return spineWriteUnloadReport(cmd, flags, rep, spineExitErr(ExitServerUnreachable, uerr))
	case status < 200 || status >= 300:
		res.Action = "error"
		rep.Results = append(rep.Results, res)
		return spineWriteUnloadReport(cmd, flags, rep, spineExitErr(ExitUpstream5xx,
			fmt.Errorf("bulk unload: HTTP %d", status)))
	}
	for _, t := range targets {
		spineRecordProvenance(ctx, db, t.ID, drainedFlags[t.ID], opts.Force, "unloaded_bulk")
	}
	rep.Results = append(rep.Results, res)
	return spineWriteUnloadReport(cmd, flags, rep, nil)
}
