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

	// Shards is set when the target was one member of a split set and every
	// sibling was summed into the weights figure.
	Shards *gguf.ShardSet `json:"shards,omitempty"`

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

Four header facts make fit REFUSE (exit 28) instead of answering, because each
one turns the standard formula into a confident wrong number:

  sharded model   a shard header describes a FRACTION of the weights. fit sums
                  every sibling shard when the whole set is on disk, and
                  refuses when any member is missing.
  MLA             compressed latent KV (attention.kv_lora_rank / key_length_mla)
                  is not n_kv_heads x head_dim x 2.
  SSM / Mamba     a fixed-size recurrent state does not grow with context.
  non-model file  adapter / imatrix / mmproj (general.type) have no servable
                  weights and no KV cache.

fit never loads anything.`,
		Example: `  llamaswap-pp-cli fit V:/models/gemma-4-E4B-it-qat-GGUF/gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf --ctx 131072
  llamaswap-pp-cli fit embeddinggemma --json
  llamaswap-pp-cli fit gemma-4-e4b --ctx 32768 --cache-type-k q8_0`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:typed-exit-codes":  "3=model not loaded / file not found, 4=proxy unreachable, 28=refused (verdict inside the uncertainty band, incomplete shard set, MLA/SSM architecture, or a non-model GGUF)",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if wantsAgentErrorEnvelope(flags) {
					return usageEnvelopeErr(flags, fmt.Errorf("%q requires a path or a loaded model name", cmd.CommandPath()))
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
			report.Notes = append(report.Notes, ggufHeaderNotes(header)...)

			// Guard 1: this file is not a model. An adapter/imatrix/mmproj has
			// no KV cache and no servable weights; sizing it produces a number
			// for something that is never loaded on its own.
			if !header.IsModel {
				return &cliError{code: ExitFitRefusal, err: fmt.Errorf(
					"REFUSING to size %s: %s.\nfit answers 'will this model fit'; this file is not a model, so there is no honest answer to give",
					path, header.NotAModelReason)}
			}

			// Guard 2: MLA / SSM. The standard
			// n_kv_heads x head_dim x 2 x n_layers x ctx formula does not
			// describe a compressed latent cache or a fixed recurrent state.
			// Applying it anyway yields a confident wrong number, which is
			// strictly worse than no number.
			if header.UnsupportedKVArch != "" {
				return &cliError{code: ExitFitRefusal, err: fmt.Errorf(
					"architecture-specific KV/state math not supported - refusing to guess.\n"+
						"%s declares %s (keys: %s). This CLI's KV estimator implements grouped-query attention with "+
						"sliding-window and shared-KV geometry; it has no model of this architecture's cache, and the "+
						"standard formula would be wrong by an unknown factor rather than by a margin.\n"+
						"To settle it: load the seat once and read the measured per-UUID delta from `llamaswap-pp-cli bench %s --runs 1`",
					filepath.Base(path), header.UnsupportedKVArch, strings.Join(header.UnsupportedKVKeys, ", "), target)}
			}

			// Guard 3: shards. A shard header describes a fraction of the
			// weights, so an unsummed verdict under-reports by ~the shard
			// count - which flips does-not-fit into fits.
			weightsBytes := header.FileSizeBytes
			if header.Split.IsShard() {
				set, serr := gguf.ResolveShards(path, header.Split.Count)
				if serr != nil || set == nil || !set.Complete {
					missing := "the sibling shards could not be located"
					if set != nil && len(set.Missing) > 0 {
						missing = fmt.Sprintf("shards %v are absent from %s", set.Missing, filepath.Dir(path))
					} else if serr != nil {
						missing = serr.Error()
					}
					return &cliError{code: ExitFitRefusal, err: fmt.Errorf(
						"REFUSING to answer: %s is %s, and %s.\n"+
							"This shard's %s is a FRACTION of the model's weights; judging capacity against it would under-report "+
							"by roughly the shard count and turn a does-not-fit into a fits. Put every shard in one directory and re-run",
						filepath.Base(path), header.Split.Summary, missing, mcFmtMiB(int(header.FileSizeBytes/(1024*1024))))}
				}
				weightsBytes = set.TotalBytes
				report.Shards = set
				report.Weights = fmt.Sprintf("summed over all %d shards (%s); this shard alone is %s",
					set.Count, mcFmtMiB(int(set.TotalBytes/(1024*1024))), mcFmtMiB(int(header.FileSizeBytes/(1024*1024))))
				report.Notes = append(report.Notes, fmt.Sprintf(
					"weights summed across the complete %d-shard set; the per-shard header would have under-reported them", set.Count))
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
			report.Fit = measure.Fit(weightsBytes, kv, gpus, flagMargin, flagReserve)
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
