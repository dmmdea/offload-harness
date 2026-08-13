// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The grammar probe is falsifiable by construction: the grammar admits only
// two strings, and the question has a third, obvious answer. A seat that
// answers "4" did NOT enforce the grammar, however confident its reply looks.
const (
	gateGrammarGBNF     = `root ::= "banana" | "pineapple"`
	gateGrammarQuestion = "What is 2+2?"
)

type gateVerdict struct {
	Model       string  `json:"model"`
	RequestedAs string  `json:"requested_as,omitempty"`
	Probe       string  `json:"probe"`
	Pass        bool    `json:"pass"`
	Verdict     string  `json:"verdict"`
	Detail      string  `json:"detail,omitempty"`
	Reply       string  `json:"reply,omitempty"`
	ToolName    string  `json:"tool_call_name,omitempty"`
	ToolArgs    string  `json:"tool_call_arguments,omitempty"`
	HTTPStatus  int     `json:"http_status,omitempty"`
	WasLoaded   bool    `json:"was_loaded_before"`
	LatencyMS   float64 `json:"latency_ms"`
}

type gateReport struct {
	SchemaVersion int           `json:"schema_version"`
	Probe         string        `json:"probe"`
	Route         string        `json:"route"`
	RanAt         string        `json:"ran_at"`
	Verdicts      []gateVerdict `json:"verdicts"`
	Notes         []string      `json:"notes,omitempty"`
}

func newMeasureGateCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gate",
		Short: "Falsifiable capability probes: gate grammar, gate tools",
		Long: `Probes a seat for a capability through the production chat route and returns a
per-seat JSON verdict.

These probes LOAD models. There is no --all: a capability sweep across a roster
would swap every model in, one after another. Name the seats you mean.`,
		Example: `  llamaswap-pp-cli gate grammar gemma-4-e2b
  llamaswap-pp-cli gate tools gemma-4-e2b --json`,
		Annotations: map[string]string{"pp:measurement-owner": "wave-c"},
		RunE:        parentNoSubcommandRunE(flags),
	}
	addNovelCommandIfAbsent(cmd, newMeasureGateGrammarCmd(flags))
	addNovelCommandIfAbsent(cmd, newMeasureGateToolsCmd(flags))
	return cmd
}

func newMeasureGateGrammarCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grammar <model...>",
		Short: "Does this seat actually enforce a GBNF grammar on the chat route?",
		Long: `Sends the question "What is 2+2?" with a grammar that admits only the strings
"banana" and "pineapple".

  PASS  the reply is exactly banana or pineapple - the grammar was enforced.
  FAIL  the reply is "4" or anything else - the seat accepted the grammar
        parameter and ignored it, which is worse than rejecting it.
  grammar-broken-on-chat-route  HTTP 500 - the gpt-oss class, where the chat
        route itself falls over on a grammar-constrained request.

An enforced grammar cannot answer 4. That is what makes this probe falsifiable
rather than a vibe check.`,
		Example: `  llamaswap-pp-cli gate grammar gemma-4-e2b
  llamaswap-pp-cli gate grammar gemma-4-e2b gpt-oss-20b --json`,
		Annotations: map[string]string{
			"pp:typed-exit-codes":  "3=model not in roster, 4=proxy unreachable, 27=upstream 5xx",
			"pp:measurement-owner": "wave-c",
		},
		RunE: gateRunE(flags, "grammar", gateProbeGrammar),
	}
	return cmd
}

func newMeasureGateToolsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools <model...>",
		Short: "Does this seat emit a well-formed tool call?",
		Long: `Sends a one-tool schema and a question that can only be answered by calling it,
then asserts the response carries a tool_call with the right name and
JSON-parseable arguments.

  PASS  a tool_call named get_weather with parseable arguments came back.
  FAIL  the model answered in prose, named a tool that does not exist, or
        emitted arguments that are not valid JSON.`,
		Example: `  llamaswap-pp-cli gate tools gemma-4-e2b --json`,
		Annotations: map[string]string{
			"pp:typed-exit-codes":  "3=model not in roster, 4=proxy unreachable, 27=upstream 5xx",
			"pp:measurement-owner": "wave-c",
		},
		RunE: gateRunE(flags, "tools", gateProbeTools),
	}
	return cmd
}

type gateProbe func(ctx context.Context, flags *rootFlags, model string, timeout time.Duration) gateVerdict

func gateRunE(flags *rootFlags, probeName string, probe gateProbe) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if flags.asJSON {
				if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"error": "requires at least one model name; there is no --all (each probe loads a model)",
					"usage": cmd.CommandPath() + " --help",
				}, flags); err != nil {
					return err
				}
				return usageErr(fmt.Errorf("%q requires at least one model name", cmd.CommandPath()))
			}
			return cmd.Help()
		}
		if dryRunOK(flags) {
			return writeDryRun(cmd.OutOrStdout(), flags, fmt.Sprintf("gate %s %v", probeName, args))
		}
		if err := validateDataSourceStrategy(flags, "live"); err != nil {
			return usageErr(err)
		}
		if handled, err := mcVerifyPlanOnly(cmd, flags, "gate "+probeName, map[string]any{
			"models": args,
			"note":   "capability probes load models and change GPU state; they never run under the verifier",
		}); handled {
			return err
		}

		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		timeout := mcTimeout(cmd, flags, 10*time.Minute)

		roster, err := mcRoster(ctx, flags, timeout)
		if err != nil {
			return mcClassify(err)
		}
		seats, err := mcRunning(ctx, flags, mcTimeout(cmd, flags, 15*time.Second))
		if err != nil {
			return mcClassify(err)
		}

		report := &gateReport{
			SchemaVersion: 1, Probe: probeName, RanAt: mcNow(),
			Route: "POST /v1/chat/completions (production route)",
		}
		failures := 0
		for _, requested := range args {
			model, known := mcResolveAlias(roster, requested)
			if !known {
				return mcModelNotFound(requested, roster)
			}
			_, loaded := mcFindSeat(seats, model)
			if !loaded {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s is not loaded; this probe will swap it in\n", model)
			}
			v := probe(ctx, flags, model, timeout)
			v.WasLoaded = loaded
			if requested != model {
				v.RequestedAs = requested
			}
			if !v.Pass {
				failures++
			}
			report.Verdicts = append(report.Verdicts, v)
		}
		if err := mcEmit(cmd, flags, report, func(w io.Writer) { gatePrint(w, report) }); err != nil {
			return err
		}
		if failures > 0 {
			return fmt.Errorf("%d of %d seats failed the %s probe", failures, len(report.Verdicts), probeName)
		}
		return nil
	}
}

func gateProbeGrammar(ctx context.Context, flags *rootFlags, model string, timeout time.Duration) gateVerdict {
	v := gateVerdict{Model: model, Probe: "grammar"}
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": gateGrammarQuestion},
		},
		"grammar":      gateGrammarGBNF,
		"max_tokens":   16,
		"temperature":  0,
		"stream":       false,
		"cache_prompt": false,
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	start := time.Now()
	err := mcPostJSON(ctx, flags, "/v1/chat/completions", body, timeout, &out)
	v.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		var he *mcHTTPError
		if As(err, &he) {
			v.HTTPStatus = he.Status
			if he.Status >= 500 {
				v.Verdict = "grammar-broken-on-chat-route"
				v.Detail = fmt.Sprintf("HTTP %d on a grammar-constrained chat request: the route itself fails, so this seat cannot be used with grammars (the gpt-oss class)", he.Status)
				return v
			}
		}
		v.Verdict = "probe-failed"
		v.Detail = err.Error()
		return v
	}
	reply := ""
	if len(out.Choices) > 0 {
		reply = strings.TrimSpace(out.Choices[0].Message.Content)
	}
	v.Reply = reply
	switch strings.ToLower(reply) {
	case "banana", "pineapple":
		v.Pass, v.Verdict = true, "grammar-enforced"
		v.Detail = "reply is inside the grammar, which admits only banana|pineapple"
	default:
		v.Verdict = "grammar-ignored"
		v.Detail = fmt.Sprintf("the grammar admits only banana|pineapple, but the seat replied %q; the grammar parameter was accepted and not enforced", reply)
	}
	return v
}

func gateProbeTools(ctx context.Context, flags *rootFlags, model string, timeout time.Duration) gateVerdict {
	v := gateVerdict{Model: model, Probe: "tools"}
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "What is the weather in Dubai right now? Use the tool."},
		},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Get the current weather for a city",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []string{"city"},
				},
			},
		}},
		"tool_choice":  "auto",
		"max_tokens":   128,
		"temperature":  0,
		"stream":       false,
		"cache_prompt": false,
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	start := time.Now()
	err := mcPostJSON(ctx, flags, "/v1/chat/completions", body, timeout, &out)
	v.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		var he *mcHTTPError
		if As(err, &he) {
			v.HTTPStatus = he.Status
			if he.Status >= 500 {
				v.Verdict = "tools-broken-on-chat-route"
				v.Detail = fmt.Sprintf("HTTP %d on a tool-enabled chat request", he.Status)
				return v
			}
		}
		v.Verdict = "probe-failed"
		v.Detail = err.Error()
		return v
	}
	if len(out.Choices) == 0 || len(out.Choices[0].Message.ToolCalls) == 0 {
		v.Verdict = "no-tool-call"
		if len(out.Choices) > 0 {
			v.Reply = strings.TrimSpace(out.Choices[0].Message.Content)
		}
		v.Detail = "the seat answered without emitting a tool_call"
		return v
	}
	call := out.Choices[0].Message.ToolCalls[0].Function
	v.ToolName, v.ToolArgs = call.Name, call.Arguments
	if call.Name != "get_weather" {
		v.Verdict = "wrong-tool"
		v.Detail = fmt.Sprintf("called %q, which is not the only tool offered", call.Name)
		return v
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &parsed); err != nil {
		v.Verdict = "malformed-arguments"
		v.Detail = "tool_call arguments are not valid JSON: " + err.Error()
		return v
	}
	v.Pass, v.Verdict = true, "tool-call-ok"
	v.Detail = "well-formed tool_call with parseable arguments"
	return v
}

func gatePrint(w io.Writer, r *gateReport) {
	fmt.Fprintf(w, "%s %s  (%s)\n", bold("gate"), r.Probe, r.Route)
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "MODEL\tRESULT\tVERDICT\tLATENCY\tDETAIL")
	for _, v := range r.Verdicts {
		result := red("FAIL")
		if v.Pass {
			result = green("PASS")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f ms\t%s\n", v.Model, result, v.Verdict, v.LatencyMS, truncate(v.Detail, 90))
	}
	tw.Flush()
	for _, v := range r.Verdicts {
		if v.Reply != "" {
			fmt.Fprintf(w, "  %s replied: %q\n", v.Model, truncate(v.Reply, 120))
		}
		if v.ToolName != "" {
			fmt.Fprintf(w, "  %s called: %s(%s)\n", v.Model, v.ToolName, truncate(v.ToolArgs, 120))
		}
	}
}
