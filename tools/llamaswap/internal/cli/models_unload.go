// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave A spine): the generated endpoint mirror for
// POST /api/models/unload/{model} shipped an unguarded fire-and-forget call.
// This replaces it with alias resolution, keep-set refusal, and a drain check
// that fails CLOSED. See .printing-press-patches/ for the reprint guard.
// pp:data-source live

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

// spineUnloadResult is one target's outcome.
type spineUnloadResult struct {
	Requested   string   `json:"requested"`
	Model       string   `json:"model,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	Action      string   `json:"action"`
	Route       string   `json:"route,omitempty"`
	Status      int      `json:"status,omitempty"`
	Drained     *bool    `json:"drained"`
	DrainMethod string   `json:"drain_method,omitempty"`
	Forced      bool     `json:"forced"`
	Notes       []string `json:"notes,omitempty"`
}

// spineUnloadReport is the command envelope.
type spineUnloadReport struct {
	SchemaVersion int                 `json:"schema_version"`
	Action        string              `json:"action"`
	BaseURL       string              `json:"base_url"`
	KeepSet       []string            `json:"keep_set"`
	KeepSetSource []string            `json:"keep_set_sources"`
	Results       []spineUnloadResult `json:"results"`
	Refused       bool                `json:"refused"`
	Warnings      []string            `json:"warnings,omitempty"`
}

func newModelsUnloadCmd(flags *rootFlags) *cobra.Command {
	var flagModel string
	var flagAll bool
	var flagDrain bool
	var flagDrainTimeout time.Duration
	var flagForceKeepset bool
	var flagKeepset []string
	var flagDB string

	cmd := &cobra.Command{
		Use:   "unload [model|alias]",
		Short: "Unload ONE model by id or alias (keep-set protected, optionally drain-aware).",
		Long: strings.Trim(`
Unload a model, resolving aliases against meta.llamaswap.aliases from
/v1/models. Three guards stand between this command and a memory-stack
outage:

  KEEP-SET REFUSAL (default on). The keep-set is read from the llama-swap
  YAML (seats with ttl:-1 or ttl:0) unioned with a keep_set list in this
  CLI's own config. It is NEVER derived from the server's ttl field: GET
  /running reports ttl:0 for a model configured ttl:-1, so a keep-set built
  from the API would rest on a value the API misreports. Refusal matches
  ALIASES as well as ids, because the mem0 stack is routinely addressed as
  text-embedding / local-embed / reranker-v2-m3 / v0.12-reranker.
  Override with --force-keepset (loud, and recorded in the ledger).

  DRAIN (--drain). Polls /upstream/{model}/slots is_processing, but ONLY
  for models already in /running: any /upstream request auto-starts a
  stopped model, so probing to decide whether to unload would load it
  first. Unloading mid-generation does not drain — the in-flight request
  dies with 502 — so this check fails CLOSED: an unreadable /slots
  (timeout or 5xx) unloads NOTHING and names the unobservable targets.
  A 404 means llama-server was started without --slots; that is endpoint
  ABSENT, not unobservable, so the documented fallback runs instead
  (recent /api/metrics/activity rows with no terminal status) and the
  output says the fallback was used. Drain is probabilistic; the static
  keep-set is the load-bearing protection.

  PROVENANCE. Every unload writes an unload_provenance row (caller, drained,
  forced) so 'keepset audit' can attribute an eviction to its cause.

Exit codes: 3 model not found, 20 keep-set refusal, 21 drain timeout,
22 drain unobservable, 4 server unreachable.`, "\n"),
		Example: strings.Trim(`
  # Unload one model by canonical id
  llamaswap-pp-cli models unload gemma-4-e4b

  # Aliases resolve; this is the same seat
  llamaswap-pp-cli models unload offload-e4b --json

  # Wait for in-flight generation to finish first (fails closed)
  llamaswap-pp-cli models unload gemma-4-26b --drain --drain-timeout 60s

  # Everything except the keep-set
  llamaswap-pp-cli models unload --all

  # Refused by default: embeddinggemma backs the mem0 memory stack
  llamaswap-pp-cli models unload local-embed
`, "\n"),
		Annotations: map[string]string{
			"pp:endpoint":          "models.unload",
			"pp:method":            "POST",
			"pp:path":              "/api/models/unload/{model}",
			"pp:typed-exit-codes":  "3,20,21,22",
			"mcp:destructive-hint": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := strings.TrimSpace(flagModel)
			if target == "" && len(args) > 0 {
				target = strings.TrimSpace(args[0])
			}
			// Verify-friendly: validation lives here, not in cobra.Args or
			// MarkFlagRequired, so --dry-run can short-circuit before any IO.
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "models unload")
			}
			if !flagAll && target == "" {
				if flags.asJSON {
					if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
						"error": "requires a model id or alias",
						"usage": cmd.CommandPath() + " --help",
					}, flags); err != nil {
						return err
					}
					return usageErr(fmt.Errorf("%q requires a model id or alias", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			opts := spineUnloadOptions{
				Drain:        flagDrain,
				DrainTimeout: flagDrainTimeout,
				Force:        flagForceKeepset,
				ExtraKeepSet: flagKeepset,
				DBPath:       flagDB,
			}
			if flagAll {
				return spineRunUnloadAll(cmd, flags, opts)
			}
			return spineRunUnloadOne(cmd, flags, target, opts)
		},
	}
	cmd.Flags().StringVar(&flagModel, "model", "", "Model id or alias to unload (positional argument works too).")
	cmd.Flags().BoolVar(&flagAll, "all", false, "Unload every running model EXCEPT the keep-set (same as 'models unload-all').")
	cmd.Flags().BoolVar(&flagDrain, "drain", false, "Wait for in-flight requests to finish before unloading. Fails closed when slot state is unreadable.")
	cmd.Flags().DurationVar(&flagDrainTimeout, "drain-timeout", 30*time.Second, "How long --drain waits for the target to go idle before giving up (exit 21, nothing unloaded).")
	cmd.Flags().BoolVar(&flagForceKeepset, "force-keepset", false, "Override keep-set refusal. Loud, recorded in unload_provenance, and capable of taking the memory stack down.")
	cmd.Flags().StringSliceVar(&flagKeepset, "keepset", nil, "Extra keep-set names for this invocation (unioned with the YAML- and config-derived set).")
	cmd.Flags().StringVar(&flagDB, "db", "", "SQLite database file path for the provenance ledger (default: resolved data directory data.db).")

	return cmd
}

type spineUnloadOptions struct {
	Drain        bool
	DrainTimeout time.Duration
	Force        bool
	ExtraKeepSet []string
	DBPath       string
}

// spineRoster is an alias-aware view of /v1/models.
type spineRoster struct {
	entries []mirror.RosterEntry
	byName  map[string]int
}

func spineLoadRoster(ctx context.Context, c *mirror.Client) (*spineRoster, error) {
	entries, err := c.Models(ctx)
	if err != nil {
		return nil, spineExitErr(ExitServerUnreachable, fmt.Errorf("read roster from /v1/models: %w", err))
	}
	r := &spineRoster{entries: entries, byName: map[string]int{}}
	for i, e := range entries {
		r.byName[strings.ToLower(e.ID)] = i
		for _, a := range e.Aliases {
			if _, taken := r.byName[strings.ToLower(a)]; !taken {
				r.byName[strings.ToLower(a)] = i
			}
		}
	}
	return r, nil
}

func (r *spineRoster) Resolve(name string) (mirror.RosterEntry, bool) {
	if r == nil {
		return mirror.RosterEntry{}, false
	}
	i, ok := r.byName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return mirror.RosterEntry{}, false
	}
	return r.entries[i], true
}

// spineNamesOf returns every name a roster entry answers to.
func spineNamesOf(e mirror.RosterEntry) []string {
	return append([]string{e.ID}, e.Aliases...)
}

func spineRunUnloadOne(cmd *cobra.Command, flags *rootFlags, target string, opts spineUnloadOptions) error {
	ctx := cmd.Context()
	base, _ := spineBaseURL(flags)
	keep := mirror.LoadKeepSet(mirror.KeepSetOptions{Extra: opts.ExtraKeepSet})
	rep := &spineUnloadReport{
		SchemaVersion: spineSchemaVersion,
		Action:        "unload",
		BaseURL:       base,
		KeepSet:       keep.Names(),
		KeepSetSource: keep.Sources,
		Warnings:      keep.Warnings,
	}

	if cliutil.IsVerifyEnv() {
		rep.Results = append(rep.Results, spineUnloadResult{Requested: target, Action: "would_unload",
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
	entry, ok := roster.Resolve(target)
	if !ok {
		// A keep-set name that is not in the roster still refuses: the
		// protection must not depend on the roster being readable or on the
		// name being spelled the way the server spells it.
		if m, protected := keep.Match(target); protected {
			rep.Refused = true
			rep.Results = append(rep.Results, spineUnloadResult{
				Requested: target, Model: m.ID, Action: "refused_keepset",
				Notes: []string{"keep-set member (" + m.Origin + "); not present in the roster either"},
			})
			return spineWriteUnloadReport(cmd, flags, rep,
				spineExitErr(ExitKeepsetRefusal, fmt.Errorf("refusing to unload keep-set member %q", target)))
		}
		rep.Results = append(rep.Results, spineUnloadResult{Requested: target, Action: "not_found"})
		return spineWriteUnloadReport(cmd, flags, rep,
			spineExitErr(ExitModelNotFound, fmt.Errorf("no model or alias %q in the roster", target)))
	}

	names := append(spineNamesOf(entry), target)
	if m, protected := keep.MatchAny(names...); protected && !opts.Force {
		rep.Refused = true
		rep.Results = append(rep.Results, spineUnloadResult{
			Requested: target, Model: entry.ID, Aliases: entry.Aliases, Action: "refused_keepset",
			Notes: []string{
				fmt.Sprintf("keep-set member %q (%s); matched via %q", m.ID, m.Origin, target),
				"nothing was sent to the server. Override with --force-keepset if you accept taking this seat down.",
			},
		})
		return spineWriteUnloadReport(cmd, flags, rep,
			spineExitErr(ExitKeepsetRefusal, fmt.Errorf("refusing to unload keep-set member %q (matched %q)", m.ID, target)))
	}

	running, err := c.Running(ctx)
	if err != nil {
		return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /running: %w", err))
	}
	isRunning := false
	for _, r := range running {
		if r.Model == entry.ID {
			isRunning = true
			break
		}
	}

	var drained *bool
	drainMethod := ""
	var drainNotes []string
	if opts.Drain {
		outcome, derr := spineDrain(ctx, c, entry.ID, isRunning, opts.DrainTimeout)
		drainMethod = outcome.Method
		drainNotes = outcome.Notes
		if derr != nil {
			rep.Results = append(rep.Results, spineUnloadResult{
				Requested: target, Model: entry.ID, Aliases: entry.Aliases,
				Action: "refused_drain", DrainMethod: outcome.Method, Drained: boolPtr(false),
				Notes: append(outcome.Notes, derr.Error()),
			})
			return spineWriteUnloadReport(cmd, flags, rep, derr)
		}
		d := true
		drained = &d
	}

	db, dbWarn := spineOpenProvenance(ctx, opts.DBPath)
	if dbWarn != "" {
		rep.Warnings = append(rep.Warnings, dbWarn)
	}
	if db != nil {
		defer db.Close()
	}

	status, uerr := c.UnloadModel(ctx, entry.ID)
	result := spineUnloadResult{
		Requested: target, Model: entry.ID, Aliases: entry.Aliases,
		Route: "POST /api/models/unload/" + entry.ID, Status: status,
		Drained: drained, DrainMethod: drainMethod, Forced: opts.Force,
		Notes: drainNotes,
	}
	switch {
	case uerr != nil:
		result.Action = "error"
		result.Notes = append(result.Notes, uerr.Error())
		spineRecordProvenance(ctx, db, entry.ID, drained, opts.Force, "error: "+uerr.Error())
		rep.Results = append(rep.Results, result)
		return spineWriteUnloadReport(cmd, flags, rep, spineExitErr(ExitServerUnreachable, uerr))
	case status == 404:
		// Version drift: the per-model route is absent on this build. The bulk
		// routes cannot target one model, so this is reported, not silently
		// escalated into an unload-everything.
		result.Action = "route_absent"
		result.Notes = append(result.Notes,
			"POST /api/models/unload/{model} returned 404 on this build; the bulk routes are not selective, so no fallback was taken for a single-model unload")
		spineRecordProvenance(ctx, db, entry.ID, drained, opts.Force, "route_absent")
		rep.Results = append(rep.Results, result)
		return spineWriteUnloadReport(cmd, flags, rep, spineExitErr(ExitModelNotFound,
			errors.New("per-model unload route not available on this llama-swap build")))
	case status >= 200 && status < 300:
		result.Action = "unloaded"
		if !isRunning {
			result.Notes = append(result.Notes, "model was not in /running before the call; the unload is a no-op")
		}
		spineRecordProvenance(ctx, db, entry.ID, drained, opts.Force, "unloaded")
	default:
		result.Action = "error"
		spineRecordProvenance(ctx, db, entry.ID, drained, opts.Force, fmt.Sprintf("http_%d", status))
		rep.Results = append(rep.Results, result)
		return spineWriteUnloadReport(cmd, flags, rep, spineExitErr(ExitUpstream5xx,
			fmt.Errorf("unload %s: HTTP %d", entry.ID, status)))
	}
	rep.Results = append(rep.Results, result)
	return spineWriteUnloadReport(cmd, flags, rep, nil)
}

// spineDrainOutcome describes what the drain check could and could not observe.
type spineDrainOutcome struct {
	Method string
	Notes  []string
}

// spineDrain waits for one model to go idle.
//
// Fail-closed contract: the caller unloads ONLY when this returns a nil error.
// Unreadable slot state (timeout or 5xx) returns ExitDrainUnobservable and
// names the target; a still-busy model at the deadline returns ExitDrainTimeout.
// Neither path unloads anything.
func spineDrain(ctx context.Context, c *mirror.Client, model string, isRunning bool, timeout time.Duration) (spineDrainOutcome, error) {
	out := spineDrainOutcome{Method: "none"}
	if !isRunning {
		// Probing /upstream for a stopped model would AUTO-START it — the
		// exact opposite of the intent. Nothing is holding VRAM, so nothing
		// can be draining.
		out.Notes = append(out.Notes, "not in /running: no drain probe sent (an /upstream probe would auto-start the model)")
		return out, nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out, spineExitErr(ExitDrainTimeout, fmt.Errorf(
				"drain timed out after %s: %s still processing; nothing was unloaded", timeout, model))
		}
		callBudget := 2 * time.Second
		if remaining < callBudget {
			callBudget = remaining
		}
		callCtx, cancel := context.WithTimeout(ctx, callBudget)
		slots, status, err := c.Slots(callCtx, model)
		cancel()

		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		switch {
		case err == nil:
			out.Method = "slots"
			busy := false
			for _, s := range slots {
				if s.IsProcessing {
					busy = true
					break
				}
			}
			if !busy {
				return out, nil
			}
		case status == 404:
			// Endpoint absent (llama-server started without --slots), NOT
			// unobservable. Documented fallback: look for recent activity rows
			// that never reached a terminal status.
			out.Method = "activity-fallback"
			out.Notes = appendOnce(out.Notes,
				"/upstream/"+model+"/slots returned 404 (llama-server started without --slots); fell back to the /api/metrics/activity in-flight check")
			busy, ferr := spineActivityInFlight(ctx, c, model)
			if ferr != nil {
				return out, spineExitErr(ExitDrainUnobservable, fmt.Errorf(
					"drain unobservable for %s: /slots is absent (404) and the activity fallback failed (%v); nothing was unloaded", model, ferr))
			}
			if !busy {
				return out, nil
			}
		case status >= 500:
			return out, spineExitErr(ExitDrainUnobservable, fmt.Errorf(
				"drain unobservable for %s: /slots answered HTTP %d; nothing was unloaded", model, status))
		default:
			// No status at all means the request never completed: a timeout or
			// a transport failure. Unobservable, fail closed.
			return out, spineExitErr(ExitDrainUnobservable, fmt.Errorf(
				"drain unobservable for %s: /slots did not answer within the poll budget (%v); nothing was unloaded", model, err))
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// spineActivityInFlight is the documented fallback when /slots is absent: a
// request still in flight appears in the activity ring WITHOUT a terminal
// status. Weaker evidence than slot state, which is why the caller reports
// that the fallback was used rather than presenting it as equivalent.
func spineActivityInFlight(ctx context.Context, c *mirror.Client, model string) (bool, error) {
	page, err := c.Activity(ctx, mirror.ActivityOpts{Model: model, Limit: 25, Sort: "id", Order: "desc"})
	if err != nil {
		return false, err
	}
	for _, row := range page.Data {
		if !row.Terminal() {
			return true, nil
		}
	}
	return false, nil
}

func spineOpenProvenance(ctx context.Context, dbPath string) (*store.Store, string) {
	db, err := spineOpenDB(ctx, dbPath)
	if err != nil {
		return nil, "provenance ledger unavailable (" + err.Error() + "); the unload proceeds but 'keepset audit' will have a hole here"
	}
	return db, ""
}

func spineRecordProvenance(ctx context.Context, db *store.Store, model string, drained *bool, forced bool, result string) {
	if db == nil {
		return
	}
	var drainedVal any
	if drained != nil {
		if *drained {
			drainedVal = 1
		} else {
			drainedVal = 0
		}
	}
	forcedVal := 0
	if forced {
		forcedVal = 1
	}
	_, _ = db.DB().ExecContext(ctx,
		`INSERT INTO unload_provenance (ts, model, caller, drained, forced, result) VALUES (?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), model, "cli", drainedVal, forcedVal, result)
}

// spineWriteUnloadReport prints the envelope and then returns the typed error,
// so a refusal is both machine-readable on stdout AND a non-zero exit.
func spineWriteUnloadReport(cmd *cobra.Command, flags *rootFlags, rep *spineUnloadReport, exitErr error) error {
	if flags != nil && (flags.asJSON || !isTerminal(cmd.OutOrStdout())) {
		if err := printJSONFiltered(cmd.OutOrStdout(), rep, flags); err != nil {
			return err
		}
	} else {
		w := cmd.OutOrStdout()
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "MODEL\tACTION\tDRAINED\tFORCED\tNOTES")
		for _, r := range rep.Results {
			name := r.Model
			if name == "" {
				name = r.Requested
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", name, r.Action, spineBoolText(r.Drained), r.Forced, strings.Join(r.Notes, "; "))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
	}
	for _, r := range rep.Results {
		if r.Forced && r.Action == "unloaded" {
			fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: --force-keepset unloaded %s, a protected keep-set member. Recorded in unload_provenance.\n", r.Model)
		}
	}
	return exitErr
}

func spineBoolText(b *bool) string {
	if b == nil {
		return "-"
	}
	if *b {
		return "yes"
	}
	return "no"
}

func boolPtr(b bool) *bool { return &b }

func appendOnce(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}
