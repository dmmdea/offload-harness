package main

// `local-offload edit-image-generative`: the CLI door of the generative edit lane (Qwen-Image-Edit through render/comfy-edit.mjs).
// It is NOT edit-image, which is the deterministic PIL editor. Single mode edits one image; --batch edits the rows of a JSONL file
// through the same batch machinery generate-image --batch uses (mediaremote.RunBatch: --results, --resume, --spill), with one
// difference: comfy-edit.mjs renders one still per process and has no warm batch, so the warm side takes ONE row at a time.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
)

// editRow is one row of an edit batch (and, with the flags as defaults, the one edit of single mode).
type editRow struct {
	Image      string   `json:"image"`
	Refs       []string `json:"refs,omitempty"`
	Prompt     string   `json:"prompt"`
	Out        string   `json:"out"`
	Negative   string   `json:"negative,omitempty"`
	Seed       int      `json:"seed,omitempty"`
	Steps      int      `json:"steps,omitempty"`
	CFG        float64  `json:"cfg,omitempty"`
	Preset     string   `json:"preset,omitempty"`
	Megapixels float64  `json:"megapixels,omitempty"`
	Placement  string   `json:"placement,omitempty"`
}

// editDefaults are the flags that act as the default of every row; a row's own value wins.
type editDefaults struct {
	refs       []string
	prompt     string
	negative   string
	seed       int
	steps      int
	cfg        float64
	preset     string
	megapixels float64
	placement  string
}

// readEditRows reads a rows.jsonl: one row per non-empty line, in order.
func readEditRows(path string) ([]editRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []editRow
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r editRow
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if jerr := dec.Decode(&r); jerr != nil {
			return nil, fmt.Errorf("batch line %d: %w", n+1, jerr)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// applyEditDefaults fills what a row leaves out from the flags and derives the seed: with --seed S and no seed on the row, row i
// (its 0-based index in the file, so it does not move when --resume skips rows) gets S+i. A row that names a seed keeps it.
func applyEditDefaults(rows []editRow, d editDefaults) {
	for i := range rows {
		r := &rows[i]
		if len(r.Refs) == 0 {
			r.Refs = append([]string(nil), d.refs...)
		}
		if r.Prompt == "" {
			r.Prompt = d.prompt
		}
		if r.Negative == "" {
			r.Negative = d.negative
		}
		if r.Seed <= 0 && d.seed > 0 {
			r.Seed = d.seed + i
		}
		if r.Steps <= 0 {
			r.Steps = d.steps
		}
		if r.CFG <= 0 {
			r.CFG = d.cfg
		}
		if r.Preset == "" {
			r.Preset = d.preset
		}
		if r.Megapixels <= 0 {
			r.Megapixels = d.megapixels
		}
		if r.Placement == "" {
			r.Placement = d.placement
		}
	}
}

// checkEditRows refuses a row that cannot run, naming it, before any lane is asked.
func checkEditRows(rows []editRow, needOut bool) error {
	if len(rows) == 0 {
		return errors.New("edit-image-generative: no rows")
	}
	for i, r := range rows {
		switch {
		case strings.TrimSpace(r.Prompt) == "":
			return fmt.Errorf("edit-image-generative: row %d: prompt required", i)
		case strings.TrimSpace(r.Image) == "":
			return fmt.Errorf("edit-image-generative: row %d: image required", i)
		case needOut && strings.TrimSpace(r.Out) == "":
			return fmt.Errorf("edit-image-generative: row %d: out required in a batch (it names the file --resume proves and the results row)", i)
		case 1+len(r.Refs) > mediacap.EditMaxImages:
			return fmt.Errorf("edit-image-generative: row %d: an edit takes at most %d images in all (the target plus %d references); got %d", i, mediacap.EditMaxImages, mediacap.EditMaxImages-1, 1+len(r.Refs))
		case r.Megapixels != 0 && (r.Megapixels < mediacap.EditMegapixelsMin || r.Megapixels > mediacap.EditMegapixelsMax):
			return fmt.Errorf("edit-image-generative: row %d: megapixels must be between %g and %g (got %g)", i, mediacap.EditMegapixelsMin, mediacap.EditMegapixelsMax, r.Megapixels)
		case r.Placement != "" && r.Placement != "single" && r.Placement != "multicard":
			return fmt.Errorf("edit-image-generative: row %d: placement must be single or multicard (got %q)", i, r.Placement)
		}
		for _, p := range append([]string{r.Image}, r.Refs...) {
			if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
				return fmt.Errorf("edit-image-generative: row %d: image not found: %s", i, p)
			}
		}
	}
	return nil
}

func (r editRow) batchJob() mediaremote.BatchJob {
	return mediaremote.BatchJob{
		Prompt: r.Prompt, Negative: r.Negative, Steps: r.Steps, Seed: r.Seed, Out: r.Out,
		Edit: &mediaremote.EditJob{Image: r.Image, Refs: r.Refs, Preset: r.Preset, CFG: r.CFG, Megapixels: r.Megapixels, Placement: r.Placement},
	}
}

// runEditImageGenerative handles `local-offload edit-image-generative`.
func runEditImageGenerative(args []string) error {
	fs := flag.NewFlagSet("edit-image-generative", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	image := fs.String("image", "", "the source image to edit (single mode; a batch names it per row)")
	var refs repeatedFlag
	fs.Var(&refs, "ref", "a reference image after the target (repeatable, at most 9; qwen-image-2.1 edit families only); with --batch it is the default of every row that names none")
	prompt := fs.String("prompt", "", "the edit instruction; with --batch the default of every row without one")
	out := fs.String("out", "", "output PNG path (single mode; default under the media dir)")
	negative := fs.String("negative", "", "hard exclusions")
	seed := fs.Int("seed", 0, "RNG seed (default random); with --batch the base: a row without its own seed gets this plus its 0-based index in the file")
	steps := fs.Int("steps", 0, "sampler steps (advanced; overrides the preset)")
	cfgScale := fs.Float64("cfg", 0, "guidance (advanced; a Lightning preset needs 1.0)")
	preset := fs.String("preset", "", "full | lightning8 | lightning4 (2511 binding only)")
	megapixels := fs.Float64("megapixels", 0, "the working canvas of this edit in megapixels, 0.01 to 16 (default: follows the source within 0.9-2.0 MP, or the binding's gen_edit_megapixels); not on qwen-image-2.1")
	placement := fs.String("placement", "", "single | multicard (default: the binding's gen_edit_placement)")
	family := fs.String("family", "", "named edit family (offload_status media.edit_families); default = this machine's default edit binding")
	waiterToken := fs.String("waiter-token", "", "single mode only: "+waiterTokenFlagUsage)
	route := fs.String("route", "auto", "local | auto (here when this machine has the edit lane, else a fleet node holding the SAME edit recipe) | remote (a fleet node; the images travel in a hash-checked bundle and the output is fetched back verified)")
	var remotes repeatedFlag
	fs.Var(&remotes, "remote", "fleet node base URL for this call (repeatable); must be one of delegate_remotes")
	batchFile := fs.String("batch", "", "edit the rows of a JSONL file ({image, refs?, prompt, out, negative?, seed?, steps?, cfg?, preset?, megapixels?, placement?}); the flags are the default of each row and a row's own value wins")
	resultsFlag := fs.String("results", "", "with --batch: write one JSON row per edit to this file as it ends ({i,out,seed,ok,ms,error,node,family,sha256,verdict})")
	resume := fs.Bool("resume", false, "with --batch: skip a row whose out already holds a PNG that decodes whole and has the size the edit canvas rule gives for its source and megapixels")
	jobsFlag := fs.Int("jobs", 0, "with --batch: the most rows in flight on the ticket path (default: this machine's lane plus one per matching node, at most 8); with --spill, the most node workers")
	spillFlag := fs.Bool("spill", false, "with --batch (route auto only): when this machine's lane is free AND a roster node holds the same edit recipe, run both at once over one queue: this machine takes one row at a time, each matching node takes one row at a time")
	dryRun := fs.Bool("dry-run", false, "resolve and print what would run (the per-row requests, this machine's edit digest, the chosen lane and the verdict of every roster node's recipe match) without rendering or sending anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := leftoverArgErr(fs, "edit-image-generative"); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	defaults := editDefaults{refs: refs, prompt: *prompt, negative: *negative, seed: *seed, steps: *steps, cfg: *cfgScale, preset: *preset, megapixels: *megapixels, placement: *placement}

	var rows []editRow
	if *batchFile != "" {
		for _, name := range []string{"image", "out"} {
			if set[name] {
				return fmt.Errorf("edit-image-generative: --%s belongs to single mode; with --batch name it on each row", name)
			}
		}
		if set["waiter-token"] {
			return fmt.Errorf("edit-image-generative: --waiter-token resumes the one place a single edit left; a batch has no one place to resume")
		}
		if *jobsFlag < 0 {
			return fmt.Errorf("edit-image-generative: --jobs %d: the number of rows in flight cannot be negative (0 is the default)", *jobsFlag)
		}
		if r, _ := mediaremote.NormalizeRoute(*route); *spillFlag && r != mediaremote.RouteAuto {
			return fmt.Errorf("edit-image-generative: --spill shares the rows between this machine and the roster nodes, which only --route auto decides; --route %s names one place for every row: drop one of them", *route)
		}
		if *resultsFlag != "" && sameFile(*resultsFlag, *batchFile) {
			return fmt.Errorf("--results %s is the same file as --batch: the results would overwrite the rows", *resultsFlag)
		}
		var err error
		if rows, err = readEditRows(*batchFile); err != nil {
			return err
		}
		applyEditDefaults(rows, defaults)
		if err := checkEditRows(rows, true); err != nil {
			return err
		}
	} else {
		for _, name := range []string{"results", "resume", "jobs", "spill"} {
			if set[name] {
				return fmt.Errorf("edit-image-generative: --%s belongs to --batch (rows.jsonl), and no batch was given", name)
			}
		}
		rows = []editRow{{Image: *image, Out: *out}}
		applyEditDefaults(rows, defaults)
		if err := checkEditRows(rows, false); err != nil {
			return err
		}
	}

	cfg, src := loadCfgWithSource(fs)
	if *dryRun {
		return editDryRun(cfg, rows, *family, *route, remotes, *batchFile != "")
	}
	if err := refuseBrokenConfig("edit-image-generative", src); err != nil {
		return err
	}
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	// The verb IS the wait: a busy lane makes each edit a ticket (ADR 0083) and the command blocks until it ends. The first interrupt
	// cancels a ticket that sent nothing anywhere; the second is the default one.
	ictx, stopInterrupt := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopInterrupt()
	go func() {
		<-ictx.Done()
		stopInterrupt()
	}()

	if *batchFile == "" {
		wait := mediaremote.Wait{Window: -1, Notify: func(line string) { fmt.Fprintln(os.Stderr, line) }}
		if src.Loaded() {
			wait.RosterFile = src.Path
		}
		req := mediaremote.EditRequest(rows[0].batchJob(), *family, "")
		withWaiterToken(req.Params, *waiterToken)
		req.Resumable = true // this verb hands the place in line back (--waiter-token on the next call): see core.Request.Resumable
		res := mediaremote.RunWith(ictx, cfg, p, req, *route, remotes, wait)
		emitMediaResult(res, *asJSON, *compactFlag)
		return nil
	}

	var rf *mediaremote.ResultsFile
	if *resultsFlag != "" {
		if rf, err = mediaremote.OpenResults(*resultsFlag); err != nil {
			return err
		}
		if kept := rf.KeptAside(); kept != "" {
			fmt.Fprintf(os.Stderr, "--results: an earlier batch left its rows in %s.partial; they are kept as %s, and this batch writes a fresh %s.partial\n", *resultsFlag, kept, *resultsFlag)
		}
	}
	opts := mediaremote.BatchOpts{
		Family: *family, Route: *route, Remotes: remotes, Jobs: *jobsFlag, Spill: *spillFlag, Resume: *resume,
		Notify: func(line string) { fmt.Fprintln(os.Stderr, line) },
	}
	if src.Loaded() {
		opts.RosterFile = src.Path
	}
	if rf != nil {
		opts.OnRow = rf.Append
	}
	jobs := make([]mediaremote.BatchJob, len(rows))
	for i, r := range rows {
		jobs[i] = r.batchJob()
	}
	b, err := mediaremote.RunBatch(ictx, cfg, p, jobs, editWarm(p, *family), opts)
	if err != nil {
		if rf != nil {
			rf.Discard()
		}
		return err
	}
	payload := map[string]any{"count": len(b.Rows), "succeeded": b.OK(), "failed": b.Failed(), "queued": b.Queued(), "skipped": b.Skipped(), "mode": b.Mode, "items": b.Rows}
	if b.SpillNote != "" {
		payload["spill_note"] = b.SpillNote
	}
	data, _ := json.Marshal(payload)
	res := core.Result{OK: b.Err == nil && b.OK() == len(b.Rows), Data: data}
	if b.Err != nil {
		res.Reason = b.Err.Error()
	}
	emitResult(res, *asJSON, "", *compactFlag)
	var rerr error
	if rf != nil {
		rerr = rf.Close(b.Rows)
	}
	switch b.ExitCode() {
	case 0:
	case imagegen.BatchExitJobsFailed:
		return &exitCodeError{code: imagegen.BatchExitJobsFailed, msg: fmt.Sprintf("edit-image-generative --batch: %d of %d rows failed (exit %d); the items with \"ok\": false in the output name them",
			len(b.Rows)-b.OK(), len(b.Rows), imagegen.BatchExitJobsFailed)}
	default:
		if b.Err != nil {
			return b.Err
		}
		return fmt.Errorf("edit-image-generative --batch: %s", unfinishedText(b))
	}
	return rerr
}

// editWarm is the batch's warm call: this machine's own lane, one edit at a time, because comfy-edit.mjs renders one still per
// process (there is no session to share, so the batch machinery hands it chunks of one). A lane that stays held for the whole wait
// before the first edit ran is a defer (nothing ran, run the batch again); a later edit that finds it held ends as a failed row.
func editWarm(runner mediaremote.Runner, family string) mediaremote.WarmFunc {
	return func(ctx context.Context, jobs []mediaremote.BatchJob, wait time.Duration) ([]mediaremote.BatchRow, bool, error) {
		deadline := time.Now().Add(wait)
		var rows []mediaremote.BatchRow
		for k, j := range jobs {
			req := mediaremote.EditRequest(j, family, "")
			res := runner.Run(ctx, req)
			for k == 0 && !res.OK && res.Deferred && core.CardHeld(res.Meta.ErrClass) && wait > 0 && time.Now().Before(deadline) && ctx.Err() == nil {
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
				}
				res = runner.Run(ctx, req)
			}
			if k == 0 && !res.OK && res.Deferred && core.CardHeld(res.Meta.ErrClass) {
				return nil, true, errors.New(res.Reason)
			}
			row := mediaremote.BatchRow{Out: j.Out, Seed: j.Seed, OK: res.OK, Ms: res.Meta.LatencyMs, Error: res.Reason}
			if res.OK {
				var d struct {
					ImagePath string `json:"image_path"`
					Seed      int    `json:"seed"`
					Family    string `json:"family"`
				}
				_ = json.Unmarshal(res.Data, &d)
				if row.Out == "" {
					row.Out = d.ImagePath
				}
				row.Seed, row.Family = max(d.Seed, row.Seed), d.Family
			}
			rows = append(rows, row)
			if ctx.Err() != nil {
				break
			}
		}
		return rows, false, nil
	}
}

// editDryRun prints what the verb would do and sends nothing: the request each row stands for, and, once, where the edits would go
// (this machine's edit digest, the lane, and the strict recipe match of every roster node). The match is the same for every row of
// one family: the digest is the node's capability, and what a row chooses (preset, steps, cfg, megapixels, placement, seed) travels in
// the request.
func editDryRun(cfg config.Config, rows []editRow, family, route string, remotes []string, batch bool) error {
	items := make([]map[string]any, len(rows))
	for i, r := range rows {
		req := mediaremote.EditRequest(r.batchJob(), family, "")
		items[i] = map[string]any{"i": i, "out": r.Out, "prompt": req.Input, "params": req.Params}
	}
	where := mediaremote.DryRunEdit(context.Background(), cfg, mediaremote.EditRequest(rows[0].batchJob(), family, ""), route, remotes)
	out := map[string]any{"dry_run": true, "batch": batch, "rows": items, "placement": where}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
