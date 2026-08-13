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
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/gguf"
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
//
// CacheN is the prompt prefix llama.cpp served from cache instead of
// processing. It is what makes a KV-depth measurement checkable: a --depth
// run is only at depth N if the server says it actually reused N tokens.
type benchTimings struct {
	CacheN             int     `json:"cache_n"`
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
	Depth      int      `json:"kv_depth_requested"`
	WallMS     float64  `json:"wall_ms"`
	CacheN     int      `json:"cache_n"`
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

// benchDepthResult is one KV depth's worth of measurement. pp and tg are
// reported as separate distributions because they are separate workloads:
// prompt processing is compute-bound and batches, generation is
// memory-bandwidth-bound and does not. A single blended tok/s hides both.
type benchDepthResult struct {
	DepthRequested int    `json:"kv_depth_requested"`
	DepthActual    int    `json:"kv_depth_filler_tokens"`
	DepthObserved  int    `json:"kv_depth_observed_cache_n"`
	DepthNote      string `json:"kv_depth_note,omitempty"`

	PP benchStat `json:"pp_tokens_per_second"`
	TG benchStat `json:"tg_tokens_per_second"`

	PromptTokens int     `json:"prompt_tokens_measured"`
	CacheHitRate float64 `json:"prompt_cache_hit_rate"`

	Samples  []benchRun `json:"samples"`
	Warnings []string   `json:"warnings,omitempty"`
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

	// Standard marks a --standard preset run (pp512 / tg128).
	Standard bool `json:"standard_preset"`

	// Model identity read from the weights file, for the canonical row and
	// the comparability key.
	ModelPath      string `json:"model_path,omitempty"`
	ModelQuant     string `json:"model_ftype,omitempty"`
	ModelSizeBytes int64  `json:"model_size_bytes,omitempty"`
	ModelParams    uint64 `json:"model_n_params,omitempty"`

	// Key is the configuration identity. Two rows may only be diffed when
	// their comparability_sha matches.
	Key benchComparabilityKey `json:"comparability_key"`

	// Depths carries one entry per --depth value, in ascending order.
	Depths []benchDepthResult `json:"depths"`

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
	Host          benchHostInfo      `json:"host"`
	Models        []benchModelResult `json:"models"`
	// StandardRows is the community-canonical markdown, emitted by --standard.
	StandardRows string `json:"standard_markdown,omitempty"`
}

func newMeasureBenchCmd(flags *rootFlags) *cobra.Command {
	var (
		flagRuns        int
		flagMaxTokens   int
		flagConcurrency int
		flagDepth       string
		flagStandard    bool
	)

	cmd := &cobra.Command{
		Use:   "bench <model...>",
		Short: "Benchmark seats through the production route, with the serving config identity attached",
		Long: `Benchmarks each model through POST /v1/chat/completions - the SAME route real
traffic takes - and reads rates from llama.cpp's own timings object.

Prompt processing and token generation are reported SEPARATELY, each as a
mean +/- sample standard deviation (n-1) over --runs, because they are
different workloads: pp is compute-bound and batches, tg is
memory-bandwidth-bound and does not. A spread above 3% of the mean is flagged
UNSTABLE - at that point the row describes two different machine states, not
one rate.

Three traps are encoded here because each one has already produced a wrong
number on this box:

  1. benching a side route measures a path nothing uses;
  2. leaving prompt caching on collapses prompt_n to ~4 and reports a fantasy
     prompt-processing rate - this command aborts when it sees that;
  3. deriving a rate from a progress-bar sample measures one instant, not the
     run - only the response's timings object is accepted.

--depth mirrors llama-bench's -d: N tokens are placed in the KV cache BEFORE
the timed window opens, so the measured rates are the rates at that context
depth. The prefill is excluded from the timing (llama.cpp reports it as
cache_n, and prompt_n counts only the new tokens), and the observed cache_n is
reported so a prefill that failed to stick cannot masquerade as a deep run.
Benching at depth 0 only OVERSTATES a seat: both rates decay as the cache
fills, and d0 is the most flattering point on that curve.

--standard emits the community-canonical pp512 / tg128 row as markdown, with
the build, the hardware line, and the comparability sha attached.

Every result records a comparability key over the serving configuration -
build, host, weights, and every seat flag that moves a number. 'bench compare'
refuses to diff two rows whose keys differ. A benchmark without its
serving-config identity is an anecdote.

Models are benched one at a time, never interleaved. Benching a model that is
not currently loaded WILL swap it in.`,
		Example: `  llamaswap-pp-cli bench gemma-4-e2b --runs 3 --max-tokens 128
  llamaswap-pp-cli bench gemma-4-e2b --runs 3 --depth 0,4096
  llamaswap-pp-cli bench gemma-4-e2b --standard
  llamaswap-pp-cli bench aux --embed --rerank`,
		Annotations: map[string]string{
			"pp:typed-exit-codes":  "3=model not in roster, 4=proxy unreachable, 27=upstream 5xx",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if wantsAgentErrorEnvelope(flags) {
					return usageEnvelopeErr(flags, fmt.Errorf("%q requires at least one model name", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			depths, err := parseDepths(flagDepth)
			if err != nil {
				return usageErr(err)
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, fmt.Sprintf("bench %v (%d runs, %d max tokens, depths %v)", args, flagRuns, flagMaxTokens, depths))
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			if handled, err := mcVerifyPlanOnly(cmd, flags, "bench", map[string]any{
				"models": args, "runs": flagRuns, "max_tokens": flagMaxTokens, "depths": depths,
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
			maxTokens := flagMaxTokens
			if flagStandard {
				maxTokens = benchStandardTG
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 10*time.Minute)

			roster, err := mcRoster(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			report := &benchReport{SchemaVersion: 2, StartedAt: mcNow(), Route: "POST /v1/chat/completions (production route)"}
			report.Host = benchReadHost(ctx, flags, mcTimeout(cmd, flags, 30*time.Second))

			for _, requested := range args {
				model, known := mcResolveAlias(roster, requested)
				if !known {
					return mcModelNotFound(requested, roster)
				}
				res, err := benchOne(ctx, cmd, flags, benchPlan{
					model: model, requested: requested, runs: flagRuns, maxTokens: maxTokens,
					concurrency: flagConcurrency, depths: depths, standard: flagStandard,
				}, report.Host, timeout)
				if err != nil {
					return err
				}
				report.Models = append(report.Models, *res)
				if flagStandard {
					report.StandardRows += benchStandardRow(res, report.Host)
				}
			}
			return mcEmit(cmd, flags, report, func(w io.Writer) { benchPrint(w, report) })
		},
	}
	cmd.Flags().IntVar(&flagRuns, "runs", 3, "Requests per model per depth (mean and sample stddev are taken across these)")
	cmd.Flags().IntVar(&flagMaxTokens, "max-tokens", 256, "Tokens to generate per request (--standard forces 128)")
	cmd.Flags().IntVar(&flagConcurrency, "concurrency", 1, "Requests in flight at once per iteration")
	cmd.Flags().StringVar(&flagDepth, "depth", "0", "KV depths to measure at, comma-separated (llama-bench -d): tokens prefilled before the timed window")
	cmd.Flags().BoolVar(&flagStandard, "standard", false, "Community-canonical preset: pp512 / tg128, emitted as a markdown row with build + hardware")
	addNovelCommandIfAbsent(cmd, newMeasureBenchAuxCmd(flags))
	addNovelCommandIfAbsent(cmd, newMeasureBenchCompareCmd(flags))
	return cmd
}

// benchPlan is one model's measurement request.
type benchPlan struct {
	model       string
	requested   string
	runs        int
	maxTokens   int
	concurrency int
	depths      []int
	standard    bool
}

func benchOne(ctx context.Context, cmd *cobra.Command, flags *rootFlags, plan benchPlan,
	host benchHostInfo, timeout time.Duration) (*benchModelResult, error) {

	model := plan.model
	res := &benchModelResult{
		Model: model, Runs: plan.runs, MaxTokens: plan.maxTokens, Concurrency: plan.concurrency,
		CachePrompt: false, Standard: plan.standard,
	}
	if plan.requested != model {
		res.RequestedAs = plan.requested
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

	// The measured prompt. --standard sizes it to 512 tokens with the model's
	// own tokenizer so a pp512 row means pp512; otherwise the fixed prompt is
	// used unchanged so historical rows stay comparable.
	prompt := benchPrompt
	tokenizedEarly := false
	if plan.standard {
		built, actual, err := benchBuildFiller(ctx, flags, model, benchStandardPP, mcTimeout(cmd, flags, 60*time.Second))
		if err != nil {
			return nil, mcClassify(fmt.Errorf("sizing the --standard %d-token prompt: %w", benchStandardPP, err))
		}
		prompt, tokenizedEarly = built, true
		res.Notes = append(res.Notes, fmt.Sprintf(
			"--standard: the prompt was sized to %d tokens by the model's own tokenizer (target %d); prompt_n below is what the server actually processed after the chat template",
			actual, benchStandardPP))
	}

	extras := mcLoadExtras(flags)
	var allSamples []benchRun
	var depthResults []benchDepthResult

	deltas, vramErr := measure.DeltaAround(ctx, extras.GPURoles, func() error {
		for _, depth := range plan.depths {
			dr := benchDepthResult{DepthRequested: depth}
			prefix := ""
			if depth > 0 {
				filler, actual, err := benchBuildFiller(ctx, flags, model, depth, mcTimeout(cmd, flags, 60*time.Second))
				if err != nil {
					dr.Warnings = append(dr.Warnings, fmt.Sprintf("depth %d skipped: building the prefill failed: %v", depth, err))
					depthResults = append(depthResults, dr)
					continue
				}
				prefix, dr.DepthActual = filler, actual
				tokenizedEarly = true
				// Untimed prefill: get the prefix into the slot's KV cache
				// before the measured request opens its window.
				warm := benchRequest(ctx, flags, model, prompt, prefix, 1, true, timeout)
				if warm.Error != "" {
					dr.Warnings = append(dr.Warnings, "the untimed prefill request failed ("+warm.Error+"); the depth below is what the server reports, not what was asked for")
				}
			}
			idx := 0
			var samples []benchRun
			for r := 0; r < plan.runs; r++ {
				batch := make([]benchRun, plan.concurrency)
				var wg sync.WaitGroup
				for c := 0; c < plan.concurrency; c++ {
					wg.Add(1)
					go func(slot int) {
						defer wg.Done()
						// Prompt caching stays OFF at depth 0 (trap 2). At a
						// non-zero depth it MUST be on, or the prefill it
						// depends on is discarded and the run silently
						// measures depth 0 instead.
						batch[slot] = benchRequest(ctx, flags, model, prompt, prefix, plan.maxTokens, depth > 0, timeout)
					}(c)
				}
				wg.Wait()
				for _, b := range batch {
					b.Index, b.Depth = idx, depth
					idx++
					samples = append(samples, b)
				}
			}
			dr.Samples = samples
			allSamples = append(allSamples, samples...)
			depthResults = append(depthResults, dr)
		}
		return nil
	})
	if vramErr != nil {
		// nvidia-smi missing must not sink a benchmark; the rates are still real.
		res.Warnings = append(res.Warnings, "VRAM not measured: "+vramErr.Error())
	}
	res.VRAM = deltas
	res.VRAMTotal = measure.TotalDelta(deltas)

	// Cold load: the first request's wall clock minus the work llama.cpp
	// reports doing. Only meaningful when this bench triggered the load AND
	// nothing before the first timed request already paid it.
	switch {
	case len(allSamples) > 0 && res.SwappedIn && tokenizedEarly:
		res.Notes = append(res.Notes,
			"cold load not measured: the tokenizer/prefill call ran before the first timed request and paid the load, so the first request's wall clock no longer contains it")
	case len(allSamples) > 0 && res.SwappedIn && allSamples[0].Error == "":
		cold := allSamples[0].WallMS - (allSamples[0].PromptMS + allSamples[0].PredictMS)
		if cold > 0 {
			res.ColdLoadMS = &cold
			allSamples[0].ColdLoadMS = &cold
		}
	}
	res.Samples = allSamples

	// Per-depth statistics.
	for i := range depthResults {
		d := &depthResults[i]
		var pp, tg []float64
		var failures, cacheN, promptN int
		for _, s := range d.Samples {
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
			if d.PromptTokens == 0 {
				d.PromptTokens = s.PromptN
			}
			cacheN += s.CacheN
			promptN += s.PromptN
			if s.TGPerSec > res.TGMax {
				res.TGMax = s.TGPerSec
			}
		}
		if failures == len(d.Samples) && len(d.Samples) > 0 {
			return nil, apiErr(fmt.Errorf("every bench request against %q at depth %d failed; first error: %s",
				model, d.DepthRequested, d.Samples[0].Error))
		}
		d.PP = benchStats(pp, "pp")
		d.TG = benchStats(tg, "tg")
		if n := len(d.Samples) - failures; n > 0 {
			d.DepthObserved = cacheN / n
		}
		if total := cacheN + promptN; total > 0 {
			d.CacheHitRate = float64(cacheN) / float64(total)
		}
		d.DepthNote = benchDepthVerdict(d)
		if d.PP.Unstable {
			d.Warnings = append(d.Warnings, d.PP.Note)
		}
		if d.TG.Unstable {
			d.Warnings = append(d.Warnings, d.TG.Note)
		}

		// prompt_n sanity. The fixed prompt is ~300 tokens; a prompt_n that
		// collapses to a handful means the prompt cache served it and the
		// prompt-processing rate is fiction. At a non-zero depth the cache is
		// deliberately on, but only the PREFIX may be served from it: the
		// measured suffix must still be processed.
		if d.PromptTokens > 0 && d.PromptTokens < 64 {
			return nil, fmt.Errorf(
				"ABORTED: prompt_n collapsed to %d for %q at depth %d. The measured prompt is ~300 tokens, so a "+
					"single-digit prompt_n means the whole prompt - not just the %d-token prefill - was served from cache, "+
					"and the prompt-processing rate would be fiction. "+
					"At depth 0 this bench sends \"cache_prompt\": false; a collapsed prompt_n there means the server ignored it "+
					"(check the seat for --cache-reuse / a proxy that rewrites the body) - fix that before trusting any PP number",
				d.PromptTokens, model, d.DepthRequested, d.DepthRequested)
		}
	}
	res.Depths = depthResults
	if len(depthResults) > 0 {
		res.PPMedian, res.TGMedian = depthResults[0].PP.Median, depthResults[0].TG.Median
		res.PromptTokens = depthResults[0].PromptTokens
	}

	res.Timings = benchTimingsMeta{
		Source: "llama.cpp timings object in the chat-completions response",
		Note:   "rates are prompt_per_second / predicted_per_second as reported by the server, never derived from wall clock or progress samples",
	}
	for _, s := range allSamples {
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
			if p, ok := mcSeatModelPath(seat.Cmd); ok {
				res.ModelPath = p
				if h, herr := gguf.Read(p); herr == nil && h.IsGGUF {
					res.ModelQuant, res.ModelSizeBytes = h.Quantization, h.FileSizeBytes
					if h.Quant != nil {
						res.ModelParams = h.Quant.Elements
					}
					res.Notes = append(res.Notes, ggufHeaderNotes(h)...)
				}
			}
		} else {
			res.Warnings = append(res.Warnings, "model was no longer in /running after the bench; serving-config identity is unavailable (was it evicted mid-run?)")
		}
	}
	res.SwapVersion = host.SwapVer
	if res.SwapVersion == "" {
		var version struct {
			Version string `json:"version"`
		}
		if err := mcGetJSON(ctx, flags, "/api/version", short, &version); err == nil {
			res.SwapVersion = version.Version
		}
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
	res.Key = buildComparabilityKey(res.ServingCmd, res.BuildInfo, res.ModelPath, res.ModelQuant, res.ModelSizeBytes, res.ModelParams, host)
	if len(res.Key.Unobserved) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"comparability key: %d of %d fields observed; unobserved (%s) are hashed as absent, so these rows only match others measured with the same gaps",
			res.Key.Observed, res.Key.Total, joinLimit(res.Key.Unobserved, 6)))
	}

	benchRecord(ctx, res)
	vramRecordDeltas(ctx, model, 0, deltas)
	return res, nil
}

// benchDepthVerdict states plainly whether the requested depth was reached.
// A prefill that did not stick would otherwise be published as a deep-context
// number that was measured on an empty cache.
func benchDepthVerdict(d *benchDepthResult) string {
	if d.DepthRequested == 0 {
		return "depth 0 (empty KV cache): the most flattering point on the decay curve. Both rates fall as context fills, so a d0-only row OVERSTATES steady-state throughput"
	}
	switch {
	case d.DepthObserved == 0:
		return fmt.Sprintf("REQUESTED depth %d but the server reused 0 cached tokens: this row was measured at depth 0, not %d. Do not quote it as a deep-context rate",
			d.DepthRequested, d.DepthRequested)
	case float64(d.DepthObserved) < float64(d.DepthActual)*0.9:
		return fmt.Sprintf("PARTIAL depth: %d tokens were prefilled but the server reused only %d (%.0f%%); the effective depth is the observed number",
			d.DepthActual, d.DepthObserved, float64(d.DepthObserved)/float64(d.DepthActual)*100)
	default:
		return fmt.Sprintf("depth reached: %d tokens prefilled, %d reused from cache (prefill excluded from the timed window)",
			d.DepthActual, d.DepthObserved)
	}
}

func joinLimit(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(", +%d more", len(items)-max)
}

// benchRequest issues one production-route request and reads the rates out of
// llama.cpp's timings block. prefix, when non-empty, is prepended to the
// measured prompt so the KV cache already holds it.
func benchRequest(ctx context.Context, flags *rootFlags, model, prompt, prefix string, maxTokens int, cachePrompt bool, timeout time.Duration) benchRun {
	content := prompt
	if prefix != "" {
		content = prefix + "\n\n" + prompt
	}
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": content},
		},
		"max_tokens": maxTokens,
		"stream":     false,
		// The trap: with prompt caching on, a repeated identical prompt is
		// served from cache and prompt_n collapses to a handful of tokens.
		// It is switched on ONLY for --depth runs, where the cached prefix is
		// the point and cache_n is checked against what was asked for.
		"cache_prompt": cachePrompt,
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
		run.CacheN = t.CacheN
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

// benchRecord writes ONE ROW PER DEPTH. A depth sweep stored as a single row
// would average two different measurements together, which is the thing the
// sweep exists to separate.
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
		keyJSON := benchKeyJSON(r.Key)
		for _, d := range r.Depths {
			if _, err := s.DB().ExecContext(ctx,
				`INSERT INTO bench_runs
				 (ts, model, config_sha, llamaswap_version, build_info, pp_median, tg_median, tg_max,
				  prompt_n, cold_load_ms, vram_delta_mib, gpu_uuid, clean_state, runs, max_tokens, concurrency, notes,
				  kv_depth, kv_depth_observed, pp_mean, pp_stddev, tg_mean, tg_stddev, cache_hit_rate,
				  comparability_sha, comparability_key)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				mcNow(), r.Model, r.ConfigSHA, r.SwapVersion, r.BuildInfo,
				d.PP.Median, d.TG.Median, d.TG.Max, d.PromptTokens, cold, r.VRAMTotal, gpuUUID,
				clean, r.Runs, r.MaxTokens, r.Concurrency, r.Timings.Source,
				d.DepthRequested, d.DepthObserved, d.PP.Mean, d.PP.Stddev, d.TG.Mean, d.TG.Stddev, d.CacheHitRate,
				r.Key.SHA, keyJSON); err != nil {
				return err
			}
		}
		return nil
	})
}

func benchPrint(w io.Writer, r *benchReport) {
	fmt.Fprintf(w, "%s  %s\n", bold("bench"), r.Route)
	if r.Host.CPU != "" {
		fmt.Fprintf(w, "  host            %s, %s | llama-swap %s %s\n", r.Host.CPU, r.Host.GPUs, r.Host.SwapVer, r.Host.SwapCommit)
	}
	for _, m := range r.Models {
		fmt.Fprintf(w, "\n%s\n", bold(m.Model))
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "DEPTH\tPP tok/s\tTG tok/s\tPROMPT_N\tCACHE HIT\tSTABLE")
		for _, d := range m.Depths {
			stable := green("yes")
			if d.PP.Unstable || d.TG.Unstable {
				stable = yellow("NO")
			}
			fmt.Fprintf(tw, "d%d\t%s\t%s\t%d\t%.0f%%\t%s\n",
				d.DepthRequested, d.PP.String(), d.TG.String(), d.PromptTokens, d.CacheHitRate*100, stable)
		}
		tw.Flush()
		for _, d := range m.Depths {
			fmt.Fprintf(w, "  d%-6d        %s\n", d.DepthRequested, d.DepthNote)
		}
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
		fmt.Fprintf(w, "  comparability   %s  (%d of %d fields observed)\n", short12(m.Key.SHA), m.Key.Observed, m.Key.Total)
		fmt.Fprintf(w, "  llama-swap      %s   llama.cpp build %s\n", m.SwapVersion, m.BuildInfo)
		fmt.Fprintf(w, "  runs            %d x %d tokens, concurrency %d\n", m.Runs, m.MaxTokens, m.Concurrency)
		for _, n := range m.Notes {
			fmt.Fprintf(w, "  note            %s\n", n)
		}
		for _, d := range m.Depths {
			for _, warn := range d.Warnings {
				fmt.Fprintf(w, "  %s d%d: %s\n", yellow("warning:"), d.DepthRequested, warn)
			}
		}
		for _, warn := range m.Warnings {
			fmt.Fprintf(w, "  %s %s\n", yellow("warning:"), warn)
		}
	}
	if r.StandardRows != "" {
		fmt.Fprintf(w, "\n%s\n\n%s", bold("standard row (markdown)"), r.StandardRows)
	}
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
