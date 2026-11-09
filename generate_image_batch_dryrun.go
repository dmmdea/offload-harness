package main

// `generate-image --batch jobs.jsonl --dry-run`: print the rows the batch would run with, and stop. Nothing renders, no lane is asked and
// no node is contacted; the config file and the jobs file are all it reads (and, with --resume, the files at each out).

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// printBatchDryRun prints one JSON document: the family and placement the batch names, and for every row its resolved prompt,
// negative, size (already snapped to the family's grid), steps, seed and out, plus, with --resume, whether a resume would skip it.
func printBatchDryRun(cfg config.Config, a imageBatchArgs, jobs []pipeline.ImageBatchJob) error {
	jobs = pipeline.NormalizeImageBatch(jobs, cfg.MediaDir, a.family)
	items := make([]map[string]any, len(jobs))
	for i, j := range jobs {
		it := map[string]any{"i": i, "prompt": j.Prompt, "out": j.Out, "seed": j.Seed}
		if j.Negative != "" {
			it["negative"] = j.Negative
		}
		if j.Width > 0 {
			it["width"] = j.Width
		}
		if j.Height > 0 {
			it["height"] = j.Height
		}
		if j.Steps > 0 {
			it["steps"] = j.Steps
		}
		if j.Refine != nil {
			it["refine"] = *j.Refine
		}
		if a.resume {
			it["resume"] = "render"
			if mediaremote.ResumeVerifies(j.Out, j.Width, j.Height) {
				it["resume"] = "skip"
			}
		}
		items[i] = it
	}
	doc := map[string]any{"dry_run": true, "count": len(jobs), "items": items}
	if a.family != "" {
		doc["family"] = a.family
	}
	if a.placement != "" {
		doc["placement"] = a.placement
	}
	var data []byte
	if a.compact {
		data, _ = json.Marshal(doc)
	} else {
		data, _ = json.MarshalIndent(doc, "", "  ")
	}
	fmt.Fprintln(os.Stdout, string(data))
	return nil
}
