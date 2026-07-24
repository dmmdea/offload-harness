// compaction_eval.go — the `compaction-eval` verb: the OmniRoute-harvest
// Phase-B harness over a pinned transcript corpus (internal/compeval).
//
//	compaction-eval run    --corpus C [--gcf] [--skeleton] [--json]
//	compaction-eval freeze --corpus C [--gcf] [--skeleton] --out baseline.json
//	compaction-eval check  --corpus C --baseline B [--tolerance 0.02]
//	compaction-eval ab     --corpus C [--gcf] [--skeleton]   (needs the live endpoint)
//
// run/freeze/check are DETERMINISTIC and model-free (the replay measures with
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

	"github.com/dmmdea/offload-harness/internal/compeval"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/grounding"
)

func runCompactionEval(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: local-offload compaction-eval <run|freeze|check|ab> --corpus <corpus.jsonl> [flags]")
	}
	mode := args[0]
	fs := flag.NewFlagSet("compaction-eval "+mode, flag.ExitOnError)
	fs.String("config", "", "config file path")
	corpusPath := fs.String("corpus", "", "pinned corpus JSONL (compeval.Entry per line)")
	gcf := fs.Bool("gcf", false, "enable the lossless GCF rung in the replayed ladder")
	skeleton := fs.Bool("skeleton", false, "enable the skeleton rung in the replayed ladder")
	baselinePath := fs.String("baseline", "", "check: frozen baseline JSON")
	outPath := fs.String("out", "", "freeze: where to write the baseline JSON")
	tolerance := fs.Float64("tolerance", compeval.DefaultTolerance, "check: allowed per-entry token drift (fraction)")
	_ = fs.Parse(args[1:])
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
		// Outcome scorer — the harness's own accept/ground signal over a
		// summarize of the rendered transcript: 1.0 accepted+grounded,
		// 0.5 accepted but ungroundable, 0.0 deferred. Coarse by design; the
		// control-pair gate decides whether it discriminates on THIS box.
		scorer := func(ctx context.Context, rendered string) (float64, error) {
			res := p.Run(ctx, core.Request{Task: core.TaskSummarize, Input: rendered})
			if !res.OK {
				return 0, nil
			}
			if g, ok := grounding.Check(core.TaskSummarize, rendered, res.Data); ok && g {
				return 1, nil
			}
			return 0.5, nil
		}
		rep, err := compeval.RunAB(context.Background(), scorer, entries, hash, opts, builtinControlPairs(), []string{cfg.Model, cfg.EscalationModel})
		if err != nil {
			return err
		}
		return emitJSON(rep)
	default:
		return fmt.Errorf("compaction-eval: unknown mode %q (run|freeze|check|ab)", mode)
	}
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
