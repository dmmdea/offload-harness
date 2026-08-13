// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source auto

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/gguf"
	"llamaswap-pp-cli/internal/measure"
)

type fitReport struct {
	SchemaVersion int    `json:"schema_version"`
	Target        string `json:"target"`
	GGUFPath      string `json:"gguf_path"`
	ResolvedFrom  string `json:"resolved_from"`

	CtxTokens  int    `json:"ctx_tokens"`
	CtxSource  string `json:"ctx_source"`
	Weights    string `json:"weights_source"`
	CacheKFrom string `json:"cache_type_k_source"`
	CacheVFrom string `json:"cache_type_v_source"`

	KV  measure.KVEstimate `json:"kv"`
	Fit measure.FitResult  `json:"fit"`

	Warnings []string `json:"warnings,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

func newMeasureFitCmd(flags *rootFlags) *cobra.Command {
	var (
		flagCtx     int
		flagCacheK  string
		flagCacheV  string
		flagMargin  int
		flagReserve int
	)

	cmd := &cobra.Command{
		Use:   "fit <loaded-model|gguf-path>",
		Short: "Will this model at this context fit the cards, as an interval with a refuse-to-answer band",
		Long: `Joins three things nothing else on this box joins: the GGUF header (weights,
layer count, GQA KV heads, per-head K/V length, sliding-window geometry), the
serving flags (context size, KV cache dtype), and the measured free VRAM per
GPU UUID.

The answer is an INTERVAL, never a point estimate. The optimistic end is
weights + KV cache; the pessimistic end adds an activation and CUDA-context
allowance. When the interval straddles a card's capacity the command REFUSES
to answer (exit 28) and names the measurement that would settle it.

Context comes from --ctx, else the loaded seat's -c flag, else the GGUF's own
context_length. There is no 4096 default: an unknown context is reported as
unknown.

fit never loads anything.`,
		Example: `  llamaswap-pp-cli fit V:/models/gemma-4-E4B-it-qat-GGUF/gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf --ctx 131072
  llamaswap-pp-cli fit embeddinggemma --json
  llamaswap-pp-cli fit gemma-4-e4b --ctx 32768 --cache-type-k q8_0`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:typed-exit-codes":  "3=model not loaded / file not found, 4=proxy unreachable, 28=verdict inside the uncertainty band (refused)",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if flags.asJSON {
					if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
						"error": "requires a GGUF path or a loaded model name",
						"usage": cmd.CommandPath() + " --help",
					}, flags); err != nil {
						return err
					}
					return usageErr(fmt.Errorf("%q requires a path or a loaded model name", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "fit "+args[0])
			}
			// auto: a path plus nvidia-smi is a local read; a model name needs
			// the live /running list to resolve its weights path and flags.
			if flags != nil && flags.dataSource == "local" && !mcLooksLikePath(args[0]) {
				return usageErr(fmt.Errorf("--data-source local requires a .gguf path; resolving the model name %q needs the live /running list", args[0]))
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 15*time.Second)

			target := args[0]
			report := &fitReport{SchemaVersion: 1, Target: target, Weights: "GGUF file size"}

			var seatCmd string
			path := target
			if mcLooksLikePath(target) {
				if abs, err := filepath.Abs(target); err == nil {
					path = abs
				}
				report.ResolvedFrom = "filesystem path"
			} else {
				seats, err := mcRunning(ctx, flags, timeout)
				if err != nil {
					return mcClassify(err)
				}
				name := target
				if roster, rErr := mcRoster(ctx, flags, timeout); rErr == nil {
					if resolved, ok := mcResolveAlias(roster, target); ok {
						name = resolved
					}
				}
				seat, ok := mcFindSeat(seats, name)
				if !ok {
					return &cliError{code: ExitModelNotFound, err: fmt.Errorf(
						"%q is not a file and is not loaded right now (loaded: %s)\n"+
							"pass the .gguf path directly, or load the seat first - fit never starts a model",
						target, mcJoinOrNone(mcLoadedNames(seats)))}
				}
				if !mcIsLlamaServer(seat.Cmd) {
					return usageErr(fmt.Errorf("seat %q is not a llama-server process (non-llama-server seat); KV fit math does not apply to it", name))
				}
				p, ok := mcSeatModelPath(seat.Cmd)
				if !ok {
					return notFoundErr(fmt.Errorf("loaded seat %q has no -m/--model in its command line", name))
				}
				path, seatCmd = p, seat.Cmd
				report.ResolvedFrom = fmt.Sprintf("live /running seat %q", name)
			}
			report.GGUFPath = path

			if _, err := os.Stat(path); err != nil {
				return notFoundErr(fmt.Errorf("cannot read %s: %w", path, err))
			}
			header, err := gguf.Read(path)
			if err != nil {
				return err
			}
			if !header.IsGGUF {
				return usageErr(fmt.Errorf("%s is not a GGUF file (%s); fit math does not apply. %s",
					path, header.NotGGUFReason, gguf.KnownNonGGUF(header.MagicSeen)))
			}

			// Context resolution, in strict precedence order. No default.
			ctxTokens := 0
			switch {
			case flagCtx > 0:
				ctxTokens, report.CtxSource = flagCtx, "--ctx flag"
			case seatCmd != "":
				if c, ok := mcSeatCtx(seatCmd); ok {
					ctxTokens, report.CtxSource = c, "live seat -c/--ctx-size"
				}
			}
			if ctxTokens == 0 && header.ContextLength > 0 {
				ctxTokens = header.ContextLength
				report.CtxSource = "GGUF context_length (the model's native maximum, not necessarily how it is served)"
			}
			if ctxTokens == 0 {
				return &cliError{code: ExitFitRefusal, err: fmt.Errorf(
					"context length is UNKNOWN for %s: no --ctx flag, no loaded seat to read -c from, and the GGUF declares no context_length. "+
						"Pass --ctx N. (There is no 4096 default here: a guessed context is a guessed KV size.)", path)}
			}

			// Cache dtypes: the live command line wins when there is one.
			cacheK, cacheV := "", ""
			report.CacheKFrom, report.CacheVFrom = "default (f16, no --cache-type-k on the seat)", "default (f16, no --cache-type-v on the seat)"
			if seatCmd != "" {
				if k, v := mcSeatCacheTypes(seatCmd); k != "" || v != "" {
					if k != "" {
						cacheK, report.CacheKFrom = k, "live seat --cache-type-k"
					}
					if v != "" {
						cacheV, report.CacheVFrom = v, "live seat --cache-type-v"
					}
				}
			}
			if flagCacheK != "" {
				cacheK, report.CacheKFrom = flagCacheK, "--cache-type-k flag"
			}
			if flagCacheV != "" {
				cacheV, report.CacheVFrom = flagCacheV, "--cache-type-v flag"
			}
			ctK, err := measure.ParseCacheType(cacheK)
			if err != nil {
				return usageErr(err)
			}
			ctV, err := measure.ParseCacheType(cacheV)
			if err != nil {
				return usageErr(err)
			}

			kv, err := measure.EstimateKV(header, ctxTokens, ctK, ctV)
			if err != nil {
				return &cliError{code: ExitFitRefusal, err: err}
			}
			report.CtxTokens = ctxTokens
			report.KV = kv
			report.Warnings = append(report.Warnings, kv.Warnings...)

			gpus, err := mcGPUs(ctx, flags)
			if err != nil {
				report.Warnings = append(report.Warnings, "nvidia-smi unavailable: capacity unknown, so no fit verdict can be given: "+err.Error())
			}
			report.Fit = measure.Fit(header.FileSizeBytes, kv, gpus, flagMargin, flagReserve)
			report.Notes = append(report.Notes,
				fmt.Sprintf("capacity per card = total - resident now (%d MiB reserve held back); resident includes the keep-set, so this is what is actually free",
					flagReserve))
			if kv.ContractDenseMiB > 0 && kv.Model == "swa-aware" {
				report.Notes = append(report.Notes, fmt.Sprintf(
					"a dense every-layer-full-context formula would claim %s of KV here; the header's sliding-window and shared-KV geometry says %s",
					mcFmtMiB(kv.ContractDenseMiB), mcFmtMiB(kv.TotalMiB)))
			}

			if err := mcEmit(cmd, flags, report, func(w io.Writer) { fitPrint(w, report) }); err != nil {
				return err
			}
			if report.Fit.Verdict == measure.VerdictUncertain {
				return &cliError{code: ExitFitRefusal, err: fmt.Errorf(
					"REFUSING to answer: the %s-%s MiB interval straddles capacity. %s",
					mcFmtMiB(report.Fit.OptimisticMiB), mcFmtMiB(report.Fit.PessimisticMiB), report.Fit.Settle)}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&flagCtx, "ctx", 0, "Context length in tokens (default: the loaded seat's -c, else the GGUF's context_length)")
	cmd.Flags().StringVar(&flagCacheK, "cache-type-k", "", "KV cache K dtype: f16, q8_0, ... (default: the seat's flag, else f16)")
	cmd.Flags().StringVar(&flagCacheV, "cache-type-v", "", "KV cache V dtype: f16, q8_0, ... (default: the seat's flag, else f16)")
	cmd.Flags().IntVar(&flagMargin, "margin-mib", measure.DefaultMarginMiB, "Activation + CUDA-context allowance added to the pessimistic end")
	cmd.Flags().IntVar(&flagReserve, "reserve-mib", measure.DefaultReserveMiB, "Headroom held back from every card before judging capacity")
	return cmd
}

func fitPrint(w io.Writer, r *fitReport) {
	fmt.Fprintf(w, "%s  %s\n", bold("fit"), r.Target)
	fmt.Fprintf(w, "  weights         %s  (%s)\n", mcFmtMiB(r.Fit.WeightsMiB), r.GGUFPath)
	fmt.Fprintf(w, "  context         %d tokens  [%s]\n", r.CtxTokens, r.CtxSource)
	fmt.Fprintf(w, "  KV cache        %s  [%s model, k=%s v=%s]\n",
		mcFmtMiB(r.KV.TotalMiB), r.KV.Model, r.KV.CacheTypeK.Name, r.KV.CacheTypeV.Name)
	fmt.Fprintf(w, "                  %d KV heads x %d/%d K/V length x %d full-attention layers",
		r.KV.HeadCountKV, r.KV.KeyLength, r.KV.ValueLength, r.KV.LayersFull)
	if r.KV.LayersSWA > 0 {
		fmt.Fprintf(w, " (+%d sliding-window layers capped at %d tokens)", r.KV.LayersSWA, r.KV.SWAWindow)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  interval        %s .. %s   (optimistic .. +%d MiB activation/CUDA margin)\n",
		mcFmtMiB(r.Fit.OptimisticMiB), mcFmtMiB(r.Fit.PessimisticMiB), r.Fit.MarginMiB)
	fmt.Fprintln(w)
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "CARD\tUUID\tTOTAL\tRESIDENT\tCAPACITY\tVERDICT")
	for _, c := range r.Fit.Cards {
		label := c.Role
		if label == "" {
			label = c.Name
		}
		fmt.Fprintf(tw, "%s\t%s\t%d MiB\t%d MiB\t%d MiB\t%s\n",
			label, measure.ShortUUID(c.UUID), c.TotalMiB, c.ResidentMiB, c.CapacityMiB, fitColor(c.Verdict))
	}
	tw.Flush()
	fmt.Fprintf(w, "\n  verdict         %s\n", fitColor(r.Fit.Verdict))
	if r.Fit.Settle != "" {
		fmt.Fprintf(w, "  to settle it    %s\n", r.Fit.Settle)
	}
	for _, a := range r.KV.Assumptions {
		fmt.Fprintf(w, "  assumption      %s\n", a)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note            %s\n", n)
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "  %s %s\n", yellow("warning:"), strings.TrimSpace(warn))
	}
}

func fitColor(v string) string {
	switch v {
	case measure.VerdictFits:
		return green(v)
	case measure.VerdictNoFit:
		return red(v)
	default:
		return yellow(v)
	}
}
