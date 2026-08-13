// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): warm a seat on purpose.
// pp:data-source live

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/lsconfig"
	"llamaswap-pp-cli/internal/mirror"
)

// glueLoadReport is the load command's envelope.
type glueLoadReport struct {
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"`
	BaseURL       string `json:"base_url"`
	Requested     string `json:"requested"`
	Model         string `json:"model,omitempty"`
	// SeatKind is llama-server or non-llama-server. A whisper-server seat
	// takes a different warming route, so it is reported rather than assumed.
	SeatKind string `json:"seat_kind,omitempty"`
	// Route is the endpoint used to trigger the load.
	Route string `json:"route,omitempty"`
	// AlreadyLoaded is true when the model was resident before the call, in
	// which case nothing was loaded and the timings mean nothing.
	AlreadyLoaded bool `json:"already_loaded"`
	// Ready is true when /running reported the seat ready.
	Ready bool `json:"ready"`
	// State is the last state observed from /running.
	State string `json:"state,omitempty"`
	// WaitedMS is wall time spent waiting for readiness.
	WaitedMS int64 `json:"waited_ms"`
	// Evicted lists models that were resident before the load and are not
	// after it — the swap this load cost. Reported, never prevented: llama-swap
	// owns placement.
	Evicted []string `json:"evicted,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

func newGlueLoadCmd(flags *rootFlags) *cobra.Command {
	var flagWait bool
	var flagTimeout time.Duration
	var flagPoll time.Duration

	cmd := &cobra.Command{
		Use:   "load <model|alias>",
		Short: "Warm a model into VRAM on purpose, with progress and a swap report.",
		Long: strings.Trim(`
llama-swap loads on demand: the first real request pays the load. 'load' pays it
deliberately, up front, so the next caller does not eat a cold start it did not
schedule.

The warming request is the MINIMUM that triggers a load for the seat kind:

  llama-server seats    a 1-token POST /v1/chat/completions through the
                        production route. Not /upstream: the production route
                        is what real traffic uses, and it is the route whose
                        readiness actually matters.
  non-llama-server      (whisper-server and friends) a GET /upstream/{id}/health
  seats                 touch, since they answer no chat route. The seat kind
                        comes from the config, so a whisper seat is never
                        false-positived as broken.

With --wait the command polls /running until the seat reports ready, then prints
what it cost: the models evicted to make room. Ctrl-C during the wait cancels
WAITING, not the load — the server keeps going, because a half-loaded model
killed mid-flight is worse than a load you stopped watching. The command says so
when it exits that way.

Alias-aware: 'load local-embed' and 'load embeddinggemma' address the same seat.

Exit codes: 3 model not found, 4 server unreachable, 27 upstream 5xx.`, "\n"),
		Example: strings.Trim(`
  # Warm the triage model and wait for it to be ready
  llamaswap-pp-cli load gemma-4-e2b --wait

  # Aliases resolve; this is the same seat
  llamaswap-pp-cli load gemma4-e2b --wait --timeout 300s --json

  # Fire the load and return immediately
  llamaswap-pp-cli load gemma-4-26b
`, "\n"),
		Annotations: map[string]string{
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,3,4,27",
			// Not read-only: this command deliberately changes GPU residency
			// and can evict another model.
			"mcp:destructive-hint": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) > 0 {
				target = strings.TrimSpace(args[0])
			}
			// Validation in RunE, never cobra.Args/MarkFlagRequired, so a
			// dry-run or verify pass short-circuits before it fires.
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "load "+target)
			}
			if target == "" {
				return glueUsageErrf("%s requires a model id or alias; run %q for usage", cmd.CommandPath(), cmd.CommandPath()+" --help")
			}
			if cliutil.IsVerifyEnv() {
				return printJSONFiltered(cmd.OutOrStdout(), glueLoadReport{
					SchemaVersion: glueSchemaVersion,
					Action:        "would_load",
					Requested:     target,
					Notes:         []string{"PRINTING_PRESS_VERIFY=1: no warming request sent; VRAM untouched"},
				}, flags)
			}
			return glueRunLoad(cmd, flags, target, flagWait, flagTimeout, flagPoll)
		},
	}
	cmd.Flags().BoolVar(&flagWait, "wait", false, "Poll /running until the seat reports ready before returning.")
	cmd.Flags().DurationVar(&flagTimeout, "timeout", 300*time.Second, "How long --wait polls before giving up. The server keeps loading either way.")
	cmd.Flags().DurationVar(&flagPoll, "poll-interval", time.Second, "How often --wait re-reads /running.")
	return cmd
}

func glueRunLoad(cmd *cobra.Command, flags *rootFlags, target string, wait bool, timeout, poll time.Duration) error {
	ctx := cmd.Context()
	base, _ := spineBaseURL(flags)
	rep := &glueLoadReport{SchemaVersion: glueSchemaVersion, Action: "load", BaseURL: base, Requested: target}

	// A load can take minutes; the client deadline must allow for one even
	// when the operator never touched --timeout.
	c, err := glueClientWithTimeout(flags, 10*time.Minute)
	if err != nil {
		return err
	}
	entry, err := glueResolve(ctx, c, target)
	if err != nil {
		return err
	}
	rep.Model = entry.ID

	before, err := c.Running(ctx)
	if err != nil {
		return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /running: %w", err))
	}
	rep.AlreadyLoaded = glueIsRunning(before, entry.ID)

	rep.SeatKind, rep.Notes = glueSeatKind(entry.ID, rep.Notes)

	if rep.AlreadyLoaded {
		rep.Ready = true
		rep.State = "ready"
		rep.Notes = append(rep.Notes, "already in /running: no warming request sent")
		return glueWriteLoadReport(cmd, flags, rep)
	}

	start := time.Now()
	route, warmErr := glueWarm(ctx, c, base, entry.ID, rep.SeatKind, flags)
	rep.Route = route
	if warmErr != nil {
		rep.Notes = append(rep.Notes, warmErr.Error())
		// A warming request that errors may STILL have started the load
		// (llama-swap begins the swap before the upstream answers), so the
		// wait below is still worth doing. Report honestly and continue.
		rep.Notes = append(rep.Notes, "the warming request failed; llama-swap may still be loading — check 'llamaswap-pp-cli ps'")
	}

	if wait {
		state, ready, werr := glueWaitReady(ctx, cmd, flags, c, entry.ID, timeout, poll)
		rep.State = state
		rep.Ready = ready
		if werr != nil {
			rep.Notes = append(rep.Notes, werr.Error())
		}
	} else {
		rep.Notes = append(rep.Notes, "not waiting (--wait polls /running until the seat is ready)")
	}
	rep.WaitedMS = time.Since(start).Milliseconds()

	after, aerr := c.Running(ctx)
	if aerr == nil {
		rep.Evicted = glueEvicted(before, after)
		if !rep.Ready {
			for _, r := range after {
				if strings.EqualFold(r.Model, entry.ID) {
					rep.State = r.State
					rep.Ready = strings.EqualFold(r.State, "ready")
				}
			}
		}
	}
	if len(rep.Evicted) > 0 {
		rep.Notes = append(rep.Notes,
			"this load evicted "+strings.Join(rep.Evicted, ", ")+" — llama-swap owns placement; the eviction is reported, not prevented")
	}
	if warmErr != nil && !rep.Ready {
		return glueWriteLoadReportErr(cmd, flags, rep, spineExitErr(ExitUpstream5xx, warmErr))
	}
	return glueWriteLoadReport(cmd, flags, rep)
}

// glueSeatKind classifies a seat from the llama-swap YAML so the warming route
// matches the binary. A whisper-server seat answers no chat route; sending it
// one and calling the 404 a failure is the false positive this avoids.
func glueSeatKind(id string, notes []string) (string, []string) {
	path, err := lsconfig.DefaultConfigPath()
	if err != nil {
		return string(lsconfig.SeatLlamaServer), append(notes,
			"llama-swap YAML not found; assuming a llama-server seat for the warming route")
	}
	f, err := lsconfig.Load(path, lsconfig.LoadOptions{})
	if err != nil {
		return string(lsconfig.SeatLlamaServer), append(notes,
			"llama-swap YAML unreadable ("+err.Error()+"); assuming a llama-server seat for the warming route")
	}
	m, ok := f.Resolve(id)
	if !ok {
		return string(lsconfig.SeatLlamaServer), append(notes,
			"seat "+id+" not found in the YAML; assuming a llama-server seat for the warming route")
	}
	return string(m.Seat), notes
}

// glueWarm sends the minimal request that makes llama-swap start the seat.
func glueWarm(ctx context.Context, c *mirror.Client, base, id, seatKind string, flags *rootFlags) (string, error) {
	if seatKind == string(lsconfig.SeatNonLlamaServer) {
		// No chat route on this binary. A health touch through the passthrough
		// is enough to trigger the swap.
		status, err := c.UpstreamHealth(ctx, id)
		route := "GET /upstream/" + id + "/health"
		if err != nil {
			return route, fmt.Errorf("warm %s: %w", id, err)
		}
		if status >= 500 {
			return route, fmt.Errorf("warm %s: HTTP %d", id, status)
		}
		return route, nil
	}
	route := "POST /v1/chat/completions (max_tokens=1)"
	body := map[string]any{
		"model":      id,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
		"max_tokens": 1,
		"stream":     false,
	}
	// The production route, on purpose: it is what real traffic uses, so its
	// readiness is the readiness that matters.
	if err := mcPostJSON(ctx, flags, "/v1/chat/completions", body, 10*time.Minute, nil); err != nil {
		return route, fmt.Errorf("warm %s: %w", id, err)
	}
	return route, nil
}

// glueWaitReady polls /running until the seat is ready.
//
// Ctrl-C cancels the WAIT, not the load: the signal context is scoped to this
// loop, and the note says so. Interrupting a multi-GB load halfway is a worse
// outcome than stopping the progress display.
func glueWaitReady(ctx context.Context, cmd *cobra.Command, flags *rootFlags, c *mirror.Client, id string, timeout, poll time.Duration) (string, bool, error) {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	if poll <= 0 {
		poll = time.Second
	}
	waitCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	// Progress goes to stderr and only for an interactive human: a machine
	// caller's stdout must stay pure JSON.
	human := wantsHumanTable(cmd.OutOrStdout(), flags) && isTerminal(cmd.ErrOrStderr())
	deadline := time.Now().Add(timeout)
	lastState := ""
	for {
		running, err := c.Running(waitCtx)
		if err == nil {
			for _, r := range running {
				if !strings.EqualFold(r.Model, id) {
					continue
				}
				lastState = r.State
				if strings.EqualFold(r.State, "ready") {
					if human {
						fmt.Fprintf(cmd.ErrOrStderr(), "\r%s: ready            \n", id)
					}
					return r.State, true, nil
				}
			}
		}
		if human {
			state := lastState
			if state == "" {
				state = "starting"
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "\r%s: %s (%.0fs)   ", id, state, time.Until(deadline).Seconds())
		}
		select {
		case <-waitCtx.Done():
			if human {
				fmt.Fprintln(cmd.ErrOrStderr())
			}
			return lastState, false, fmt.Errorf(
				"stopped waiting (interrupted); the server was NOT told to stop and is probably still loading %s", id)
		case <-time.After(poll):
		}
		if time.Now().After(deadline) {
			if human {
				fmt.Fprintln(cmd.ErrOrStderr())
			}
			return lastState, false, fmt.Errorf(
				"gave up waiting after %s; the load is still running server-side (check 'llamaswap-pp-cli ps')", timeout)
		}
	}
}

// glueEvicted names models resident before a load and gone after it.
func glueEvicted(before, after []mirror.RunningEntry) []string {
	post := map[string]bool{}
	for _, r := range after {
		post[strings.ToLower(r.Model)] = true
	}
	var out []string
	for _, r := range before {
		if !post[strings.ToLower(r.Model)] {
			out = append(out, r.Model)
		}
	}
	return out
}

func glueWriteLoadReport(cmd *cobra.Command, flags *rootFlags, rep *glueLoadReport) error {
	return glueWriteLoadReportErr(cmd, flags, rep, nil)
}

func glueWriteLoadReportErr(cmd *cobra.Command, flags *rootFlags, rep *glueLoadReport, exitErr error) error {
	err := mcEmit(cmd, flags, rep, func(w io.Writer) {
		fmt.Fprintf(w, "model:    %s\n", rep.Model)
		fmt.Fprintf(w, "seat:     %s\n", orDash(rep.SeatKind))
		fmt.Fprintf(w, "route:    %s\n", orDash(rep.Route))
		fmt.Fprintf(w, "ready:    %v (state %s)\n", rep.Ready, orDash(rep.State))
		fmt.Fprintf(w, "waited:   %.1fs\n", float64(rep.WaitedMS)/1000)
		if len(rep.Evicted) > 0 {
			fmt.Fprintf(w, "evicted:  %s\n", strings.Join(rep.Evicted, ", "))
		}
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	})
	if err != nil {
		return err
	}
	return exitErr
}
