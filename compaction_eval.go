// compaction_eval.go — the `compaction-eval` verb: the OmniRoute-harvest
// Phase-B harness over a pinned transcript corpus (internal/compeval).
//
//	compaction-eval harvest --traces DIR --out corpus.jsonl [--min-turns N] [--max-entries N]
//	compaction-eval run    --corpus C [--gcf] [--skeleton] [--json]
//	compaction-eval freeze --corpus C [--gcf] [--skeleton] --out baseline.json
//	compaction-eval check  --corpus C --baseline B [--tolerance 0.02]
//	compaction-eval ab     --corpus C [--gcf] [--skeleton]   (needs the live endpoint)
//
// harvest builds a REAL replay corpus from standalone agent trace files with
// redaction-at-harvest (see internal/compeval/harvest.go) — the produced file
// is machine-local eval data, never committed. run/freeze/check are
// DETERMINISTIC and model-free (the replay measures with
// the production ladder + its own token estimator). ab scores full-vs-compacted
// through the live pipeline behind the control-pair self-test gate — a scorer
// that cannot rank a known-good/known-degraded pair aborts the A/B rather than
// producing a confident number from a blind judge. Every artifact is stamped
// with the corpus hash; a PII finding refuses the whole corpus.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/dmmdea/offload-harness/internal/compeval"
	"github.com/dmmdea/offload-harness/internal/core"
)

func runCompactionEval(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: local-offload compaction-eval <harvest|run|freeze|check|ab> [--corpus <corpus.jsonl>] [flags] (harvest takes --traces/--out instead of --corpus)")
	}
	mode := args[0]
	fs := flag.NewFlagSet("compaction-eval "+mode, flag.ExitOnError)
	fs.String("config", "", "config file path (consumed by ab; run/freeze/check are model-free and ignore it)")
	corpusPath := fs.String("corpus", "", "pinned corpus JSONL (compeval.Entry per line)")
	gcf := fs.Bool("gcf", false, "enable the lossless GCF rung in the replayed ladder")
	skeleton := fs.Bool("skeleton", false, "enable the skeleton rung in the replayed ladder")
	baselinePath := fs.String("baseline", "", "check: frozen baseline JSON")
	outPath := fs.String("out", "", "freeze/harvest: where to write the output file")
	tolerance := fs.Float64("tolerance", compeval.DefaultTolerance, "check: allowed per-entry token drift (fraction)")
	tracesDir := fs.String("traces", "", "harvest: directory of standalone agent trace files (*.json)")
	minTurns := fs.Int("min-turns", 3, "harvest: skip traces with fewer transcript turns (<=0 also means the default, 3)")
	maxEntries := fs.Int("max-entries", 0, "harvest: cap harvested entries (0 = all)")
	_ = fs.Parse(args[1:])
	if mode == "harvest" {
		return runCompactionHarvest(*tracesDir, *outPath, *minTurns, *maxEntries)
	}
	if *corpusPath == "" {
		return fmt.Errorf("compaction-eval %s: --corpus is required", mode)
	}

	entries, hash, err := compeval.Load(*corpusPath)
	if err != nil {
		return err
	}
	if findings := compeval.VetPII(entries); len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(os.Stderr, "[compaction-eval] PII refusal: entry %s class %s\n", f.EntryID, f.Class)
		}
		return fmt.Errorf("corpus refused: %d PII finding(s) — a replay corpus must be vetted, never scrubbed in place", len(findings))
	}
	opts := compeval.LadderOpts{GCF: *gcf, Skeleton: *skeleton}

	switch mode {
	case "run":
		rep := compeval.Evaluate(entries, hash, opts)
		return emitJSON(rep)
	case "freeze":
		if *outPath == "" {
			return fmt.Errorf("compaction-eval freeze: --out is required")
		}
		rep := compeval.Evaluate(entries, hash, opts)
		b := compeval.Freeze(rep)
		if err := compeval.SaveBaseline(*outPath, b); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[compaction-eval] baseline frozen: %s (corpus %.12s, ladder %s, %d entries)\n", *outPath, hash, rep.Ladder, len(b.TokensPerEntry))
		return nil
	case "check":
		if *baselinePath == "" {
			return fmt.Errorf("compaction-eval check: --baseline is required")
		}
		b, err := compeval.LoadBaseline(*baselinePath)
		if err != nil {
			return err
		}
		rep := compeval.Evaluate(entries, hash, opts)
		breaches, err := compeval.Check(b, rep, *tolerance)
		if err != nil {
			return err
		}
		if len(breaches) > 0 {
			_ = emitJSON(breaches)
			return fmt.Errorf("ratchet BREACHED: %d entr%s outside ±%.1f%%", len(breaches), plural(len(breaches), "y", "ies"), *tolerance*100)
		}
		fmt.Fprintf(os.Stderr, "[compaction-eval] ratchet holds (corpus %.12s, ladder %s, tolerance ±%.1f%%)\n", hash, rep.Ladder, *tolerance*100)
		return nil
	case "ab":
		cfg := loadCfg(fs)
		p, cleanup, err := openPipeline(cfg)
		if err != nil {
			return err
		}
		defer cleanup()
		// Outcome scorer — accept + ENTITY RECALL over a summarize of the
		// rendered transcript: 0.0 deferred; else 0.2 plus up to 0.8 for how
		// many of the source's FORCE_PRESERVE entities the summary actually
		// carries (capped at 8). Two dead ends were measured live before this
		// shape and are why grounding is deliberately NOT a term: (a) pure
		// self-grounding scores a degraded, entity-free source 1.0 (nothing
		// falsifiable), and (b) as a gate it INVERTS the ranking — the
		// entity-dense good side fails strict number-grounding on benign
		// paraphrase while the empty side passes trivially. Recall pays only
		// for preserved signal, which is the thing compaction can destroy.
		// The control-pair gate still decides admissibility on THIS box.
		scorer := func(ctx context.Context, rendered string) (float64, error) {
			res := p.Run(ctx, core.Request{Task: core.TaskSummarize, Input: rendered})
			if !res.OK {
				return 0, nil
			}
			var payload struct {
				Summary string `json:"summary"`
			}
			_ = json.Unmarshal(res.Data, &payload)
			summary := payload.Summary
			if summary == "" {
				summary = string(res.Data)
			}
			src := compeval.Entities(rendered)
			if len(src) == 0 {
				return 0.2, nil // accepted, but an entity-free source proves nothing more
			}
			sum := compeval.Entities(summary)
			hits := 0
			for e := range sum {
				if _, ok := src[e]; ok {
					hits++
				}
			}
			denom := len(src)
			if denom > 8 {
				denom = 8
			}
			recall := float64(hits) / float64(denom)
			if recall > 1 {
				recall = 1
			}
			return 0.2 + 0.8*recall, nil
		}
		rep, err := compeval.RunAB(context.Background(), scorer, entries, hash, opts, builtinControlPairs(), []string{cfg.Model, cfg.EscalationModel})
		if err != nil {
			return err
		}
		return emitJSON(rep)
	default:
		return fmt.Errorf("compaction-eval: unknown mode %q (harvest|run|freeze|check|ab)", mode)
	}
}

// runCompactionHarvest builds a real replay corpus from standalone agent
// traces: convert → redact-at-harvest → classify → residual-PII gate → write
// (round-trip-proven through the strict loader). Stats go to stderr; the
// corpus hash is the artifact every downstream report is stamped with.
func runCompactionHarvest(tracesDir, outPath string, minTurns, maxEntries int) error {
	if tracesDir == "" || outPath == "" {
		return fmt.Errorf("compaction-eval harvest: --traces and --out are required")
	}
	entries, stats, err := compeval.HarvestTraces(tracesDir, compeval.HarvestOpts{MinTurns: minTurns, MaxEntries: maxEntries})
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("compaction-eval harvest: no usable traces in %s (%d file(s), all skipped)", tracesDir, stats.Files)
	}
	hash, err := compeval.WriteCorpus(outPath, entries)
	if err != nil {
		return err
	}
	for _, n := range stats.Skipped {
		fmt.Fprintf(os.Stderr, "[compaction-eval] harvest skipped %s: %s\n", n.File, n.Reason)
	}
	classes := make([]string, 0, len(stats.Redactions))
	for class := range stats.Redactions {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		fmt.Fprintf(os.Stderr, "[compaction-eval] harvest redacted %d × %s\n", stats.Redactions[class], class)
	}
	fmt.Fprintf(os.Stderr, "[compaction-eval] harvested %d/%d trace(s) → %s (corpus %.12s)\n", stats.Harvested, stats.Files, outPath, hash)
	return emitJSON(struct {
		CorpusHash string               `json:"corpus_hash"`
		Out        string               `json:"out"`
		Stats      compeval.HarvestStats `json:"stats"`
	}{hash, outPath, stats})
}

// builtinControlPairs are the standing known-ordering probes: the same factual
// content intact vs deliberately degraded (entities stripped + order mangled).
// If the scorer cannot rank these, its full-vs-compacted verdicts are not
// admissible on this box (SelfTest aborts the run).
func builtinControlPairs() []compeval.ControlPair {
	good := "user: objective: summarize the deployment report\n" +
		"tool: Deploy 41 finished at 14:02 in 96s. Service api-gateway version 2.14.1 rolled to 12 nodes; " +
		"health checks passed on all nodes; p95 latency 210ms (was 240ms); error rate 0.02%. " +
		"One warning: node-7 restarted twice before stabilizing. Rollback window closes at 16:00.\n"
	degraded := "user: objective: summarize the deployment report\n" +
		"tool: finished at in Service version rolled to nodes checks passed on all latency was error rate " +
		"One node restarted twice before window closes at deploy the report the the\n"
	return []compeval.ControlPair{{Name: "deploy-report-intact-vs-stripped", Good: good, Degraded: degraded}}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// emitJSON pretty-prints a report to stdout (the machine-readable artifact;
// human progress goes to stderr).
func emitJSON(v any) error {
	j, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(j, '\n'))
	return err
}
