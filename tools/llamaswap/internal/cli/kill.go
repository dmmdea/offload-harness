// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): the polite alternative to unload-mid-flight.
// pp:data-source live

package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/mirror"
)

// glueKillTarget is one cancellation attempt.
type glueKillTarget struct {
	// ID is the request id passed to the cancel route.
	ID string `json:"id"`
	// Model is the model the request was served by, when known from the feed.
	Model string `json:"model,omitempty"`
	// ReqPath is the endpoint the request hit, when known.
	ReqPath string `json:"req_path,omitempty"`
	// Status is the HTTP status of the cancel call.
	Status int `json:"status,omitempty"`
	// Outcome is cancelled, not_found, or error.
	Outcome string `json:"outcome"`
	// Note carries anything the fields cannot say.
	Note string `json:"note,omitempty"`
}

// glueKillReport is the kill command's envelope.
type glueKillReport struct {
	SchemaVersion int              `json:"schema_version"`
	Action        string           `json:"action"`
	BaseURL       string           `json:"base_url"`
	Selector      string           `json:"selector"`
	Targets       []glueKillTarget `json:"targets"`
	Cancelled     int              `json:"cancelled"`
	Notes         []string         `json:"notes,omitempty"`
}

func newGlueKillCmd(flags *rootFlags) *cobra.Command {
	var flagModel string
	var flagAll bool

	cmd := &cobra.Command{
		Use:   "kill [request-id...]",
		Short: "Cancel in-flight requests by id or by model — the polite alternative to unloading mid-generation.",
		Long: strings.Trim(`
Cancel a request that is still running, without touching the model that is
serving it.

This is the verb to reach for instead of 'models unload' when the problem is one
runaway request: unloading mid-generation kills the in-flight request with a 502
and costs a full reload for the next caller. Cancelling costs nothing else.

Three ways to select what to cancel:

  kill 431 432        explicit request ids
  kill --model X      every in-flight request currently served by model X
  kill --all          every in-flight request, whatever the model

Ids come from the activity feed: a request that has not finished appears there
WITHOUT a terminal HTTP status. That is also the definition used by --model and
--all, so those two select exactly the rows 'activity' shows as unfinished.

A 404 from the cancel route means the request finished between listing and
cancelling — reported as not_found, not as an error, because nothing is wrong.

Exit codes: 2 usage, 4 server unreachable.`, "\n"),
		Example: strings.Trim(`
  # Cancel one request by id
  llamaswap-pp-cli kill 431

  # Cancel everything a runaway model is currently serving
  llamaswap-pp-cli kill --model gemma-4-26b --json

  # Cancel every in-flight request
  llamaswap-pp-cli kill --all
`, "\n"),
		Annotations: map[string]string{
			"pp:data-source":       "live",
			"pp:typed-exit-codes":  "0,2,4",
			"mcp:destructive-hint": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validation in RunE, not cobra.Args, so --dry-run short-circuits
			// before it fires.
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "kill")
			}
			selector := strings.Join(args, ",")
			switch {
			case len(args) > 0 && (flagAll || flagModel != ""):
				return glueUsageErrf("%s takes explicit ids OR --model/--all, not both", cmd.CommandPath())
			case len(args) == 0 && !flagAll && flagModel == "":
				return glueUsageErrf("%s requires a request id, --model, or --all; run %q for usage",
					cmd.CommandPath(), cmd.CommandPath()+" --help")
			case flagModel != "":
				selector = "model=" + flagModel
			case flagAll:
				selector = "all"
			}
			if cliutil.IsVerifyEnv() {
				return printJSONFiltered(cmd.OutOrStdout(), glueKillReport{
					SchemaVersion: glueSchemaVersion,
					Action:        "would_cancel",
					Selector:      selector,
					Notes:         []string{"PRINTING_PRESS_VERIFY=1: no cancel request sent"},
				}, flags)
			}
			return glueRunKill(cmd, flags, args, flagModel, flagAll, selector)
		},
	}
	cmd.Flags().StringVar(&flagModel, "model", "", "Cancel every in-flight request served by this model (id or alias).")
	cmd.Flags().BoolVar(&flagAll, "all", false, "Cancel every in-flight request, whatever the model.")
	return cmd
}

func glueRunKill(cmd *cobra.Command, flags *rootFlags, ids []string, model string, all bool, selector string) error {
	ctx := cmd.Context()
	base, _ := spineBaseURL(flags)
	rep := &glueKillReport{SchemaVersion: glueSchemaVersion, Action: "kill", BaseURL: base, Selector: selector}

	c, err := glueClient(flags)
	if err != nil {
		return err
	}

	var targets []glueKillTarget
	if len(ids) > 0 {
		for _, id := range ids {
			targets = append(targets, glueKillTarget{ID: strings.TrimSpace(id)})
		}
	} else {
		canonical := ""
		if model != "" {
			entry, rerr := glueResolve(ctx, c, model)
			if rerr != nil {
				return rerr
			}
			canonical = entry.ID
			rep.Selector = "model=" + canonical
		}
		found, ferr := glueInflight(ctx, c, canonical)
		if ferr != nil {
			return spineExitErr(ExitServerUnreachable, ferr)
		}
		targets = found
		if len(targets) == 0 {
			rep.Notes = append(rep.Notes, "no in-flight requests matched (the activity feed shows no rows without a terminal status)")
		}
	}

	// The generated endpoint client, on the generated route — same transport
	// every other command uses, with the loopback rewrite applied.
	api, err := newLoopbackClient(flags)
	if err != nil {
		return err
	}
	for i := range targets {
		t := &targets[i]
		path := replacePathParam("/api/inflight/{id}/cancel", "id", formatCLIParamValue(t.ID))
		_, status, cerr := api.PostWithParams(ctx, path, map[string]string{}, map[string]any{})
		t.Status = status
		switch {
		case status == 404:
			t.Outcome = "not_found"
			t.Note = "the request finished between listing and cancelling; nothing to cancel"
		case cerr != nil:
			t.Outcome = "error"
			t.Note = cerr.Error()
		case status >= 200 && status < 300:
			t.Outcome = "cancelled"
			rep.Cancelled++
		default:
			t.Outcome = "error"
			t.Note = fmt.Sprintf("HTTP %d", status)
		}
	}
	rep.Targets = targets

	return mcEmit(cmd, flags, rep, func(w io.Writer) {
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "ID\tMODEL\tPATH\tOUTCOME\tNOTE")
		for _, t := range rep.Targets {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, orDash(t.Model), orDash(t.ReqPath), t.Outcome, orDash(t.Note))
		}
		_ = tw.Flush()
		fmt.Fprintf(w, "cancelled %d of %d\n", rep.Cancelled, len(rep.Targets))
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	})
}

// glueInflight lists activity rows that never reached a terminal HTTP status —
// the observable definition of "still running". Optionally narrowed to one
// canonical model id.
func glueInflight(ctx context.Context, c *mirror.Client, model string) ([]glueKillTarget, error) {
	page, err := c.Activity(ctx, mirror.ActivityOpts{Model: model, Limit: 200, Sort: "id", Order: "desc"})
	if err != nil {
		return nil, fmt.Errorf("read /api/metrics/activity: %w", err)
	}
	var out []glueKillTarget
	for _, row := range page.Data {
		if row.Terminal() {
			continue
		}
		if model != "" && !strings.EqualFold(row.Model, model) {
			continue
		}
		out = append(out, glueKillTarget{
			ID:      strconv.FormatInt(row.ID, 10),
			Model:   row.Model,
			ReqPath: row.ReqPath,
		})
	}
	return out, nil
}
