package main

// `local-offload generate-image --batch jobs.jsonl [--family F] [--route auto|local|remote] [--results r.jsonl] [--resume]
// [--jobs N]`: the CLI half of the fleet batch (P0 plan S3, register F28; ADR 0083 decision 13). Where the jobs run, the
// verdict per job and the exit status are mediaremote.RunBatch's; this file reads the jobs file, wires the warm call
// (pipeline.RunImageBatchAs, which needs a ComfyUI and so cannot live in mediaremote), keeps the results file as jobs end,
// and prints the same JSON the batch printed before, with the new keys added.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// imageBatchArgs are the flags of a `generate-image --batch` call.
type imageBatchArgs struct {
	file, family, route, results string
	resume                       bool
	jobs                         int
	refine                       bool
	remotes                      []string
	asJSON, compact              bool
}

// readBatchJobs reads a jobs.jsonl: one job per non-empty line, in order.
func readBatchJobs(path string) ([]pipeline.ImageBatchJob, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var jobs []pipeline.ImageBatchJob
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var j pipeline.ImageBatchJob
		if jerr := json.Unmarshal([]byte(line), &j); jerr != nil {
			return nil, fmt.Errorf("batch line %d: %w", n+1, jerr)
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func toBatchJobs(in []pipeline.ImageBatchJob) []mediaremote.BatchJob {
	out := make([]mediaremote.BatchJob, len(in))
	for i, j := range in {
		out[i] = mediaremote.BatchJob{Prompt: j.Prompt, Negative: j.Negative, Width: j.Width, Height: j.Height, Steps: j.Steps, Seed: j.Seed, Out: j.Out, Refine: j.Refine}
	}
	return out
}

func fromBatchJobs(in []mediaremote.BatchJob) []pipeline.ImageBatchJob {
	out := make([]pipeline.ImageBatchJob, len(in))
	for i, j := range in {
		out[i] = pipeline.ImageBatchJob{Prompt: j.Prompt, Negative: j.Negative, Width: j.Width, Height: j.Height, Steps: j.Steps, Seed: j.Seed, Out: j.Out, Refine: j.Refine}
	}
	return out
}

// runImageBatchVerb runs the batch and reports it. fs carries --config (and nothing else it needs).
func runImageBatchVerb(fs *flag.FlagSet, a imageBatchArgs) error {
	if a.results != "" && sameFile(a.results, a.file) {
		return fmt.Errorf("--results %s is the same file as --batch: the results would overwrite the jobs", a.results)
	}
	jobs, err := readBatchJobs(a.file)
	if err != nil {
		return err
	}
	// --refine=false applies to every job that does not set its own "refine" (job-level wins).
	applyBatchRefineDefault(jobs, a.refine)
	cfg, src := loadCfgWithSource(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	// Every job gets its seed and its out HERE, once, whichever way the batch then runs: the default out is a hash of the
	// seed, and a seed minted by each path would be two names for one job.
	jobs = pipeline.NormalizeImageBatch(jobs, cfg.MediaDir, a.family)

	var rf *mediaremote.ResultsFile
	if a.results != "" {
		// Opened before anything runs: a path that cannot be written stops the command before any lane time is spent.
		if rf, err = mediaremote.OpenResults(a.results); err != nil {
			return err
		}
		if kept := rf.KeptAside(); kept != "" {
			fmt.Fprintf(os.Stderr, "--results: an earlier batch left its rows in %s.partial; they are kept as %s, and this batch writes a fresh %s.partial\n", a.results, kept, a.results)
		}
	}

	// The batch IS the wait: when every lane is busy its jobs become tickets and the command blocks until they end. The
	// first interrupt cancels the tickets that sent nothing (a job already on a node cannot be recalled, ADR 0064); the
	// second is the default one.
	ictx, stopInterrupt := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopInterrupt()
	go func() {
		<-ictx.Done()
		stopInterrupt()
	}()

	var warmItems []pipeline.ImageBatchItem
	warm := func(ctx context.Context, js []mediaremote.BatchJob, wait time.Duration) ([]mediaremote.BatchRow, bool, error) {
		items, berr := p.RunImageBatchWaiting(ctx, a.family, fromBatchJobs(js), wait)
		warmItems = items
		rows := make([]mediaremote.BatchRow, len(items))
		for i, it := range items {
			rows[i] = mediaremote.BatchRow{Out: it.Out, Seed: it.Seed, OK: it.OK, Ms: it.Ms, Error: it.Error, Family: it.Family}
		}
		// A busy card is a DEFER, not a failure: the batch waited its window behind another holder and produced nothing,
		// which is recoverable by running it again.
		return rows, berr != nil && pipeline.IsGPUBusy(berr), berr
	}
	opts := mediaremote.BatchOpts{
		Family: a.family, Route: a.route, Remotes: a.remotes, Jobs: a.jobs, Resume: a.resume,
		Notify: func(line string) { fmt.Fprintln(os.Stderr, line) },
	}
	if src.Loaded() {
		opts.RosterFile = src.Path
	}
	if rf != nil {
		opts.OnRow = rf.Append
	}
	b, err := mediaremote.RunBatch(ictx, cfg, p, toBatchJobs(jobs), warm, opts)
	if err != nil {
		if rf != nil {
			rf.Discard()
		}
		return err
	}

	_, fam, ferr := p.Cfg().ResolveImageFamily(a.family)
	payload := batchPayload(b, fam, ferr == nil, warmItems, p.Cfg().ImageGenRefinerModel != "")
	data, _ := json.Marshal(payload)
	res := core.Result{OK: b.Err == nil && b.OK() == len(b.Rows), Data: data}
	if b.Err != nil {
		res.Reason = b.Err.Error()
		if b.Deferred {
			res.Deferred = true
			res.Meta.ErrClass = "gpu_busy"
		}
	}
	emitResult(res, a.asJSON, "", a.compact)
	var rerr error
	if rf != nil {
		rerr = rf.Close(b.Rows)
	}
	if err := batchExit(b); err != nil {
		return err
	}
	return rerr
}

// batchExit is the error the command ends with for a finished batch, nil for exit 0. A stop keeps the error the warm call
// stopped with (exit 1, as before); some jobs failed is exit 4 with the code the render helpers end such a batch with, so a
// shell caller no longer has to parse the JSON to learn that pictures are missing.
func batchExit(b *mediaremote.Batch) error {
	switch b.ExitCode() {
	case 0:
		return nil
	case imagegen.BatchExitJobsFailed:
		return &exitCodeError{code: imagegen.BatchExitJobsFailed, msg: fmt.Sprintf(
			"generate-image --batch: %d of %d jobs failed (exit %d); the items with \"ok\": false in the output name them",
			len(b.Rows)-b.OK(), len(b.Rows), imagegen.BatchExitJobsFailed)}
	}
	if b.Err != nil {
		return b.Err
	}
	return fmt.Errorf("generate-image --batch: %s", unfinishedText(b))
}

// unfinishedText says why the jobs with verdict queued are unfinished, by cause: an interrupt (a job already on a node cannot
// be recalled, so it is collected by a repeat with the same seed), or no place for them (a job waited the queue's ceiling and
// the jobs behind it were not sent, or a card stayed held: nothing was sent that needs recalling, and the rows say why).
func unfinishedText(b *mediaremote.Batch) string {
	n, total := b.Queued(), len(b.Rows)
	waited := fmt.Sprintf("a job waited the queue's %g h ceiling for a place and found none, and the jobs behind it were not sent", mediaremote.BatchHoldWindow().Hours())
	switch {
	case b.Interrupted && b.Ceiling:
		return fmt.Sprintf("interrupted with %d of %d jobs unfinished (verdict queued; also %s): a job already on a node cannot be recalled, so run the same batch again with --resume (give each job a seed so the node's job is collected, not rendered twice)", n, total, waited)
	case b.Interrupted:
		return fmt.Sprintf("interrupted with %d of %d jobs unfinished (verdict queued): a job already on a node cannot be recalled, so run the same batch again with --resume (give each job a seed so the node's job is collected, not rendered twice)", n, total)
	case b.Ceiling:
		return fmt.Sprintf("%d of %d jobs are queued (verdict queued): %s; nothing is wrong with them, so run the same batch again with --resume when the fleet has room", n, total, waited)
	}
	return fmt.Sprintf("%d of %d jobs are queued (verdict queued): no lane or node had a place for them, and the error of each row says why; nothing is wrong with them, so run the same batch again with --resume", n, total)
}

// sameFile says two paths name one file that exists: the same file on disk, whatever the spelling, the case or the link
// between them. A jobs file that does not exist stops the command on its own, so a name that exists nowhere needs no answer.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// batchPayload is the JSON a batch prints: the shape it always had (count, succeeded, failed, items, and the refiner's
// fallback count when a refiner is configured) with the new keys added, a verdict, hash and node on every item and the mode
// the batch ran in. A warm item keeps what only the warm call knows (the refiner's outcome). resolved says fam is this
// machine's binding of the named family.
//
// The binding's family and licence are stamped only where the batch COMPARED its recipe with the one that rendered
// (everywhere but ModeSerial); a serial item carries the family and licence its node reported, and the header names none.
// refine_fallbacks counts warm items, so it is reported for a warm batch whose warm call ran and answered only.
func batchPayload(b *mediaremote.Batch, fam config.FamilyInfo, resolved bool, warmItems []pipeline.ImageBatchItem, refiner bool) map[string]any {
	if b.Mode == mediaremote.ModeSerial {
		fam, resolved = config.FamilyInfo{}, false
	}
	warmAt := map[int]pipeline.ImageBatchItem{}
	for k, idx := range b.WarmJobs {
		if k < len(warmItems) {
			warmAt[idx] = warmItems[k]
		}
	}
	items := make([]map[string]any, len(b.Rows))
	refineFallbacks := 0
	for i, row := range b.Rows {
		var it *pipeline.ImageBatchItem
		if w, ok := warmAt[i]; ok {
			it = &w
			if w.RefineFallback != "" {
				refineFallbacks++
			}
		}
		items[i] = batchItem(row, it, fam)
	}
	payload := map[string]any{
		"count": len(b.Rows), "succeeded": b.OK(), "failed": len(b.Rows) - b.OK(), "items": items, "mode": b.Mode,
	}
	if n := b.Skipped(); n > 0 {
		payload["skipped"] = n
	}
	if len(b.Cluster) > 0 {
		payload["cluster"] = b.Cluster
	}
	// Surface the refiner fallback count in the batch summary when a refiner is configured and the warm call ran and answered
	// (0 = all jobs refined or opted out) — absent otherwise: tickets and serial renders were not measured, a warm call that a
	// busy lane kept waiting or that stopped measured nothing either, and a 0 would say they did.
	if refiner && b.Mode == mediaremote.ModeWarm && !b.Deferred && b.Err == nil && len(warmItems) > 0 {
		payload["refine_fallbacks"] = refineFallbacks
	}
	if resolved {
		payload["family"] = fam.Name
		if fam.License != "" {
			payload["license"] = fam.License
		}
		if fam.CommercialUse != nil {
			payload["commercial_use"] = *fam.CommercialUse
		}
	}
	return payload
}

// batchItem is one printed item: the warm call's item when there is one, with the batch's row laid over it (the row is the
// verdict after the file was proved), and the binding's license on it as every item always carried.
func batchItem(row mediaremote.BatchRow, it *pipeline.ImageBatchItem, fam config.FamilyInfo) map[string]any {
	m := map[string]any{}
	decode := func(v any) {
		b, _ := json.Marshal(v)
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber() // a seed above 2^53 stays the seed it is
		var layer map[string]any
		if dec.Decode(&layer) == nil {
			for k, v := range layer {
				m[k] = v
			}
		}
	}
	if it != nil {
		decode(*it)
	}
	decode(row)
	if _, has := m["license"]; !has && fam.License != "" {
		m["license"] = fam.License
	}
	if _, has := m["commercial_use"]; !has && fam.CommercialUse != nil {
		m["commercial_use"] = *fam.CommercialUse
	}
	return m
}
