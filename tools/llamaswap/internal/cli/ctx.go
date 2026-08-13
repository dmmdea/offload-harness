// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source live

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/gguf"
	"llamaswap-pp-cli/internal/measure"
	"llamaswap-pp-cli/internal/store"
)

// ctxProbeText is the built-in prompt used when neither --prompt-file nor
// --tokens is given. Fixed so repeated runs are comparable.
const ctxProbeText = "The quick brown fox jumps over the lazy dog. " +
	"Context budgeting only works with real tokens: a chars/4 estimate is about 2x off on Gemma tokenizers, " +
	"which is how a context audit gets retracted."

type ctxReport struct {
	SchemaVersion int    `json:"schema_version"`
	Model         string `json:"model"`
	RequestedAs   string `json:"requested_as,omitempty"`

	Loaded       bool   `json:"loaded"`
	NCtxLive     int    `json:"n_ctx_live"`
	NCtxSource   string `json:"n_ctx_source"`
	SeatCtxFlag  int    `json:"seat_ctx_flag,omitempty"`
	PromptLabel  string `json:"prompt_label"`
	RealTokens   int    `json:"real_tokens"`
	TokenSource  string `json:"token_source"`
	OutputBudget int    `json:"output_budget_tokens"`
	Room         int    `json:"room_tokens"`
	Verdict      string `json:"verdict"`

	TargetCtx  int                 `json:"target_ctx,omitempty"`
	TargetKV   *measure.KVEstimate `json:"kv_at_target_ctx,omitempty"`
	TargetKVGB float64             `json:"kv_at_target_ctx_gib,omitempty"`

	Notes    []string `json:"notes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

func newMeasureCtxCmd(flags *rootFlags) *cobra.Command {
	var (
		flagPromptFile string
		flagTokens     int
		flagTargetCtx  int
		flagOutBudget  int
		flagAllowLoad  bool
	)

	cmd := &cobra.Command{
		Use:   "ctx <model>",
		Short: "Real tokens vs the seat's live n_ctx: room left, and KV cost at a target context",
		Long: `Answers the two questions a context audit actually needs, with measurements
rather than estimates:

  real_tokens  POST /upstream/{model}/tokenize - the model's OWN tokenizer.
               A chars/4 estimate is ~2x off on Gemma and has already caused
               one retracted verdict on this box.
  n_ctx_live   GET /upstream/{model}/props - what the running process actually
               has, which is not always what the YAML says.

Both endpoints are AUTO-START endpoints: probing an unloaded model makes
llama-swap load it (multi-GB, evicts whatever is resident). So this command
refuses unless the model is already in /running. --allow-load opts in
explicitly.`,
		Example: `  llamaswap-pp-cli ctx embeddinggemma
  llamaswap-pp-cli ctx gemma-4-e4b --prompt-file ./transcript.txt --target-ctx 131072
  llamaswap-pp-cli ctx gemma-4-e4b --tokens 37128 --json`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:typed-exit-codes":  "3=model not loaded or not in roster, 4=proxy unreachable, 27=upstream 5xx",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if flags.asJSON {
					if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
						"error": "requires a model name",
						"usage": cmd.CommandPath() + " --help",
					}, flags); err != nil {
						return err
					}
					return usageErr(fmt.Errorf("%q requires a model name", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "ctx "+args[0])
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 60*time.Second)

			requested := args[0]
			report := &ctxReport{SchemaVersion: 1, OutputBudget: flagOutBudget, TargetCtx: flagTargetCtx}

			roster, err := mcRoster(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			model, known := mcResolveAlias(roster, requested)
			if !known {
				return mcModelNotFound(requested, roster)
			}
			report.Model = model
			if model != requested {
				report.RequestedAs = requested
			}

			seats, err := mcRunning(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			seat, loaded := mcFindSeat(seats, model)
			report.Loaded = loaded
			if !loaded {
				if !flagAllowLoad {
					return &cliError{code: ExitModelNotFound, err: fmt.Errorf(
						"%q is not loaded, and /upstream/{model}/tokenize + /props are AUTO-START endpoints: probing it would make llama-swap load the model "+
							"(multi-GB, evicting whatever is resident). Loaded right now: %s.\n"+
							"Either bench/chat it first, or re-run with --allow-load to accept that cost deliberately",
						model, mcJoinOrNone(mcLoadedNames(seats)))}
				}
				report.Warnings = append(report.Warnings,
					"--allow-load: this probe STARTED the model; VRAM and whatever it evicted changed as a side effect")
			}
			if loaded && !mcIsLlamaServer(seat.Cmd) {
				return usageErr(fmt.Errorf("seat %q is not a llama-server process (non-llama-server seat); tokenize/props context math does not apply", model))
			}

			// Real tokens.
			switch {
			case flagTokens > 0:
				report.RealTokens, report.TokenSource, report.PromptLabel = flagTokens, "--tokens flag (not tokenized)", "caller-supplied count"
			default:
				text := ctxProbeText
				report.PromptLabel = "builtin-probe"
				if flagPromptFile != "" {
					raw, err := os.ReadFile(flagPromptFile)
					if err != nil {
						return usageErr(fmt.Errorf("reading --prompt-file: %w", err))
					}
					text, report.PromptLabel = string(raw), flagPromptFile
				}
				var tok struct {
					Tokens []any `json:"tokens"`
				}
				if err := mcPostJSON(ctx, flags, "/upstream/"+model+"/tokenize", map[string]any{"content": text}, timeout, &tok); err != nil {
					return mcClassify(err)
				}
				report.RealTokens, report.TokenSource = len(tok.Tokens), "POST /upstream/"+model+"/tokenize (the model's own tokenizer)"
			}

			// Live n_ctx.
			var props struct {
				DefaultGenerationSettings struct {
					NCtx int `json:"n_ctx"`
				} `json:"default_generation_settings"`
				ModelPath string `json:"model_path"`
				BuildInfo string `json:"build_info"`
			}
			if err := mcGetJSON(ctx, flags, "/upstream/"+model+"/props", timeout, &props); err != nil {
				return mcClassify(err)
			}
			report.NCtxLive, report.NCtxSource = props.DefaultGenerationSettings.NCtx, "GET /upstream/"+model+"/props (live process)"
			if loaded {
				if c, ok := mcSeatCtx(seat.Cmd); ok {
					report.SeatCtxFlag = c
					if c != report.NCtxLive {
						report.Notes = append(report.Notes, fmt.Sprintf(
							"seat was started with -c %d but the live process reports n_ctx %d (llama.cpp adjusts for slots: n_ctx is divided across %s)",
							c, report.NCtxLive, "the seat's parallel slots"))
					}
				}
			}

			report.Room = report.NCtxLive - report.RealTokens - flagOutBudget
			switch {
			case report.NCtxLive == 0:
				report.Verdict = "unknown: the process reported no n_ctx"
			case report.RealTokens > report.NCtxLive:
				report.Verdict = fmt.Sprintf("OVERFLOW: prompt alone is %d tokens over n_ctx", report.RealTokens-report.NCtxLive)
			case report.Room < 0:
				report.Verdict = fmt.Sprintf("OVERFLOW with the %d-token output budget: %d tokens short", flagOutBudget, -report.Room)
			case report.Room < report.NCtxLive/10:
				report.Verdict = "TIGHT: under 10% of the window left after the output budget"
			default:
				report.Verdict = "fits"
			}

			// KV at a target context needs the weights file, which only a
			// loaded seat can point at without parsing the YAML.
			if flagTargetCtx > 0 {
				if !loaded {
					report.Warnings = append(report.Warnings, "--target-ctx needs the GGUF header, which is resolved from the loaded seat's -m path; skipped")
				} else if path, ok := mcSeatModelPath(seat.Cmd); ok {
					header, err := gguf.Read(path)
					if err != nil {
						report.Warnings = append(report.Warnings, "reading GGUF for the KV estimate: "+err.Error())
					} else if header.IsGGUF {
						k, v := mcSeatCacheTypes(seat.Cmd)
						ctK, errK := measure.ParseCacheType(k)
						ctV, errV := measure.ParseCacheType(v)
						if errK != nil || errV != nil {
							report.Warnings = append(report.Warnings, "unrecognized cache type on the seat; KV estimate skipped")
						} else if est, err := measure.EstimateKV(header, flagTargetCtx, ctK, ctV); err != nil {
							report.Warnings = append(report.Warnings, "KV estimate: "+err.Error())
						} else {
							report.TargetKV = &est
							report.TargetKVGB = est.TotalBytes / (1024 * 1024 * 1024)
						}
					}
				}
			}

			ctxRecord(ctx, report)
			return mcEmit(cmd, flags, report, func(w io.Writer) { ctxPrint(w, report) })
		},
	}
	cmd.Flags().StringVar(&flagPromptFile, "prompt-file", "", "Tokenize this file's contents instead of the built-in probe")
	cmd.Flags().IntVar(&flagTokens, "tokens", 0, "Use this token count instead of tokenizing (for auditing a recorded prompt size)")
	cmd.Flags().IntVar(&flagTargetCtx, "target-ctx", 0, "Also report the KV cache cost of serving this model at this context")
	cmd.Flags().IntVar(&flagOutBudget, "output-budget", 512, "Tokens reserved for the response when computing room")
	cmd.Flags().BoolVar(&flagAllowLoad, "allow-load", false, "Allow probing an unloaded model, accepting that it will be LOADED (multi-GB, evicts residents)")
	return cmd
}

func ctxRecord(ctx context.Context, r *ctxReport) {
	mcRecord(ctx, "ctx probe", func(s *store.Store) error {
		_, err := s.DB().ExecContext(ctx,
			`INSERT INTO ctx_probes (ts, model, n_ctx_live, prompt_label, real_tokens, room, verdict)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			mcNow(), r.Model, r.NCtxLive, r.PromptLabel, r.RealTokens, r.Room, r.Verdict)
		return err
	})
}

func ctxPrint(w io.Writer, r *ctxReport) {
	fmt.Fprintf(w, "%s  %s\n", bold("ctx"), r.Model)
	fmt.Fprintf(w, "  real tokens     %d  [%s, prompt: %s]\n", r.RealTokens, r.TokenSource, r.PromptLabel)
	fmt.Fprintf(w, "  n_ctx live      %d  [%s]\n", r.NCtxLive, r.NCtxSource)
	if r.SeatCtxFlag > 0 {
		fmt.Fprintf(w, "  seat -c flag    %d\n", r.SeatCtxFlag)
	}
	fmt.Fprintf(w, "  output budget   %d tokens\n", r.OutputBudget)
	fmt.Fprintf(w, "  room            %d tokens\n", r.Room)
	fmt.Fprintf(w, "  verdict         %s\n", r.Verdict)
	if r.TargetKV != nil {
		fmt.Fprintf(w, "  KV @ %d      %.2f GiB  [%s model, %d KV heads, %d full-attention layers]\n",
			r.TargetCtx, r.TargetKVGB, r.TargetKV.Model, r.TargetKV.HeadCountKV, r.TargetKV.LayersFull)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note            %s\n", n)
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "  %s %s\n", yellow("warning:"), warn)
	}
}
