package mediaremote

// The image batch across the fleet (P0 plan S3, register F28; ADR 0083, decision 12): `generate-image --batch jobs.jsonl`
// with a family, a route, a results file, a resume and a bound on the jobs in flight.
//
// WHERE A BATCH RUNS is decided once, at the start, and never mixed:
//
//   - the machine's lane is free (or the route is local, or there is no fleet): the WHOLE batch goes to the warm call the
//     caller passes in (pipeline.RunImageBatchAs: one ComfyUI session, the checkpoint loads once), exactly as before this
//     file existed;
//   - the lane is busy and at least one roster node renders the batch's recipe: EACH JOB is one ticket of the queue
//     (ADR 0083) and the call blocks for it, so a job starts on the first matching lane that frees, here or on a node, in
//     the order the batch listed them. At most N tickets are in flight, so a 200-job file does not make 200 tickets;
//   - the lane is busy and NO node renders the recipe (krea2 and the 2512 build exist on this machine's files only): the
//     warm call too, as before. Per-job tickets would only be one cold start per picture on the same lane, which is
//     slower than waiting for the lane once; the answer names each node and why it does not match.
//
// There is no mixed local/remote work stealing and no batch task on the fleet (v1): a node renders one job per ticket,
// with a ComfyUI cold start each (zero-warm, ADR 0009), and the warm session exists only on this machine.
//
// WHAT COMES BACK is one BatchRow per job in job order, and the same row is appended to the results file as soon as the
// job's verdict is known: {i, out, seed, ok, ms, error} (what pipeline.parseBatchResults reads from the render script)
// plus node, family, sha256 and verdict. rendered: the lane ran it and the file at `out` is there, non-empty, hashed;
// skipped-existing: --resume found the file already there AND proved it (non-empty, a PNG that decodes whole, the size the
// job names); failed: it did not render (the reason is in error); queued: the command ended before the job did.
//
// RESUME is fail-safe by construction. The ENOSPC night of 2026-10-09 left 21 zero-byte PNGs that a skip-existing script
// took for finished work, so a file is skipped only when it proves itself, and anything doubtful is rendered again. A job
// that names no `out` is never skipped: its default name derives from a seed minted afresh each run.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	pngcodec "image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/imagegen"
)

// BatchJob is one line of a jobs.jsonl: a prompt and the per-job parameters of a single render. It has the fields of
// pipeline.ImageBatchJob (the warm call's own job) and none of its behaviour, so the CLI converts at the boundary and this
// package does not depend on the pipeline.
type BatchJob struct {
	Prompt   string `json:"prompt"`
	Negative string `json:"negative,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Steps    int    `json:"steps,omitempty"`
	Seed     int    `json:"seed,omitempty"`
	Out      string `json:"out,omitempty"`
	// Refine is an explicit opt-out of the prompt refiner when false; nil or true is the machine's own setting.
	Refine *bool `json:"refine,omitempty"`
}

// Verdict says what became of one job.
type Verdict string

const (
	VerdictRendered Verdict = "rendered"
	VerdictSkipped  Verdict = "skipped-existing"
	VerdictFailed   Verdict = "failed"
	VerdictQueued   Verdict = "queued"
)

// BatchRow is the result of one job, and one line of the results file.
type BatchRow struct {
	I    int    `json:"i"`
	Out  string `json:"out"`
	Seed int    `json:"seed"`
	OK   bool   `json:"ok"`
	// Ms is the render's wall time as the lane that ran it reports it (this machine's pipeline, or the node's job); the
	// wait for a lane is not in it. 0 for a skipped job.
	Ms    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
	// Node is where it rendered: the roster node's id, or this machine's own id for a render here. Empty for a job that
	// did not render.
	Node string `json:"node,omitempty"`
	// Family is the name THIS machine gives the recipe (the one the batch named, or its default binding's): a node
	// renders the same recipe under its own name for it, and the row says where, not under which name.
	Family string `json:"family,omitempty"`
	// SHA256 is the hash of the file at Out, read after the render: for a node's job the fetch has already verified the
	// file against the sha256 the node published.
	SHA256  string  `json:"sha256,omitempty"`
	Verdict Verdict `json:"verdict"`
}

// WarmFunc renders the given jobs through one warm session on this machine (pipeline.RunImageBatchAs). It returns one row
// per job, positionally, with the fields the render reports (out, seed, ok, ms, error); the batch fills the rest. deferred
// is a busy lane that kept the batch waiting past its window: nothing ran, the jobs are queued, and err says why.
type WarmFunc func(ctx context.Context, jobs []BatchJob) (rows []BatchRow, deferred bool, err error)

// BatchOpts is how the caller wants the batch run.
type BatchOpts struct {
	// Family names the image family every job renders under ("" = this machine's default binding).
	Family string
	// Route is local, auto or remote ("" = auto), with the meaning it has for a single render.
	Route string
	// Remotes narrows delegate_remotes for this batch (each must be one of them).
	Remotes []string
	// Jobs bounds the tickets in flight (0 = the default: 1 for this machine's lane plus one per roster node whose recipe
	// matches, at most batchInFlightCap). It applies to the ticket path only.
	Jobs int
	// Resume skips the jobs whose out already holds a file that proves itself.
	Resume bool
	// RosterFile is the config file whose delegate_remotes the tickets re-read when it changes.
	RosterFile string
	// Notify gets one line for a human at a time: where the batch runs, where a ticket stands. Called under a lock.
	Notify func(string)
	// OnRow gets each job's row as soon as it is final. Called under a lock, in the order the jobs finish.
	OnRow func(BatchRow)
}

// Batch is how a batch ended.
type Batch struct {
	// Rows are the jobs' results in job order, one per job.
	Rows []BatchRow
	// Mode says where the jobs ran: ModeWarm (one warm session here), ModeTickets (one ticket per job), ModeSerial (one at
	// a time through the single-render call) or ModeNone (every job was skipped or failed before any lane was asked).
	Mode string
	// InFlight is the bound on tickets that applied (ModeTickets only).
	InFlight int
	// Cluster is why no node was used, one row per roster node, when the batch ran warm for want of a match.
	Cluster []ClusterRow
	// WarmJobs are the jobs the warm call was handed, as indexes into the file (the ones that were not skipped or failed
	// before any lane was asked): the call's own results are positional over exactly these.
	WarmJobs []int
	// Deferred: a busy lane kept the warm batch waiting past its window; nothing ran. Err says why.
	Deferred bool
	// Interrupted: the caller's context ended before every job did.
	Interrupted bool
	// Err is the error the warm call stopped with, if it did.
	Err error
}

const (
	ModeWarm    = "warm"
	ModeTickets = "tickets"
	ModeSerial  = "serial"
	ModeNone    = "none"
)

// batchInFlightCap bounds the default number of tickets in flight (a chosen constant, not a measurement: 1 for this
// machine's lane and one per matching node never needs more than the roster has, and a roster of 7 nodes has not been
// seen). --jobs overrides it.
const batchInFlightCap = 8

// batchBusyRetries is how many times a job whose single-render call came back "a card is held" is sent again. A lane that
// read free when the job was admitted can be taken by a sibling job a moment later; the second call finds it busy and
// becomes a ticket, which waits for a lane instead of looping, so the retry is bounded and cheap (chosen).
const batchBusyRetries = 2

// Failed, Queued and Skipped count the rows by verdict.
func (b *Batch) Failed() int  { return b.count(VerdictFailed) }
func (b *Batch) Queued() int  { return b.count(VerdictQueued) }
func (b *Batch) Skipped() int { return b.count(VerdictSkipped) }

func (b *Batch) count(v Verdict) int {
	n := 0
	for _, r := range b.Rows {
		if r.Verdict == v {
			n++
		}
	}
	return n
}

// OK counts the jobs that have their picture: rendered now or skipped because it was already there.
func (b *Batch) OK() int {
	n := 0
	for _, r := range b.Rows {
		if r.OK {
			n++
		}
	}
	return n
}

// ExitCode is the status the command ends with. 0: every job has its picture, or a busy lane kept the whole batch waiting
// (a defer, recoverable by running it again, as it always was). imagegen.BatchExitJobsFailed: it ran every job and some
// failed. 1: it stopped (the warm call errored, or the caller interrupted it before every job ended), so a wrapper can tell
// "some failed" from "stopped".
func (b *Batch) ExitCode() int {
	switch {
	case b.Deferred:
		return 0
	case b.Err != nil || b.Interrupted || b.Queued() > 0:
		return 1
	case b.Failed() > 0:
		return imagegen.BatchExitJobsFailed
	}
	return 0
}

// batchCall is the single-render call one ticket-path job makes. A seam so a test can count the jobs in flight.
var batchCall = func(ctx context.Context, cfg config.Config, runner Runner, req core.Request, route string, remotes []string, w Wait) core.Result {
	return RunWith(ctx, cfg, runner, req, route, remotes, w)
}

// RunBatch runs the jobs. The error is a refusal before anything ran (an empty batch, an empty prompt, two different jobs
// for one out, an unknown family or route); once the jobs start, whatever happens is in the rows and the Batch.
//
// The caller has given every job a seed and an out (pipeline.NormalizeImageBatch), so a job that names neither renders the
// same under either path; a job without them still runs, with a seed minted here and the out the lane chooses.
func RunBatch(ctx context.Context, cfg config.Config, runner Runner, jobs []BatchJob, warm WarmFunc, o BatchOpts) (*Batch, error) {
	route, ok := NormalizeRoute(o.Route)
	if !ok {
		return nil, fmt.Errorf("unrecognized route %q; use local, auto or remote", o.Route)
	}
	if o.Jobs < 0 {
		return nil, fmt.Errorf("--jobs %d: the number of jobs in flight cannot be negative (0 is the default)", o.Jobs)
	}
	if len(jobs) == 0 {
		return nil, errors.New("empty batch")
	}
	if route == RouteRemote {
		// A hand-forced remote batch needs a fleet to send to; say so now, not once per job.
		if _, err := candidateBases(cfg, o.Remotes); err != nil {
			return nil, err
		}
	}
	for i, j := range jobs {
		if strings.TrimSpace(j.Prompt) == "" {
			return nil, fmt.Errorf("job %d: empty prompt", i)
		}
	}
	if route == RouteAuto {
		cfg = effectiveCfg(cfg, o.RosterFile) // a node added to the config file since this process began is in the fleet
	}
	_, fam, ferr := cfg.ResolveImageFamily(o.Family)
	if ferr != nil {
		return nil, ferr
	}
	// A seed a job did not give is minted now, before the outs are compared: two seedless jobs for one out are two different
	// pictures, whatever else they share.
	jobs = append([]BatchJob(nil), jobs...)
	for i := range jobs {
		if jobs[i].Seed <= 0 {
			jobs[i].Seed = mintSeed()
		}
	}
	if err := distinctOuts(jobs); err != nil {
		return nil, err
	}
	b := &Batch{Rows: make([]BatchRow, len(jobs)), Mode: ModeNone}
	st := &batchState{b: b, o: o, fam: fam.Name, self: selfOrLocal(cfg)}

	// 1. Jobs that cannot be placed at all fail on their own, before any lane is spent on them or on their neighbours.
	var todo []int
	for i, j := range jobs {
		if j.Out != "" {
			if err := os.MkdirAll(filepath.Dir(j.Out), 0o755); err != nil {
				st.final(i, BatchRow{I: i, Out: j.Out, Seed: j.Seed, Verdict: VerdictFailed, Error: "cannot make the folder for out: " + err.Error()})
				continue
			}
		}
		// 2. A file that proves itself is not rendered again.
		if o.Resume && j.Out != "" && pngVerifies(j.Out, j.Width, j.Height) {
			row := BatchRow{I: i, Out: j.Out, Seed: j.Seed, OK: true, Verdict: VerdictSkipped}
			if sum, _, err := fileSHA256(j.Out); err == nil {
				row.SHA256 = sum
			}
			st.final(i, row)
			continue
		}
		todo = append(todo, i)
	}
	if len(todo) == 0 {
		return b, nil
	}

	// 3. Where the rest runs.
	mode, inflight, why := chooseBatchMode(ctx, cfg, runner, jobs[todo[0]], todo, route, warm != nil, o)
	b.Mode, b.InFlight, b.Cluster = mode, inflight, why.cluster
	if why.line != "" {
		st.say(why.line)
	}
	switch mode {
	case ModeWarm:
		if o.Jobs > 0 {
			st.say(fmt.Sprintf("--jobs %d applies to the ticket path only: this batch renders on one warm session", o.Jobs))
		}
		b.WarmJobs = todo
		st.runWarm(ctx, jobs, todo, warm)
	default:
		st.runJobs(ctx, cfg, runner, jobs, todo, route, inflight)
	}
	if ctx.Err() != nil {
		b.Interrupted = b.Queued() > 0
	}
	return b, nil
}

// distinctOuts refuses two DIFFERENT jobs that would write one file: the second would silently replace the first. Two
// identical jobs are the same picture twice and are left alone.
func distinctOuts(jobs []BatchJob) error {
	seen := map[string]int{}
	for i, j := range jobs {
		if j.Out == "" {
			continue
		}
		key := filepath.Clean(j.Out)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if prev, dup := seen[key]; dup {
			if sameJob(jobs[prev], j) {
				continue
			}
			return fmt.Errorf("jobs %d and %d are different pictures with the same out %s: give each its own out", prev, i, j.Out)
		}
		seen[key] = i
	}
	return nil
}

func sameJob(a, b BatchJob) bool {
	if (a.Refine == nil) != (b.Refine == nil) || (a.Refine != nil && *a.Refine != *b.Refine) {
		return false
	}
	a.Refine, b.Refine = nil, nil
	return a == b
}

// selfOrLocal is the id a render on this machine is attributed to.
func selfOrLocal(cfg config.Config) string {
	if id := selfNodeID(cfg); id != "" {
		return id
	}
	return "local"
}

// batchState carries what the runners share: the rows, the lock the callbacks run under, the names every row carries.
type batchState struct {
	b    *Batch
	o    BatchOpts
	fam  string
	self string
	mu   sync.Mutex
}

// final records a job's row, the one time it is written, and hands it to the caller.
func (s *batchState) final(i int, row BatchRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row.I = i
	if row.Family == "" {
		row.Family = s.fam
	}
	switch row.Verdict {
	case VerdictRendered, VerdictSkipped:
		row.OK = true
	default:
		row.OK = false
	}
	s.b.Rows[i] = row
	if s.o.OnRow != nil {
		s.o.OnRow(row)
	}
}

func (s *batchState) say(line string) {
	if s.o.Notify == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.o.Notify(line)
}

// ---- where the batch runs ---------------------------------------------------------------------------------------------

type modeWhy struct {
	line    string
	cluster []ClusterRow
}

// chooseBatchMode decides, once, where the jobs in todo run, and the bound on tickets in flight for the ticket path.
func chooseBatchMode(ctx context.Context, cfg config.Config, runner Runner, first BatchJob, todo []int, route string, haveWarm bool, o BatchOpts) (mode string, inflight int, why modeWhy) {
	warmOr := func(line string) (string, int, modeWhy) {
		if !haveWarm {
			return ModeSerial, 1, modeWhy{line: line}
		}
		return ModeWarm, 0, modeWhy{line: line}
	}
	switch route {
	case RouteLocal:
		return warmOr("")
	case RouteRemote:
		// The single-render call has no claim on a node, so two jobs in flight could pick one idle node: one at a time.
		return ModeSerial, 1, modeWhy{line: serialLine(o.Jobs)}
	}

	// route auto
	if len(cfg.DelegateRemotes) == 0 && len(o.Remotes) == 0 {
		return warmOr("") // no fleet: the batch is what it always was
	}
	if ambientLease() {
		return warmOr("") // a lease this process inherited chose its card
	}
	req := batchRequest(first, o.Family)
	if !LocalConfigured(cfg, req) {
		// No image lane on this machine: every job goes to a node through the single-render call, which has no claim on a
		// node (the claim belongs to the ticket queue), so one at a time.
		return ModeSerial, 1, modeWhy{line: serialLine(o.Jobs)}
	}
	if _, ok := storeFor(cfg); !ok {
		return warmOr("") // no state_dir: there is no ticket queue to hold a job in
	}
	if v := core.ProbeLane(ctx, runner, req); v.Free && !v.Unjudged {
		return warmOr("") // the lane is free: one warm session renders the whole batch
	}
	local, okID, _ := localIdentity(cfg, o.Family)
	if !okID {
		return warmOr("")
	}
	bases, err := candidateBases(cfg, o.Remotes)
	if err != nil {
		// A bad --remote is reported the way a single render reports it; the batch is what it was without a fleet.
		return warmOr("the fleet cannot be used for this batch (" + err.Error() + "), so it runs on this machine's lane")
	}
	cands, rows := candidatesFor(ctx, cfg, bases, local, needOf(req.Params))
	matching := len(cands)
	for _, r := range rows {
		if r.matched {
			matching++
		}
	}
	if matching == 0 {
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].idx < rows[b].idx })
		cluster := make([]ClusterRow, 0, len(rows))
		var parts []string
		for _, r := range rows {
			cluster = append(cluster, r.row)
			parts = append(parts, r.row.Node+": "+r.row.Why)
		}
		line := "no node of the roster renders this recipe, so the batch waits for this machine's lane and runs warm on it as before"
		if len(parts) > 0 {
			line += " (" + strings.Join(parts, "; ") + ")"
		}
		m, n, w := warmOr(line)
		w.cluster = cluster
		return m, n, w
	}
	n := o.Jobs
	if n == 0 {
		n = 1 + matching
		if n > batchInFlightCap {
			n = batchInFlightCap
		}
	}
	if n > len(todo) {
		n = len(todo)
	}
	return ModeTickets, n, modeWhy{line: fmt.Sprintf("this machine's lane is busy and %d roster node(s) render this recipe: %d job(s) become tickets, at most %d in flight", matching, len(todo), n)}
}

// serialLine says --jobs did not apply where the jobs run one at a time.
func serialLine(asked int) string {
	if asked > 1 {
		return fmt.Sprintf("--jobs %d does not apply here: the jobs run one at a time (a node is claimed only by the ticket queue)", asked)
	}
	return ""
}

// batchRequest is the single render a job stands for: what the lane question, the roster match and the ticket are about.
func batchRequest(j BatchJob, family string) core.Request {
	params := map[string]any{}
	if j.Out != "" {
		params["out"] = j.Out
	}
	if j.Negative != "" {
		params["negative"] = j.Negative
	}
	for k, v := range map[string]int{"width": j.Width, "height": j.Height, "steps": j.Steps, "seed": j.Seed} {
		if v > 0 {
			params[k] = v
		}
	}
	// Only an EXPLICIT refine=false reaches the lane, the way the single render sends it.
	if j.Refine != nil && !*j.Refine {
		params["refine"] = false
	}
	if family != "" {
		params["family"] = family
	}
	return core.Request{Task: core.TaskGenerateImage, Door: "cli:generate-image", Input: j.Prompt, Params: params}
}

// ---- the warm path ------------------------------------------------------------------------------------------------------

func (s *batchState) runWarm(ctx context.Context, jobs []BatchJob, todo []int, warm WarmFunc) {
	sub := make([]BatchJob, len(todo))
	for k, i := range todo {
		sub[k] = jobs[i]
	}
	rows, deferred, err := warm(ctx, sub)
	s.b.Err = err
	s.b.Deferred = deferred && err != nil
	for k, i := range todo {
		var row BatchRow
		switch {
		case k < len(rows):
			row = rows[k]
			row.Seed = firstPositive(row.Seed, jobs[i].Seed)
			if row.Out == "" {
				row.Out = jobs[i].Out
			}
			if row.OK {
				row.Node = s.self
				row.Verdict = VerdictRendered
				proveFile(&row)
			} else {
				row.Verdict = VerdictFailed
				if row.Error == "" {
					row.Error = "the render reported no reason"
				}
			}
		case deferred:
			row = BatchRow{Out: jobs[i].Out, Seed: jobs[i].Seed, Verdict: VerdictQueued, Error: errText(err, "the lane was held and the batch gave up waiting for it")}
		default:
			row = BatchRow{Out: jobs[i].Out, Seed: jobs[i].Seed, Verdict: VerdictFailed, Error: errText(err, "no result recorded (batch aborted?)")}
		}
		s.final(i, row)
	}
}

func errText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

func firstPositive(a, b int) int {
	if a > 0 {
		return a
	}
	return b
}

// ---- the ticket path -----------------------------------------------------------------------------------------------------

func (s *batchState) runJobs(ctx context.Context, cfg config.Config, runner Runner, jobs []BatchJob, todo []int, route string, inflight int) {
	if inflight < 1 {
		inflight = 1
	}
	slots := make(chan struct{}, inflight)
	var wg sync.WaitGroup
launch:
	for _, i := range todo {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			s.notStarted(jobs, todo, i)
			break launch
		}
		if ctx.Err() != nil {
			<-slots
			s.notStarted(jobs, todo, i)
			break launch
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			s.final(i, s.runJob(ctx, cfg, runner, i, jobs[i], route))
		}(i)
	}
	wg.Wait()
}

// notStarted records the jobs from `from` on, which the interrupt reached before they were sent anywhere.
func (s *batchState) notStarted(jobs []BatchJob, todo []int, from int) {
	started := false
	for _, i := range todo {
		if i == from {
			started = true
		}
		if started {
			s.final(i, BatchRow{Out: jobs[i].Out, Seed: jobs[i].Seed, Verdict: VerdictQueued, Error: "not started: the batch was interrupted before this job was sent to a lane"})
		}
	}
}

// runJob is one job's single-render call, with the wait for its lane inside it.
func (s *batchState) runJob(ctx context.Context, cfg config.Config, runner Runner, i int, j BatchJob, route string) BatchRow {
	req := batchRequest(j, s.o.Family)
	w := Wait{Window: -1, RosterFile: s.o.RosterFile}
	if s.o.Notify != nil {
		w.Notify = func(line string) { s.say(fmt.Sprintf("job %d: %s", i, line)) }
	}
	start := time.Now()
	var res core.Result
	for attempt := 0; ; attempt++ {
		res = batchCall(ctx, cfg, runner, req, route, s.o.Remotes, w)
		if res.OK || ctx.Err() != nil || attempt >= batchBusyRetries || !res.Deferred || !core.CardHeld(res.Meta.ErrClass) {
			break
		}
		// A lane that read free when this job was admitted was taken by another job of this batch first. Asked again, the
		// call finds it busy and becomes a ticket.
		s.say(fmt.Sprintf("job %d: the lane was taken by another job a moment after it read free; asking again", i))
	}
	row := BatchRow{Out: j.Out, Seed: j.Seed, Ms: res.Meta.LatencyMs}
	if row.Ms <= 0 {
		row.Ms = time.Since(start).Milliseconds()
	}
	if !res.OK {
		row.Error = res.Reason
		if row.Error == "" {
			row.Error = "the lane reported no reason"
		}
		if ctx.Err() != nil {
			row.Verdict = VerdictQueued
			row.Error = "interrupted before this job ended: " + row.Error
		} else {
			row.Verdict = VerdictFailed
		}
		return row
	}
	var data struct {
		ImagePath  string `json:"image_path"`
		Node       string `json:"node"`
		Unverified bool   `json:"unverified"`
	}
	_ = json.Unmarshal(res.Data, &data)
	if row.Out == "" {
		row.Out = data.ImagePath
	}
	row.Node = data.Node
	if row.Node == "" {
		row.Node = s.self
	}
	row.Verdict = VerdictRendered
	if data.Unverified && data.Node != "" {
		// A node that publishes no sha256 for its file cannot be checked, and a picture nobody checked is not a result.
		row.Verdict = VerdictFailed
		row.Error = "the file came back from " + data.Node + " but the node published no sha256 for it, so it could not be verified"
		return row
	}
	proveFile(&row)
	return row
}

// proveFile completes a row whose lane reported success: the file must be at out and not empty, and its hash is read from
// it. A lane that said it rendered a file that is not there is a failure, not a result.
func proveFile(r *BatchRow) {
	sum, n, err := fileSHA256(r.Out)
	switch {
	case r.Out == "":
		r.Verdict, r.Error = VerdictFailed, "the lane reported success but named no file"
	case err != nil:
		r.Verdict, r.Error = VerdictFailed, "the lane reported success but the file cannot be read: "+err.Error()
	case n == 0:
		r.Verdict, r.Error = VerdictFailed, "the lane reported success but "+r.Out+" is empty"
	default:
		r.SHA256 = sum
	}
}

// ---- proving a file --------------------------------------------------------------------------------------------------------

// pngVerifies reports whether path holds a PNG that decodes whole and has the size the job names (a width or a height of 0
// is not named). A file that decodes is a picture; one that is missing, empty, cut short or another format is not, and the
// job renders again. A family that snaps the size (qwen-image-2.1 floors to 32) gives back a smaller picture than a size
// off its grid asks for: that never matches and is rendered again, which is the safe direction.
func pngVerifies(path string, w, h int) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	img, err := pngcodec.Decode(bufio.NewReader(f))
	if err != nil {
		return false
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return false
	}
	return (w <= 0 || b.Dx() == w) && (h <= 0 || b.Dy() == h)
}

func fileSHA256(path string) (sum string, n int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err = io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ---- the results file ------------------------------------------------------------------------------------------------------

// ResultsFile is the results.jsonl of a batch. A row is appended as soon as its job is final, so a batch that dies leaves
// the jobs that finished; Close rewrites the file whole in job order, atomically, so a finished batch has the tidy file.
type ResultsFile struct {
	path string
	f    *os.File
	mu   sync.Mutex
	werr error
}

// OpenResults creates (truncating) the results file and the folder it is in. It is opened BEFORE the batch starts, so a
// path that cannot be written stops the command before any lane time is spent.
func OpenResults(path string) (*ResultsFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("--results %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("--results %s: %w", path, err)
	}
	return &ResultsFile{path: path, f: f}, nil
}

// Discard closes and removes the file: the batch was refused before it began and has no results to keep.
func (r *ResultsFile) Discard() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.f.Close()
	_ = os.Remove(r.path)
}

// Append writes one row. A failed write is remembered and reported by Close; it does not stop the batch.
func (r *ResultsFile) Append(row BatchRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line, err := json.Marshal(row)
	if err == nil {
		_, err = r.f.Write(append(line, '\n'))
	}
	if err != nil && r.werr == nil {
		r.werr = err
	}
}

// Close rewrites the file with every row of the batch in job order (a temp file in the same folder, renamed over it).
func (r *ResultsFile) Close(rows []BatchRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.f.Close()
	var buf strings.Builder
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("--results %s: %w", r.path, err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("--results %s: %w", r.path, err)
	}
	if r.werr != nil {
		return fmt.Errorf("--results %s: a row could not be appended as its job finished (%v); the file written at the end is complete", r.path, r.werr)
	}
	return nil
}
