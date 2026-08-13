// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source live

package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/measure"
)

// Scratch seats live in a narrow, declared band. Anything outside it risks
// colliding with the proxy's own startPort span or with a service that is
// supposed to be there.
const (
	scratchPortMin     = 18796
	scratchPortMax     = 18799
	scratchDefaultPort = 18797
)

// scratchAllowlist is the ENTIRE set of flags a scratch seat may change from
// production. It is not a convenience limit - it is the feature. Two rankings
// on this box were invalidated by evals that silently dropped a production
// flag, so anything outside this list is refused rather than applied.
var scratchAllowlist = map[string][]string{
	"--port":         {"--port"},
	"--ctx-size":     {"-c", "--ctx-size"},
	"--n-gpu-layers": {"-ngl", "--n-gpu-layers", "--gpu-layers"},
}

type scratchOverride struct {
	Canonical string `json:"canonical_flag"`
	Applied   string `json:"applied_as"`
	Value     string `json:"value"`
	Was       string `json:"previous_value,omitempty"`
	Added     bool   `json:"added,omitempty"`
}

type scratchDiffLine struct {
	Flag string `json:"flag"`
	From string `json:"production"`
	To   string `json:"scratch"`
	Kind string `json:"kind"` // changed | added
}

type scratchPlan struct {
	SchemaVersion int               `json:"schema_version"`
	Source        string            `json:"cmd_source"`
	Model         string            `json:"model,omitempty"`
	Port          int               `json:"port"`
	ProductionCmd string            `json:"production_cmd"`
	ScratchCmd    string            `json:"scratch_cmd"`
	Overrides     []scratchOverride `json:"overrides"`
	Diff          []scratchDiffLine `json:"flag_diff"`
	HealthURL     string            `json:"health_url"`
	Hidden        bool              `json:"hidden_window"`
	Warnings      []string          `json:"warnings,omitempty"`
}

type scratchResult struct {
	Plan      *scratchPlan `json:"plan"`
	Started   bool         `json:"started"`
	PID       int          `json:"pid,omitempty"`
	HealthyIn float64      `json:"healthy_in_seconds,omitempty"`
	Note      string       `json:"note,omitempty"`
}

func newMeasureScratchCmd(flags *rootFlags) *cobra.Command {
	var (
		flagPort    int
		flagFromCmd string
		flagSet     []string
		flagPlan    bool
		flagWait    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "scratch <loaded-model>",
		Short: "Run an ephemeral eval seat derived EXACTLY from a production command line",
		Long: `Starts a throwaway llama-server on a scratch port using the exact command line
the production seat is running with, plus the overrides you name - and nothing
else.

Only three flags may be overridden: --port, -c/--ctx-size, and
-ngl/--n-gpu-layers. Every other --set is refused. That allowlist IS the
feature: an eval seat that quietly drops --reasoning, a CUDA device pin, or
--image-min-tokens produces a ranking that does not describe production, and
that has already happened twice here.

The full flag diff against production is printed before anything starts. The
child runs with no console window, in the foreground, and is killed when this
command exits (Ctrl-C).`,
		Example: `  llamaswap-pp-cli scratch gemma-4-e2b --port 18797 --set "-c 4096"
  llamaswap-pp-cli scratch gemma-4-e2b --plan --json
  llamaswap-pp-cli scratch --from-cmd "C:/llama.cpp/llama-server.exe -m V:/models/x.gguf --port 9201" --port 18798`,
		Annotations: map[string]string{
			"pp:typed-exit-codes":  "2=refused override, 3=model not loaded, 4=proxy unreachable, 23=port conflict",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && flagFromCmd == "" {
				if flags.asJSON {
					if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
						"error": "requires a loaded model name or --from-cmd",
						"usage": cmd.CommandPath() + " --help",
					}, flags); err != nil {
						return err
					}
					return usageErr(fmt.Errorf("%q requires a loaded model name or --from-cmd", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "scratch seat")
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()

			plan, err := scratchBuildPlan(ctx, cmd, flags, args, flagFromCmd, flagPort, flagSet)
			if err != nil {
				return err
			}

			if handled, err := mcVerifyPlanOnly(cmd, flags, "scratch", map[string]any{
				"plan": plan,
				"note": "scratch spawns a real llama-server; under the verifier it only plans",
			}); handled {
				return err
			}
			if flagPlan {
				return mcEmit(cmd, flags, &scratchResult{Plan: plan, Note: "plan only (--plan): nothing was started"},
					func(w io.Writer) { scratchPrintPlan(w, plan, "plan only: nothing started") })
			}

			// Port must be free. Fail closed: a scratch seat that quietly
			// attaches to somebody else's listener measures that listener.
			if err := scratchPortFree(plan.Port); err != nil {
				return err
			}
			return scratchRun(ctx, cmd, flags, plan, flagWait)
		},
	}
	cmd.Flags().IntVar(&flagPort, "port", scratchDefaultPort, fmt.Sprintf("Scratch port (must be %d-%d)", scratchPortMin, scratchPortMax))
	cmd.Flags().StringVar(&flagFromCmd, "from-cmd", "", "Use this exact command line instead of deriving it from a loaded seat")
	cmd.Flags().StringArrayVar(&flagSet, "set", nil, `Override an allowlisted flag, e.g. --set "-c 4096" (repeatable)`)
	cmd.Flags().BoolVar(&flagPlan, "plan", false, "Print the derived command and flag diff without starting anything")
	cmd.Flags().DurationVar(&flagWait, "wait", 3*time.Minute, "How long to wait for the scratch seat to become healthy")
	return cmd
}

func scratchBuildPlan(ctx context.Context, cmd *cobra.Command, flags *rootFlags, args []string, fromCmd string, port int, sets []string) (*scratchPlan, error) {
	plan := &scratchPlan{SchemaVersion: 1, Port: port, Hidden: true}

	if port < scratchPortMin || port > scratchPortMax {
		return nil, &cliError{code: ExitPortConflict, err: fmt.Errorf(
			"port %d is outside the scratch band %d-%d; scratch seats declare their port inside a reserved band so they cannot collide with the proxy's own startPort span",
			port, scratchPortMin, scratchPortMax)}
	}

	production := strings.TrimSpace(fromCmd)
	if production != "" {
		plan.Source = "--from-cmd"
	} else {
		timeout := mcTimeout(cmd, flags, 15*time.Second)
		seats, err := mcRunning(ctx, flags, timeout)
		if err != nil {
			return nil, mcClassify(err)
		}
		name := args[0]
		if roster, rErr := mcRoster(ctx, flags, timeout); rErr == nil {
			if resolved, ok := mcResolveAlias(roster, name); ok {
				name = resolved
			}
		}
		seat, ok := mcFindSeat(seats, name)
		if !ok {
			return nil, &cliError{code: ExitModelNotFound, err: fmt.Errorf(
				"%q is not loaded, so there is no live command line to derive from (loaded: %s). "+
					"Load it first, or pass the exact command with --from-cmd. Reading it out of the YAML is `config explain`'s job, not this command's",
				args[0], mcJoinOrNone(mcLoadedNames(seats)))}
		}
		production, plan.Model, plan.Source = seat.Cmd, name, fmt.Sprintf("live /running seat %q", name)
	}
	plan.ProductionCmd = production

	tokens := mcSplitCmd(production)
	if len(tokens) == 0 {
		return nil, usageErr(fmt.Errorf("empty command line"))
	}
	if !mcIsLlamaServer(production) {
		plan.Warnings = append(plan.Warnings, "this command line is not llama-server; the allowlisted flags may not mean what they mean for llama.cpp")
	}

	// Parse and validate every --set BEFORE touching anything.
	overrides := []scratchOverride{{Canonical: "--port", Applied: "--port", Value: strconv.Itoa(port)}}
	for _, raw := range sets {
		key, value, err := scratchParseSet(raw)
		if err != nil {
			return nil, err
		}
		canonical, ok := scratchCanonical(key)
		if !ok {
			return nil, usageErr(fmt.Errorf(
				"REFUSED: --set %q. Only %s may differ from production. Everything else is refused on purpose: "+
					"an eval seat that silently drops a production flag produces a ranking that does not describe production",
				key, scratchAllowedList()))
		}
		if canonical == "--port" {
			p, err := strconv.Atoi(value)
			if err != nil {
				return nil, usageErr(fmt.Errorf("--set %q: port must be a number", raw))
			}
			if p < scratchPortMin || p > scratchPortMax {
				return nil, &cliError{code: ExitPortConflict, err: fmt.Errorf("--set port %d is outside the scratch band %d-%d", p, scratchPortMin, scratchPortMax)}
			}
			plan.Port = p
			overrides[0].Value = value
			continue
		}
		overrides = append(overrides, scratchOverride{Canonical: canonical, Value: value})
	}
	overrides[0].Value = strconv.Itoa(plan.Port)

	scratchTokens := append([]string(nil), tokens...)
	for i := range overrides {
		applied, was, added := scratchApply(scratchTokens, overrides[i].Canonical, overrides[i].Value)
		scratchTokens = applied
		overrides[i].Was, overrides[i].Added = was, added
		overrides[i].Applied = scratchAppliedSpelling(tokens, overrides[i].Canonical)
		kind := "changed"
		if added {
			kind = "added"
		}
		plan.Diff = append(plan.Diff, scratchDiffLine{
			Flag: overrides[i].Applied, From: was, To: overrides[i].Value, Kind: kind,
		})
	}
	plan.Overrides = overrides
	plan.ScratchCmd = strings.Join(scratchTokens, " ")
	plan.HealthURL = fmt.Sprintf("http://%s:%d/health", mcLoopback, plan.Port)
	return plan, nil
}

func scratchParseSet(raw string) (string, string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", usageErr(fmt.Errorf("empty --set"))
	}
	if !strings.HasPrefix(s, "-") {
		return "", "", usageErr(fmt.Errorf("--set %q must start with a flag, e.g. --set \"-c 4096\"", raw))
	}
	if i := strings.IndexAny(s, "= \t"); i > 0 {
		return s[:i], strings.TrimSpace(s[i+1:]), nil
	}
	return "", "", usageErr(fmt.Errorf("--set %q needs a value, e.g. --set \"-c 4096\"", raw))
}

func scratchCanonical(flag string) (string, bool) {
	for canonical, spellings := range scratchAllowlist {
		for _, s := range spellings {
			if s == flag {
				return canonical, true
			}
		}
	}
	return "", false
}

func scratchAllowedList() string {
	var all []string
	for canonical, spellings := range scratchAllowlist {
		all = append(all, fmt.Sprintf("%s (%s)", canonical, strings.Join(spellings, "|")))
	}
	sort.Strings(all)
	return strings.Join(all, ", ")
}

// scratchAppliedSpelling keeps the production command's own spelling of a
// flag (-c vs --ctx-size) so the printed diff lines up with the real cmd.
func scratchAppliedSpelling(tokens []string, canonical string) string {
	for _, s := range scratchAllowlist[canonical] {
		for _, tok := range tokens {
			if tok == s || strings.HasPrefix(tok, s+"=") {
				return s
			}
		}
	}
	return canonical
}

// scratchApply replaces (or appends) one flag's value, returning the previous
// value and whether the flag was newly added.
func scratchApply(tokens []string, canonical, value string) ([]string, string, bool) {
	spellings := scratchAllowlist[canonical]
	for i, tok := range tokens {
		for _, s := range spellings {
			if tok == s && i+1 < len(tokens) {
				was := tokens[i+1]
				out := append([]string(nil), tokens...)
				out[i+1] = value
				return out, was, false
			}
			if strings.HasPrefix(tok, s+"=") {
				was := strings.TrimPrefix(tok, s+"=")
				out := append([]string(nil), tokens...)
				out[i] = s + "=" + value
				return out, was, false
			}
		}
	}
	return append(append([]string(nil), tokens...), canonical, value), "", true
}

// scratchPortFree fails closed on a busy port.
func scratchPortFree(port int) error {
	addr := net.JoinHostPort(mcLoopback, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 750*time.Millisecond)
	if err == nil {
		conn.Close()
		return &cliError{code: ExitPortConflict, err: fmt.Errorf(
			"something is already listening on %s; refusing to start a scratch seat there (a scratch seat that attaches to somebody else's listener measures that listener)", addr)}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return &cliError{code: ExitPortConflict, err: fmt.Errorf("cannot bind %s: %w", addr, err)}
	}
	return ln.Close()
}

func scratchRun(ctx context.Context, cmd *cobra.Command, flags *rootFlags, plan *scratchPlan, wait time.Duration) error {
	tokens := mcSplitCmd(plan.ScratchCmd)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	child := exec.CommandContext(runCtx, tokens[0], tokens[1:]...)
	measure.HideWindow(child)
	child.Stdout = cmd.ErrOrStderr()
	child.Stderr = cmd.ErrOrStderr()

	scratchPrintPlan(cmd.ErrOrStderr(), plan, "starting")
	start := time.Now()
	if err := child.Start(); err != nil {
		return fmt.Errorf("starting scratch seat: %w", err)
	}
	pid := child.Process.Pid

	done := make(chan error, 1)
	go func() { done <- child.Wait() }()

	// Kill the child on Ctrl-C, on context cancellation, and on return.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	killed := false
	kill := func() {
		if killed || child.Process == nil {
			return
		}
		killed = true
		_ = child.Process.Kill()
	}
	defer kill()

	healthy := false
	deadline := time.Now().Add(wait)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) && !healthy {
		select {
		case err := <-done:
			return fmt.Errorf("scratch seat exited before becoming healthy (%v); see its log above", err)
		case <-sigCh:
			kill()
			return nil
		case <-runCtx.Done():
			kill()
			return runCtx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		resp, err := client.Get(plan.HealthURL)
		if err == nil {
			resp.Body.Close()
			healthy = resp.StatusCode == http.StatusOK
		}
	}
	if !healthy {
		kill()
		return fmt.Errorf("scratch seat did not answer %s within %s; killed it", plan.HealthURL, wait)
	}

	result := &scratchResult{
		Plan: plan, Started: true, PID: pid,
		HealthyIn: time.Since(start).Seconds(),
		Note:      "foreground: Ctrl-C stops the scratch seat, and it is killed when this command exits",
	}
	if err := mcEmit(cmd, flags, result, func(w io.Writer) {
		fmt.Fprintf(w, "%s pid %d healthy in %.1fs at %s\n", green("scratch seat up:"), pid, result.HealthyIn, plan.HealthURL)
		fmt.Fprintf(w, "  Ctrl-C to stop it (the child is killed on exit)\n")
	}); err != nil {
		return err
	}

	select {
	case <-sigCh:
		fmt.Fprintln(cmd.ErrOrStderr(), "\nstopping scratch seat...")
		kill()
		<-done
		return nil
	case err := <-done:
		return fmt.Errorf("scratch seat exited: %w", err)
	case <-runCtx.Done():
		kill()
		return runCtx.Err()
	}
}

func scratchPrintPlan(w io.Writer, p *scratchPlan, state string) {
	fmt.Fprintf(w, "%s  (%s)\n", bold("scratch seat"), state)
	fmt.Fprintf(w, "  derived from    %s\n", p.Source)
	fmt.Fprintf(w, "  production cmd  %s\n", p.ProductionCmd)
	fmt.Fprintf(w, "  scratch cmd     %s\n", p.ScratchCmd)
	fmt.Fprintf(w, "  %s\n", bold("flag diff vs production"))
	for _, d := range p.Diff {
		from := d.From
		if from == "" {
			from = "(absent)"
		}
		fmt.Fprintf(w, "    %-14s %s -> %s   [%s]\n", d.Flag, from, d.To, d.Kind)
	}
	fmt.Fprintf(w, "  everything else is IDENTICAL to production (only %s may be overridden)\n", scratchAllowedList())
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "  %s %s\n", yellow("warning:"), warn)
	}
}
