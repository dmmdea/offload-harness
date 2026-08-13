// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source live

package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/store"
)

// Fixed aux workloads. Byte-identical across runs so latency is comparable.
const (
	auxEmbedText = "Retrieval quality depends on the pooling mode the embedder was started with; " +
		"a model that answers is not necessarily a model that answers the same way it did yesterday."
	auxRerankQuery = "How is the keep-set protected from eviction?"
)

var auxRerankDocs = []string{
	"The keep-set is parsed from the CLI's own config and the llama-swap YAML, never from the server's ttl field.",
	"Sliding-window attention caps the KV cache of most layers at the window size rather than the full context.",
	"Backups are content-addressed so a filename is only ever treated as a label.",
}

type auxSample struct {
	Index     int     `json:"index"`
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

type auxResult struct {
	Kind        string      `json:"kind"`
	Model       string      `json:"model"`
	Endpoint    string      `json:"endpoint"`
	Loaded      bool        `json:"was_loaded_before"`
	Runs        int         `json:"runs"`
	MedianMS    float64     `json:"median_latency_ms"`
	MinMS       float64     `json:"min_latency_ms"`
	MaxMS       float64     `json:"max_latency_ms"`
	ItemsPerSec float64     `json:"items_per_second"`
	Dims        int         `json:"embedding_dims,omitempty"`
	PromptToks  int         `json:"prompt_tokens,omitempty"`
	TopScore    *float64    `json:"top_score,omitempty"`
	TopIndex    *int        `json:"top_index,omitempty"`
	Samples     []auxSample `json:"samples"`
	Warnings    []string    `json:"warnings,omitempty"`
}

type auxReport struct {
	SchemaVersion int         `json:"schema_version"`
	StartedAt     string      `json:"started_at"`
	Results       []auxResult `json:"results"`
	Notes         []string    `json:"notes,omitempty"`
}

func newMeasureBenchAuxCmd(flags *rootFlags) *cobra.Command {
	var (
		flagEmbed       bool
		flagRerank      bool
		flagRuns        int
		flagEmbedModel  string
		flagRerankModel string
	)

	cmd := &cobra.Command{
		Use:   "aux",
		Short: "Latency and throughput for the resident embedder and reranker",
		Long: `Benchmarks the keep-set: the embedding model through POST /v1/embeddings and
the reranker through POST /v1/rerank, falling back to /rerank on a 404 for
older builds.

These are read-only serving calls against models that are meant to stay
resident, so this is the cheap half of the bench family - it swaps nothing in
as long as the keep-set is where it should be.

With neither --embed nor --rerank, both run.`,
		Example: `  llamaswap-pp-cli bench aux --embed --rerank
  llamaswap-pp-cli bench aux --embed --runs 5 --json`,
		Annotations: map[string]string{
			"pp:typed-exit-codes":  "3=model not in roster, 4=proxy unreachable, 27=upstream 5xx",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "bench aux")
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			if handled, err := mcVerifyPlanOnly(cmd, flags, "bench aux", map[string]any{
				"embed": flagEmbed, "rerank": flagRerank, "runs": flagRuns,
			}); handled {
				return err
			}
			if flagRuns < 1 {
				return usageErr(fmt.Errorf("--runs must be at least 1"))
			}
			doEmbed, doRerank := flagEmbed, flagRerank
			if !doEmbed && !doRerank {
				doEmbed, doRerank = true, true
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 5*time.Minute)

			roster, err := mcRoster(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			seats, err := mcRunning(ctx, flags, mcTimeout(cmd, flags, 15*time.Second))
			if err != nil {
				return mcClassify(err)
			}

			report := &auxReport{SchemaVersion: 1, StartedAt: mcNow()}
			report.Notes = append(report.Notes, "keep-set members are expected to be resident; a cold first call here means something evicted them")

			if doEmbed {
				model, known := mcResolveAlias(roster, flagEmbedModel)
				if !known {
					return mcModelNotFound(flagEmbedModel, roster)
				}
				res, err := auxBenchEmbed(ctx, flags, model, flagRuns, timeout, seats)
				if err != nil {
					return err
				}
				report.Results = append(report.Results, *res)
			}
			if doRerank {
				model, known := mcResolveAlias(roster, flagRerankModel)
				if !known {
					return mcModelNotFound(flagRerankModel, roster)
				}
				res, err := auxBenchRerank(ctx, flags, model, flagRuns, timeout, seats)
				if err != nil {
					return err
				}
				report.Results = append(report.Results, *res)
			}

			auxRecord(ctx, report)
			return mcEmit(cmd, flags, report, func(w io.Writer) { auxPrint(w, report) })
		},
	}
	cmd.Flags().BoolVar(&flagEmbed, "embed", false, "Benchmark the embedding model")
	cmd.Flags().BoolVar(&flagRerank, "rerank", false, "Benchmark the reranker")
	cmd.Flags().IntVar(&flagRuns, "runs", 3, "Requests per model")
	cmd.Flags().StringVar(&flagEmbedModel, "embed-model", "embeddinggemma", "Embedding model id or alias")
	cmd.Flags().StringVar(&flagRerankModel, "rerank-model", "bge-reranker-v2-m3", "Reranker model id or alias")
	return cmd
}

func auxBenchEmbed(ctx context.Context, flags *rootFlags, model string, runs int, timeout time.Duration, seats []mcSeat) (*auxResult, error) {
	_, loaded := mcFindSeat(seats, model)
	res := &auxResult{Kind: "embed", Model: model, Endpoint: "POST /v1/embeddings", Runs: runs, Loaded: loaded}
	if !loaded {
		res.Warnings = append(res.Warnings, "model was not resident before this run; the first sample includes a cold load")
	}
	var lat []float64
	for i := 0; i < runs; i++ {
		var out struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		start := time.Now()
		err := mcPostJSON(ctx, flags, "/v1/embeddings", map[string]any{"model": model, "input": auxEmbedText}, timeout, &out)
		ms := float64(time.Since(start).Microseconds()) / 1000
		s := auxSample{Index: i, LatencyMS: ms}
		if err != nil {
			s.Error = err.Error()
		} else {
			lat = append(lat, ms)
			if len(out.Data) > 0 {
				res.Dims = len(out.Data[0].Embedding)
			}
			res.PromptToks = out.Usage.PromptTokens
		}
		res.Samples = append(res.Samples, s)
	}
	if len(lat) == 0 {
		return nil, mcClassify(fmt.Errorf("every /v1/embeddings request against %q failed: %s", model, res.Samples[0].Error))
	}
	auxSummarize(res, lat)
	return res, nil
}

func auxBenchRerank(ctx context.Context, flags *rootFlags, model string, runs int, timeout time.Duration, seats []mcSeat) (*auxResult, error) {
	_, loaded := mcFindSeat(seats, model)
	res := &auxResult{Kind: "rerank", Model: model, Endpoint: "POST /v1/rerank", Runs: runs, Loaded: loaded}
	if !loaded {
		res.Warnings = append(res.Warnings, "model was not resident before this run; the first sample includes a cold load")
	}
	path := "/v1/rerank"
	var lat []float64
	for i := 0; i < runs; i++ {
		body := map[string]any{"model": model, "query": auxRerankQuery, "documents": auxRerankDocs, "top_n": len(auxRerankDocs)}
		var out struct {
			Results []struct {
				Index          int     `json:"index"`
				RelevanceScore float64 `json:"relevance_score"`
			} `json:"results"`
		}
		start := time.Now()
		err := mcPostJSON(ctx, flags, path, body, timeout, &out)
		if err != nil && path == "/v1/rerank" {
			var he *mcHTTPError
			if As(err, &he) && he.Status == http.StatusNotFound {
				// Older llama-swap builds expose the bare /rerank path only.
				path = "/rerank"
				res.Endpoint = "POST /rerank (fallback: /v1/rerank returned 404)"
				res.Warnings = append(res.Warnings, "this build does not serve /v1/rerank; fell back to /rerank")
				start = time.Now()
				err = mcPostJSON(ctx, flags, path, body, timeout, &out)
			}
		}
		ms := float64(time.Since(start).Microseconds()) / 1000
		s := auxSample{Index: i, LatencyMS: ms}
		if err != nil {
			s.Error = err.Error()
		} else {
			lat = append(lat, ms)
			if len(out.Results) > 0 {
				top := out.Results[0]
				for _, r := range out.Results {
					if r.RelevanceScore > top.RelevanceScore {
						top = r
					}
				}
				score, idx := top.RelevanceScore, top.Index
				res.TopScore, res.TopIndex = &score, &idx
			}
		}
		res.Samples = append(res.Samples, s)
	}
	if len(lat) == 0 {
		return nil, mcClassify(fmt.Errorf("every rerank request against %q failed: %s", model, res.Samples[0].Error))
	}
	auxSummarize(res, lat)
	return res, nil
}

func auxSummarize(res *auxResult, lat []float64) {
	res.MedianMS = median(lat)
	res.MinMS, res.MaxMS = lat[0], lat[0]
	for _, v := range lat {
		if v < res.MinMS {
			res.MinMS = v
		}
		if v > res.MaxMS {
			res.MaxMS = v
		}
	}
	if res.MedianMS > 0 {
		res.ItemsPerSec = 1000 / res.MedianMS
	}
}

func auxRecord(ctx context.Context, r *auxReport) {
	mcRecord(ctx, "bench aux", func(s *store.Store) error {
		for _, res := range r.Results {
			if _, err := s.DB().ExecContext(ctx,
				`INSERT INTO bench_runs (ts, model, pp_median, tg_median, runs, concurrency, notes)
				 VALUES (?, ?, ?, ?, ?, 1, ?)`,
				r.StartedAt, res.Model, res.ItemsPerSec, res.MedianMS, res.Runs,
				fmt.Sprintf("bench aux %s: pp_median=items/sec, tg_median=median latency ms, endpoint %s", res.Kind, res.Endpoint)); err != nil {
				return err
			}
		}
		return nil
	})
}

func auxPrint(w io.Writer, r *auxReport) {
	fmt.Fprintf(w, "%s  (%s)\n", bold("bench aux"), r.StartedAt)
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "KIND\tMODEL\tMEDIAN\tMIN\tMAX\tREQ/S\tDETAIL")
	for _, res := range r.Results {
		detail := ""
		switch {
		case res.Dims > 0:
			detail = fmt.Sprintf("%d dims, %d prompt tokens", res.Dims, res.PromptToks)
		case res.TopScore != nil:
			detail = fmt.Sprintf("top doc #%d score %.4f", *res.TopIndex, *res.TopScore)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f ms\t%.1f ms\t%.1f ms\t%.2f\t%s\n",
			res.Kind, res.Model, res.MedianMS, res.MinMS, res.MaxMS, res.ItemsPerSec, detail)
	}
	tw.Flush()
	for _, res := range r.Results {
		for _, warn := range res.Warnings {
			fmt.Fprintf(w, "  %s %s: %s\n", yellow("warning:"), res.Model, warn)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note  %s\n", n)
	}
}
