// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source live

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/measure"
	"llamaswap-pp-cli/internal/store"
)

// benchPrompt is the fixed benchmark prompt. Fixed on purpose: prompt-processing
// throughput is meaningless across runs unless the prompt is byte-identical, and
// a prompt that changes between builds silently invalidates every comparison.
//
// Measured on this box (POST /upstream/gemma-4-e2b/tokenize, 2026-08-13):
// 311 tokens raw, and prompt_n = 320 once llama.cpp wraps it in the seat's
// chat template. Tokenizers differ per model, so treat these as the gemma-4
// figures, not a constant - which is exactly why prompt_n is recorded per run
// instead of assumed.
const benchPrompt = `You are reviewing an operations runbook for a local inference server that hosts several quantized language models behind a proxy which swaps them in and out of GPU memory on demand. The runbook covers six areas: how models are declared in the configuration file, how the proxy decides which model to evict when memory is short, how the embedding and reranking models are pinned so that they are never evicted, how benchmarks are recorded so that a result can always be traced back to the exact serving configuration that produced it, how context length is budgeted against the key-value cache, and how a failed model load is diagnosed from the proxy logs. For each area, list the specific failure mode that the runbook is designed to prevent, name the single measurement that would prove the prevention is working, and state plainly what an operator should do when that measurement disagrees with the runbook. Be concrete and avoid generalities: name the endpoint, the flag, or the log line that carries the evidence in each case, and say which of the six areas is the most likely to be wrong in practice. Then, assuming the operator has exactly one hour and a single GPU that is already two thirds full, put the six areas in the order you would work through them, and justify the ordering by what each step unblocks rather than by how quickly it can be finished. Finish with the one sentence you would put at the top of the runbook so that a reader who stops after that sentence still knows which measurement to trust when the documentation and the running process disagree with each other.`

// benchTimings is llama.cpp's own timing block. It is the ONLY accepted
// source of a rate here: deriving tokens/second from a progress bar sample or
// from wall-clock alone has already produced one false regression report.
type benchTimings struct {
	PromptN            int     `json:"prompt_n"`
	PromptMS           float64 `json:"prompt_ms"`
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedN         int     `json:"predicted_n"`
	PredictedMS        float64 `json:"predicted_ms"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
}

type benchChatResponse struct {
	Timings *benchTimings `json:"timings"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type benchRun struct {
	Index      int      `json:"index"`
	WallMS     float64  `json:"wall_ms"`
	PromptN    int      `json:"prompt_n"`
	PromptMS   float64  `json:"prompt_ms"`
	PPPerSec   float64  `json:"pp_tokens_per_second"`
	PredictedN int      `json:"predicted_n"`
	PredictMS  float64  `json:"predicted_ms"`
	TGPerSec   float64  `json:"tg_tokens_per_second"`
	Source     string   `json:"rate_source"`
	Error      string   `json:"error,omitempty"`
	ColdLoadMS *float64 `json:"cold_load_ms,omitempty"`
}

type benchModelResult struct {
	Model        string `json:"model"`
	RequestedAs  string `json:"requested_as,omitempty"`
	WasLoaded    bool   `json:"was_loaded_before"`
	SwappedIn    bool   `json:"swapped_in_by_this_bench"`
	ConfigSHA    string `json:"config_sha256"`
	ServingCmd   string `json:"serving_cmd"`
	SwapVersion  string `json:"llamaswap_version"`
	BuildInfo    string `json:"build_info"`
	CleanState   bool   `json:"clean_state"`
	Runs         int    `json:"runs"`
	MaxTokens    int    `json:"max_tokens"`
	Concurrency  int    `json:"concurrency"`
	CachePrompt  bool   `json:"cache_prompt"`
	PromptTokens int    `json:"prompt_tokens_measured"`

	PPMedian float64 `json:"pp_median_tokens_per_second"`
	TGMedian float64 `json:"tg_median_tokens_per_second"`
	TGMax    float64 `json:"tg_max_tokens_per_second"`

	ColdLoadMS *float64         `json:"cold_load_ms,omitempty"`
	VRAM       []measure.Delta  `json:"vram_delta_per_uuid,omitempty"`
	VRAMTotal  int              `json:"vram_delta_total_mib"`
	Samples    []benchRun       `json:"samples"`
	Warnings   []string         `json:"warnings,omitempty"`
	Notes      []string         `json:"notes,omitempty"`
	Timings    benchTimingsMeta `json:"timing_provenance"`
}

type benchTimingsMeta struct {
	Source string `json:"source"`
	Note   string `json:"note"`
}

type benchReport struct {
	SchemaVersion int                `json:"schema_version"`
	StartedAt     string             `json:"started_at"`
	Route         string             `json:"route"`
	Models        []benchModelResult `json:"models"`
}

func newMeasureBenchCmd(flags *rootFlags) *cobra.Command {
	var (
		flagRuns        int
		flagMaxTokens   int
		flagConcurrency int
	)

	cmd := &cobra.Command{
		Use:   "bench <model...>",
		Short: "Benchmark seats through the production route, with the serving config identity attached",
		Long: `Benchmarks each model through POST /v1/chat/completions - the SAME route real
traffic takes - with "cache_prompt": false in the body, and reads rates from
llama.cpp's own timings object.

Three traps are encoded here because each one has already produced a wrong
number on this box:

  1. benching a side route measures a path nothing uses;
  2. leaving prompt caching on collapses prompt_n to ~4 and reports a fantasy
     prompt-processing rate - this command aborts when it sees that;
  3. deriving a rate from a progress-bar sample measures one instant, not the
     run - only the response's timings object is accepted.

Every result records the sha256 of the seat's live command line, the llama-swap
version, and the llama.cpp build_info read from the loaded process. A benchmark
without its serving-config identity is an anecdote.

Models are benched one at a time, never interleaved. Benching a model that is
not currently loaded WILL swap it in.`,
		Example: `  llamaswap-pp-cli bench gemma-4-e2b --runs 2 --max-tokens 128
  llamaswap-pp-cli bench gemma-4-e2b gemma-4-e4b --runs 3 --json
  llamaswap-pp-cli bench aux --embed --rerank`,
		Annotations: map[string]string{
			"pp:typed-exit-codes":  "3=model not in roster, 4=proxy unreachable, 27=upstream 5xx",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if flags.asJSON {
					if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
						"error": "requires at least one model name (or the 'aux' subcommand)",
						"usage": cmd.CommandPath() + " --help",
					}, flags); err != nil {
						return err
					}
					return usageErr(fmt.Errorf("%q requires at least one model name", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, fmt.Sprintf("bench %v (%d runs, %d max tokens)", args, flagRuns, flagMaxTokens))
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			if handled, err := mcVerifyPlanOnly(cmd, flags, "bench", map[string]any{
				"models": args, "runs": flagRuns, "max_tokens": flagMaxTokens,
				"note": "bench loads models and changes GPU state; it never runs under the verifier",
			}); handled {
				return err
			}
			if flagRuns < 1 {
				return usageErr(fmt.Errorf("--runs must be at least 1"))
			}
			if flagConcurrency < 1 {
				return usageErr(fmt.Errorf("--concurrency must be at least 1"))
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 10*time.Minute)

			roster, err := mcRoster(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			report := &benchReport{SchemaVersion: 1, StartedAt: mcNow(), Route: "POST /v1/chat/completions (production route)"}

			for _, requested := range args {
				model, known := mcResolveAlias(roster, requested)
				if !known {
					return mcModelNotFound(requested, roster)
				}
				res, err := benchOne(ctx, cmd, flags, model, requested, flagRuns, flagMaxTokens, flagConcurrency, timeout)
				if err != nil {
					return err
				}
				report.Models = append(report.Models, *res)
			}
			return mcEmit(cmd, flags, report, func(w io.Writer) { benchPrint(w, report) })
		},
	}
	cmd.Flags().IntVar(&flagRuns, "runs", 3, "Requests per model (medians are taken across these)")
	cmd.Flags().IntVar(&flagMaxTokens, "max-tokens", 256, "Tokens to generate per request")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", 1, "Requests in flight at once per iteration")
	addNovelCommandIfAbsent(cmd, newMeasureBenchAuxCmd(flags))
	return cmd
}

func benchOne(ctx context.Context, cmd *cobra.Command, flags *rootFlags, model, requested string,
	runs, maxTokens, concurrency int, timeout time.Duration) (*benchModelResult, error) {

	res := &benchModelResult{
		Model: model, Runs: runs, MaxTokens: maxTokens, Concurrency: concurrency,
		CachePrompt: false,
	}
	if requested != model {
		res.RequestedAs = requested
	}

	seatsBefore, err := mcRunning(ctx, flags, mcTimeout(cmd, flags, 15*time.Second))
	if err != nil {
		return nil, mcClassify(err)
	}
	_, res.WasLoaded = mcFindSeat(seatsBefore, model)
	res.CleanState = len(seatsBefore) == 0
	if !res.WasLoaded {
		res.SwappedIn = true
		fmt.Fprintf(os.Stderr, "warning: %s is NOT loaded; benching it will swap it in (a multi-GB load that may evict what is resident now: %s)\n",
			model, mcJoinOrNone(mcLoadedNames(seatsBefore)))
		res.Warnings = append(res.Warnings, "model was not loaded before this bench; the first request paid a cold load")
	}
	if !res.CleanState {
		res.Notes = append(res.Notes, fmt.Sprintf("not a clean-state run: %s already resident, so VRAM deltas are relative to that", mcJoinOrNone(mcLoadedNames(seatsBefore))))
	}

	extras := mcLoadExtras(flags)
	var samples []benchRun
	deltas, err := measure.DeltaAround(ctx, extras.GPURoles, func() error {
		idx := 0
		for r := 0; r < runs; r++ {
			batch := make([]benchRun, concurrency)
			var wg sync.WaitGroup
			for c := 0; c < concurrency; c++ {
				wg.Add(1)
				go func(slot int) {
					defer wg.Done()
					batch[slot] = benchRequest(ctx, flags, model, maxTokens, timeout)
				}(c)
			}
			wg.Wait()
			for _, b := range batch {
				b.Index = idx
				idx++
				samples = append(samples, b)
			}
		}
		return nil
	})
	if err != nil {
		// nvidia-smi missing must not sink a benchmark; the rates are still real.
		res.Warnings = append(res.Warnings, "VRAM not measured: "+err.Error())
	}
	res.VRAM = deltas
	res.VRAMTotal = measure.TotalDelta(deltas)

	// Cold load: the first request's wall clock minus the work llama.cpp
	// reports doing. Only meaningful when this bench triggered the load.
	if len(samples) > 0 && res.SwappedIn && samples[0].Error == "" {
		cold := samples[0].WallMS - (samples[0].PromptMS + samples[0].PredictMS)
		if cold > 0 {
			res.ColdLoadMS = &cold
			samples[0].ColdLoadMS = &cold
		}
	}
	res.Samples = samples

	var pp, tg []float64
	var failures int
	for _, s := range samples {
		if s.Error != "" {
			failures++
			continue
		}
		if s.PPPerSec > 0 {
			pp = append(pp, s.PPPerSec)
		}
		if s.TGPerSec > 0 {
			tg = append(tg, s.TGPerSec)
		}
		if res.PromptTokens == 0 {
			res.PromptTokens = s.PromptN
		}
		if s.TGPerSec > res.TGMax {
			res.TGMax = s.TGPerSec
		}
	}
	if failures == len(samples) && len(samples) > 0 {
		return nil, apiErr(fmt.Errorf("every bench request against %q failed; first error: %s", model, samples[0].Error))
	}
	res.PPMedian, res.TGMedian = median(pp), median(tg)

	// prompt_n sanity. The fixed prompt is ~300 tokens; a prompt_n that
	// collapses to a handful means the prompt cache served it and the
	// prompt-processing rate is fiction.
	if res.PromptTokens > 0 && res.PromptTokens < 64 {
		return nil, fmt.Errorf(
			"ABORTED: prompt_n collapsed to %d for %q. The fixed benchmark prompt is ~300 tokens, so a single-digit prompt_n means "+
				"prompt caching served the prompt and the prompt-processing rate would be fiction. "+
				"This bench sends \"cache_prompt\": false; a collapsed prompt_n means the server ignored it "+
				"(check the seat for --cache-reuse / a proxy that rewrites the body) - fix that before trusting any PP number",
			res.PromptTokens, model)
	}

	res.Timings = benchTimingsMeta{
		Source: "llama.cpp timings object in the chat-completions response",
		Note:   "rates are prompt_per_second / predicted_per_second as reported by the server, never derived from wall clock or progress samples",
	}
	for _, s := range samples {
		if s.Source != "" && s.Source != "timings" {
			res.Timings.Source = s.Source
			res.Warnings = append(res.Warnings, "the server returned no timings object; rates were derived from usage + wall clock and are NOT comparable to timings-based rows")
			break
		}
	}

	// Config identity, read AFTER the load so it describes the process that
	// actually served these tokens.
	short := mcTimeout(cmd, flags, 30*time.Second)
	if seatsAfter, err := mcRunning(ctx, flags, short); err == nil {
		if seat, ok := mcFindSeat(seatsAfter, model); ok {
			res.ServingCmd = seat.Cmd
			res.ConfigSHA = mcSHA256(seat.Cmd)
		} else {
			res.Warnings = append(res.Warnings, "model was no longer in /running after the bench; serving-config identity is unavailable (was it evicted mid-run?)")
		}
	}
	var version struct {
		Version string `json:"version"`
	}
	if err := mcGetJSON(ctx, flags, "/api/version", short, &version); err == nil {
		res.SwapVersion = version.Version
	}
	if res.ServingCmd != "" {
		var props struct {
			BuildInfo string `json:"build_info"`
		}
		if err := mcGetJSON(ctx, flags, "/upstream/"+model+"/props", short, &props); err == nil {
			res.BuildInfo = props.BuildInfo
		}
	}
	if res.ConfigSHA == "" || res.BuildInfo == "" {
		res.Warnings = append(res.Warnings, "incomplete serving-config identity (config_sha/build_info): this row cannot be joined to a future comparison with confidence")
	}

	benchRecord(ctx, res)
	vramRecordDeltas(ctx, model, 0, deltas)
	return res, nil
}

// benchRequest issues one production-route request and reads the rates out of
// llama.cpp's timings block.
func benchRequest(ctx context.Context, flags *rootFlags, model string, maxTokens int, timeout time.Duration) benchRun {
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": benchPrompt},
		},
		"max_tokens": maxTokens,
		"stream":     false,
		// The trap: with prompt caching on, a repeated identical prompt is
		// served from cache and prompt_n collapses to a handful of tokens.
		"cache_prompt": false,
		"temperature":  0,
	}
	var out benchChatResponse
	start := time.Now()
	err := mcPostJSON(ctx, flags, "/v1/chat/completions", body, timeout, &out)
	wall := float64(time.Since(start).Microseconds()) / 1000.0

	run := benchRun{WallMS: wall}
	if err != nil {
		run.Error = err.Error()
		return run
	}
	if out.Timings != nil {
		t := out.Timings
		run.PromptN, run.PromptMS, run.PPPerSec = t.PromptN, t.PromptMS, t.PromptPerSecond
		run.PredictedN, run.PredictMS, run.TGPerSec = t.PredictedN, t.PredictedMS, t.PredictedPerSecond
		run.Source = "timings"
		if run.PPPerSec == 0 && t.PromptMS > 0 {
			run.PPPerSec = float64(t.PromptN) / (t.PromptMS / 1000)
		}
		if run.TGPerSec == 0 && t.PredictedMS > 0 {
			run.TGPerSec = float64(t.PredictedN) / (t.PredictedMS / 1000)
		}
		return run
	}
	// Degraded path, explicitly labeled: no timings object in the response.
	run.Source = "wall-clock fallback (NO timings object in the response)"
	run.PromptN, run.PredictedN = out.Usage.PromptTokens, out.Usage.CompletionTokens
	if wall > 0 && run.PredictedN > 0 {
		run.TGPerSec = float64(run.PredictedN) / (wall / 1000)
	}
	return run
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

func benchRecord(ctx context.Context, r *benchModelResult) {
	if cliutil.IsVerifyEnv() {
		return
	}
	mcRecord(ctx, "bench run", func(s *store.Store) error {
		var cold any
		if r.ColdLoadMS != nil {
			cold = int(*r.ColdLoadMS)
		}
		// Attribute the row to the card that actually took the allocation.
		// When nothing moved (the model was already resident), the row gets
		// NO gpu attribution: guessing one would put a benchmark on a card
		// that never held the model.
		var gpuUUID any
		if len(r.VRAM) > 0 {
			best := r.VRAM[0]
			for _, d := range r.VRAM {
				if d.DeltaMiB > best.DeltaMiB {
					best = d
				}
			}
			if best.DeltaMiB > 0 {
				gpuUUID = best.UUID
			}
		}
		clean := 0
		if r.CleanState {
			clean = 1
		}
		_, err := s.DB().ExecContext(ctx,
			`INSERT INTO bench_runs
			 (ts, model, config_sha, llamaswap_version, build_info, pp_median, tg_median, tg_max,
			  prompt_n, cold_load_ms, vram_delta_mib, gpu_uuid, clean_state, runs, max_tokens, concurrency, notes)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			mcNow(), r.Model, r.ConfigSHA, r.SwapVersion, r.BuildInfo,
			r.PPMedian, r.TGMedian, r.TGMax, r.PromptTokens, cold, r.VRAMTotal, gpuUUID,
			clean, r.Runs, r.MaxTokens, r.Concurrency, r.Timings.Source)
		return err
	})
}

func benchPrint(w io.Writer, r *benchReport) {
	fmt.Fprintf(w, "%s  %s\n", bold("bench"), r.Route)
	for _, m := range r.Models {
		fmt.Fprintf(w, "\n%s\n", bold(m.Model))
		fmt.Fprintf(w, "  PP median       %.1f tok/s  (prompt_n %d, cache_prompt=false)\n", m.PPMedian, m.PromptTokens)
		fmt.Fprintf(w, "  TG median       %.1f tok/s  (max %.1f)\n", m.TGMedian, m.TGMax)
		if m.ColdLoadMS != nil {
			fmt.Fprintf(w, "  cold load       %.0f ms  (first-request wall minus prompt+predicted)\n", *m.ColdLoadMS)
		}
		if len(m.VRAM) > 0 {
			for _, d := range m.VRAM {
				label := d.Role
				if label == "" {
					label = d.Name
				}
				fmt.Fprintf(w, "  VRAM %-10s %+d MiB  (%d -> %d MiB, %s)\n",
					label, d.DeltaMiB, d.BaselineMiB, d.AfterMiB, measure.ShortUUID(d.UUID))
			}
			fmt.Fprintf(w, "  VRAM total      %+d MiB\n", m.VRAMTotal)
		}
		fmt.Fprintf(w, "  config sha      %s\n", short12(m.ConfigSHA))
		fmt.Fprintf(w, "  llama-swap      %s   llama.cpp build %s\n", m.SwapVersion, m.BuildInfo)
		fmt.Fprintf(w, "  runs            %d x %d tokens, concurrency %d\n", m.Runs, m.MaxTokens, m.Concurrency)
		for _, n := range m.Notes {
			fmt.Fprintf(w, "  note            %s\n", n)
		}
		for _, warn := range m.Warnings {
			fmt.Fprintf(w, "  %s %s\n", yellow("warning:"), warn)
		}
	}
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
