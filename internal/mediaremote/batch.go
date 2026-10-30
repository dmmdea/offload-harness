package mediaremote

// The image batch across the fleet (P0 plan S3, register F28; ADR 0083, decision 13): `generate-image --batch jobs.jsonl`
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
//     warm call too, but it keeps its place in line for the lane (BatchHoldWindow) instead of giving up at the pipeline's own
//     window and coming back `queued`. Per-job tickets would only be one cold start per picture on the same lane, which is
//     slower than waiting for the lane once; the answer names each node and why it does not match;
//   - the jobs go one at a time through the single-render call (route remote, a machine with no image lane, a family only
//     the nodes define): see runJob for the wait, and BatchRow for what a row says about a recipe nobody compared.
//
// There is no mixed local/remote work stealing and no batch task on the fleet (v1): a node renders one job per ticket,
// with a ComfyUI cold start each (zero-warm, ADR 0009), and the warm session exists only on this machine.
//
// WHAT COMES BACK is one BatchRow per job in job order, and the same row is appended to <results>.partial as soon as the
// job's verdict is known (see ResultsFile; on the warm path the jobs end together, when the warm session does, so their rows
// arrive then): {i, out, seed, ok, ms, error} (what pipeline.parseBatchResults reads from the render script) plus node,
// family, sha256 and verdict. rendered: the lane ran it and the file at `out` is there, non-empty, hashed;
// skipped-existing: --resume found the file already there AND proved it (non-empty, a PNG that decodes whole, the size the
// job names); failed: it did not render (the reason is in error) and this job wrote nothing that a --resume could take for a
// picture (a picture that was already at `out` before the batch is not this job's to remove); queued: the job has no result
// yet, because the command ended before it did or because no lane or node had a place for it (a busy card is a place in
// line, never a failure: the reason is in error and --resume renders it).
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
	"sync/atomic"
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
	// License and CommercialUse are the node's own words about the recipe it rendered, set only where this machine did not
	// compare that recipe with its own (ModeSerial); everywhere else the licence is the local binding's.
	License       string `json:"license,omitempty"`
	CommercialUse *bool  `json:"commercial_use,omitempty"`
}

// BatchHoldWindow is how long a warm batch that must wait for a busy lane keeps its place in line: the queue's own ceiling
// for a ticket (ADR 0039). Chosen, not measured.
func BatchHoldWindow() time.Duration { return ticketMaxWait }

// batchWaitSteps is how long a serial job waits between two asks of a fleet that has no place for it, the last step
// holding (chosen: the steps the queue leaves a silent node alone for). A variable so a test can shorten it. One job waits
// at most ticketMaxWait in all.
var batchWaitSteps = []time.Duration{5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute}

// WarmFunc renders the given jobs through one warm session on this machine (pipeline.RunImageBatchAs). It returns one row
// per job, positionally, with the fields the render reports (out, seed, ok, ms, error); the batch fills the rest. deferred
// is a busy lane that kept the batch waiting past its window: nothing ran, the jobs are queued, and err says why.
type WarmFunc func(ctx context.Context, jobs []BatchJob, wait time.Duration) (rows []BatchRow, deferred bool, err error)

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
	// Ceiling: a job waited the queue's own ceiling (BatchHoldWindow) for a place and found none, so the jobs not yet sent
	// were queued at once instead of each waiting the ceiling in turn.
	Ceiling bool
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

// ExitCode is the status the command ends with. 0: every job has its picture, or a busy lane kept the whole warm batch
// waiting past its window (a defer, recoverable by running it again, as it always was: the rows are `queued`, so exit 0 does
// NOT say every picture is there). imagegen.BatchExitJobsFailed: some job failed, whether it ran or failed before any lane was
// asked, and a defer does not hide it. 1: it stopped (the warm call errored, the caller interrupted it before every job
// ended, or a job is queued because no lane or node had a place for it), so a wrapper can tell "some failed" from "stopped".
func (b *Batch) ExitCode() int {
	switch {
	case b.Deferred:
		if b.Failed() > 0 {
			return imagegen.BatchExitJobsFailed
		}
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
	carried := false
	if ferr != nil {
		if !carriesFamily(cfg, o, route) {
			return nil, ferr
		}
		// A family only the nodes define is carried to them by name, as a single render carries it: this machine has no
		// binding for it and renders nothing here.
		fam, carried = config.FamilyInfo{Name: o.Family}, true
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

	// 1. Jobs that cannot be placed at all fail on their own, before any lane is spent on them or on their neighbours. Their
	// rows are recorded once the mode is known (flush below), because what a row says about the family depends on it.
	var todo []int
	var pre []BatchRow
	for i, j := range jobs {
		if j.Out != "" {
			if err := os.MkdirAll(filepath.Dir(j.Out), 0o755); err != nil {
				pre = append(pre, BatchRow{I: i, Out: j.Out, Seed: j.Seed, Verdict: VerdictFailed, Error: "cannot make the folder for out: " + err.Error()})
				continue
			}
		}
		// 2. A file that proves itself is not rendered again.
		if o.Resume && j.Out != "" && pngVerifies(j.Out, j.Width, j.Height) {
			row := BatchRow{I: i, Out: j.Out, Seed: j.Seed, OK: true, Verdict: VerdictSkipped}
			if sum, _, err := fileSHA256(j.Out); err == nil {
				row.SHA256 = sum
			}
			pre = append(pre, row)
			continue
		}
		todo = append(todo, i)
	}
	flush := func() {
		for _, r := range pre {
			st.final(r.I, r)
		}
	}
	if len(todo) == 0 {
		flush()
		return b, nil
	}

	// 3. Where the rest runs.
	mode, inflight, why := chooseBatchMode(ctx, cfg, runner, jobs[todo[0]], todo, route, warm != nil, carried, o)
	b.Mode, b.InFlight, b.Cluster = mode, inflight, why.cluster
	if mode == ModeSerial {
		// The recipe was never compared with this machine's binding, so no row claims that binding's name: it carries the
		// family the caller typed (nothing, for a batch that named none) and what the node itself says.
		st.serial, st.fam = true, o.Family
	}
	flush()
	if why.line != "" {
		st.say(why.line)
	}
	switch mode {
	case ModeWarm:
		if o.Jobs > 0 {
			st.say(fmt.Sprintf("--jobs %d applies to the ticket path only: this batch renders on one warm session", o.Jobs))
		}
		b.WarmJobs = todo
		wait := time.Duration(0)
		if why.hold {
			wait = BatchHoldWindow()
		}
		st.runWarm(ctx, jobs, todo, warm, wait)
	default:
		st.runJobs(ctx, cfg, runner, jobs, todo, route, inflight)
	}
	if ctx.Err() != nil {
		b.Interrupted = b.Queued() > 0
	}
	b.Ceiling = st.ceiling.Load()
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
		key := outKey(j.Out)
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

// outKey names the file an out stands for: absolute and clean, and lower-cased where the usual file system ignores case
// (the rule samePath keeps). A relative and an absolute spelling of one path are one key.
func outKey(out string) string {
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}
	out = filepath.Clean(out)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		out = strings.ToLower(out)
	}
	return out
}

// sameJob says two jobs are one picture: every parameter equal, whichever way the out is spelled.
func sameJob(a, b BatchJob) bool {
	if (a.Refine == nil) != (b.Refine == nil) || (a.Refine != nil && *a.Refine != *b.Refine) {
		return false
	}
	a.Refine, b.Refine = nil, nil
	a.Out, b.Out = "", ""
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
	b *Batch
	o BatchOpts
	// fam is the family name a row carries when its lane did not say one: this machine's name for the recipe it compared, or,
	// where jobs go one at a time without a comparison (serial), the name the caller typed.
	fam  string
	self string
	// serial: the jobs go through the single-render call with no comparison of the recipe, so a row carries the family and
	// the licence the node that rendered it reported.
	serial bool
	// ceiling is set when a job waited the queue's ceiling for a place and found none: the jobs not yet sent are then queued
	// at once (runJobs).
	ceiling atomic.Bool
	mu      sync.Mutex
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
	// hold: the batch runs warm because the lane is busy and no roster node can relieve it, so it keeps its place in line
	// for the lane (BatchHoldWindow) instead of giving up at the pipeline's own window.
	hold bool
}

// chooseBatchMode decides, once, where the jobs in todo run, and the bound on tickets in flight for the ticket path.
func chooseBatchMode(ctx context.Context, cfg config.Config, runner Runner, first BatchJob, todo []int, route string, haveWarm, carried bool, o BatchOpts) (mode string, inflight int, why modeWhy) {
	warmOr := func(line string) (string, int, modeWhy) {
		if !haveWarm {
			return ModeSerial, 1, modeWhy{line: line}
		}
		return ModeWarm, 0, modeWhy{line: line}
	}
	if carried {
		// A family this machine has no binding for renders on a node or nowhere.
		return ModeSerial, 1, modeWhy{line: serialLine(o.Jobs)}
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
		return warmOr("") // no queue (the lease refuses the state root; a config with no state_dir has one): there is nothing to hold a job in
	}
	if v := core.ProbeLane(ctx, runner, req); v.Free && !v.Unjudged {
		return warmOr("") // the lane is free: one warm session renders the whole batch
	}
	// This machine's own identity (F59: it waits on the fingerprints of its weight files, up to 20 s for each on a cold cache, so it is built
	// only now, after the lane was asked and found held, as the single call's door does). Without one the batch is what it was.
	local, okID := identityOrLog(ctx, cfg, o.Family)
	if !okID {
		return warmOr("")
	}
	bases, err := candidateBases(cfg, o.Remotes)
	if err != nil {
		// A bad --remote is reported the way a single render reports it; the batch is what it was without a fleet.
		return warmOr("the fleet cannot be used for this batch (" + err.Error() + "), so it runs on this machine's lane")
	}
	cands, rows := candidatesFor(ctx, cfg, bases, local, needOf(req))
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
		line := "no node of the roster renders this recipe, so the batch keeps its place in line for this machine's lane (up to " + BatchHoldWindow().String() + ") and runs warm on it"
		if len(parts) > 0 {
			line += " (" + strings.Join(parts, "; ") + ")"
		}
		m, n, w := warmOr(line)
		w.cluster = cluster
		w.hold = m == ModeWarm
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

// serialLine says how jobs run where they go one at a time: --jobs does not apply, the node's recipe is not compared, and a
// busy node is waited for.
func serialLine(asked int) string {
	line := "the jobs run one at a time through the single-render call: the node's recipe is not compared with this machine's, so each row carries the family and licence the node reports, and a node with no place for a job is waited for"
	if asked > 1 {
		line = fmt.Sprintf("--jobs %d does not apply here: the jobs run one at a time (a node is claimed only by the ticket queue); ", asked) + line
	}
	return line
}

// carriesFamily says a family this machine cannot resolve is carried to the nodes by name rather than refused: the batch is
// forced onto the fleet (route remote), or this machine has no image lane at all (a thin client) and a fleet to hand it to.
// A machine that renders locally keeps refusing a name it does not know, so a typo is caught before any job is sent.
func carriesFamily(cfg config.Config, o BatchOpts, route string) bool {
	switch route {
	case RouteRemote:
		return true
	case RouteAuto:
		hasFleet := len(cfg.DelegateRemotes) > 0 || len(o.Remotes) > 0
		return hasFleet && !LocalConfigured(cfg, core.Request{Task: core.TaskGenerateImage})
	}
	return false
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

func (s *batchState) runWarm(ctx context.Context, jobs []BatchJob, todo []int, warm WarmFunc, wait time.Duration) {
	sub := make([]BatchJob, len(todo))
	for k, i := range todo {
		sub[k] = jobs[i]
	}
	rows, deferred, err := warm(ctx, sub, wait)
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
		case ctx.Err() != nil:
			// The caller's interrupt reached the batch before it had a lane: nothing was wrong with the job.
			row = BatchRow{Out: jobs[i].Out, Seed: jobs[i].Seed, Verdict: VerdictQueued, Error: "interrupted before this job ran: " + errText(err, ctx.Err().Error())}
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
			s.notStarted(jobs, todo, i, notStartedInterrupted)
			break launch
		}
		if ctx.Err() != nil {
			<-slots
			s.notStarted(jobs, todo, i, notStartedInterrupted)
			break launch
		}
		if s.ceiling.Load() {
			// A job waited the queue's whole ceiling for a place (the oldest in the line, or the one in front of this
			// one): the fleet has none for this job either, and sending it would only hold it for another ceiling.
			<-slots
			s.notStarted(jobs, todo, i, fmt.Sprintf("not started: an earlier job of this batch waited %s for a place and found none, so none is likely for this one; run the batch again with --resume to render it", shortDuration(ticketMaxWait)))
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

// notStartedInterrupted is the reason of a job the caller's interrupt reached before it was sent anywhere.
const notStartedInterrupted = "not started: the batch was interrupted before this job was sent to a lane"

// notStarted records the jobs from `from` on as queued with the reason: they were not sent anywhere.
func (s *batchState) notStarted(jobs []BatchJob, todo []int, from int, why string) {
	started := false
	for _, i := range todo {
		if i == from {
			started = true
		}
		if started {
			s.final(i, BatchRow{Out: jobs[i].Out, Seed: jobs[i].Seed, Verdict: VerdictQueued, Error: why})
		}
	}
}

// shortDuration words a wait for a person: whole minutes from a minute up, the duration as it is below.
func shortDuration(d time.Duration) string {
	if d >= time.Minute {
		return d.Round(time.Minute).String()
	}
	return d.String()
}

// runJob is one job's single-render call, with the wait for its lane inside it.
//
// A job is never failed for want of a place: an answer that says no lane or node had one for it (a held card, the queue's
// own ceiling, a fleet with no node free to take it) ends the job `queued` with that reason, and --resume renders it. On the
// ticket path the call blocks in the queue for its lane, and a lane that read free but was taken by a sibling job a moment
// later (a held card answered by the lane itself, not by the queue) is asked again a bounded number of times
// (batchBusyRetries); the queue's own end of a ticket (queueEnded) is not that race and is never sent again, because that
// would start another ticket and wait another ceiling. Where the jobs go one at a time (serial) there is no ticket to hold
// the job, so a held card or a fleet with no node free to take it is asked again after a pause (batchWaitSteps) until the
// job is placed, the caller interrupts, or the queue's own ceiling (ticketMaxWait) passes. A fleet that can NEVER take the
// job (every node left out for a configuration fact, see noPlaceNow) is not waited for: the job fails at once, naming each
// node's miss. Anything else the call answers is final.
func (s *batchState) runJob(ctx context.Context, cfg config.Config, runner Runner, i int, j BatchJob, route string) BatchRow {
	req := batchRequest(j, s.o.Family)
	w := Wait{Window: -1, RosterFile: s.o.RosterFile}
	if s.o.Notify != nil {
		w.Notify = func(line string) { s.say(fmt.Sprintf("job %d: %s", i, line)) }
	}
	start := time.Now()
	var res core.Result
	waits := 0
	for attempt := 0; ; attempt++ {
		res = batchCall(ctx, cfg, runner, req, route, s.o.Remotes, w)
		if res.OK || ctx.Err() != nil || !res.Deferred {
			break
		}
		if s.serial {
			if !noPlaceNow(res) || time.Since(start) >= ticketMaxWait {
				break
			}
			pause := batchWaitSteps[min(waits, len(batchWaitSteps)-1)]
			waits++
			s.say(fmt.Sprintf("job %d: no place for it yet (%s); asking again in %s", i, res.Reason, pause))
			if !sleepCtx(ctx, pause) {
				break
			}
			continue
		}
		if attempt >= batchBusyRetries || !core.CardHeld(res.Meta.ErrClass) || queueEnded(res) {
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
		switch {
		case ctx.Err() != nil:
			row.Verdict = VerdictQueued
			row.Error = "interrupted before this job ended: " + row.Error
		case s.serial && res.Deferred && noPlaceNow(res):
			// Nothing is wrong with the job: the fleet had no room for the whole of the queue's ceiling.
			row.Verdict = VerdictQueued
			row.Error = fmt.Sprintf("waited %s for a place and found none (%s): the job is queued, run the batch again with --resume to render it", time.Since(start).Round(time.Second), row.Error)
			s.ceiling.Store(true)
		case res.Deferred && queueEnded(res):
			// The queue ended the ticket without a place for this job (its ceiling, or a ticket it could not take over). Nothing
			// is wrong with the job, and the queue has waited for it already: it is not sent again here.
			row.Verdict = VerdictQueued
			row.Error = fmt.Sprintf("the queue ended this job's ticket without a place for it (%s): the job is queued, run the batch again with --resume to render it", row.Error)
			if queueCeiling(res) {
				s.ceiling.Store(true)
			}
		case res.Deferred && core.CardHeld(res.Meta.ErrClass):
			// A card that stays held is a place in line, not a failure: the lane itself answered held on every ask.
			row.Verdict = VerdictQueued
			row.Error = fmt.Sprintf("no place for it after %d ask(s) (%s): the job is queued, run the batch again with --resume to render it", 1+batchBusyRetries, row.Error)
		default:
			row.Verdict = VerdictFailed
		}
		return row
	}
	var data struct {
		ImagePath     string `json:"image_path"`
		Node          string `json:"node"`
		Unverified    bool   `json:"unverified"`
		Family        string `json:"family"`
		License       string `json:"license"`
		CommercialUse *bool  `json:"commercial_use"`
	}
	_ = json.Unmarshal(res.Data, &data)
	if row.Out == "" {
		row.Out = data.ImagePath
	}
	row.Node = data.Node
	if row.Node == "" {
		row.Node = s.self
	}
	if s.serial {
		// The node's own words: this machine never compared its recipe with the one that rendered the picture.
		row.Family, row.License, row.CommercialUse = data.Family, data.License, data.CommercialUse
	}
	row.Verdict = VerdictRendered
	if data.Unverified && data.Node != "" {
		// A node that publishes no sha256 for its file cannot be checked, and a picture nobody checked is not a result. The
		// fetch has already put the file at out; a failed job leaves nothing there, so the next --resume cannot take it for a
		// finished picture. It is set aside, not destroyed: the render took minutes.
		row.Verdict = VerdictFailed
		row.Error = "the file came back from " + data.Node + " but the node published no sha256 for it, so it could not be verified" + setAside(row.Out)
		return row
	}
	proveFile(&row)
	return row
}

// noPlaceNow says a call's answer is "there is no place for this job now" and not a verdict on the job: a card that is held
// (a node's own busy answer), or the delegator's own capacity defer that names no error class (no node is reachable, free
// of a text lease and able to take it, or one refused the POST for want of room). All of these clear by themselves. A fleet
// that can never take the job is NOT here: pickNodeFor files it under core.ErrClassNoNodeConfigured when every node was left
// out for a configuration fact (it does not serve the task, a route is not configured, it cannot carry refine=false, the
// tailnet guard refused its entry), and waiting does not clear those.
func noPlaceNow(res core.Result) bool {
	return core.CardHeld(res.Meta.ErrClass) || (res.DeferClass == core.DeferClassCapacity && res.Meta.ErrClass == "")
}

// sleepCtx waits d, or until ctx ends; it reports whether the full pause passed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// setAside moves a file the batch rejected next to its out as <out>.unverified, and says where it went (the words that end
// the row's reason). If it cannot be moved it is removed, so nothing a failed job wrote stays at its path; if neither works
// the words say the file is still there.
func setAside(out string) string {
	if out == "" {
		return ""
	}
	if _, err := os.Lstat(out); err != nil {
		return ""
	}
	aside := out + ".unverified"
	_ = os.Remove(aside)
	if err := os.Rename(out, aside); err == nil {
		return "; the picture was set aside as " + aside + " and nothing is left at out"
	}
	if err := os.Remove(out); err == nil {
		return "; the picture could not be set aside and was removed, so nothing is left at out"
	}
	return "; WARNING: the picture could not be moved or removed and is still at out, so --resume may take it for finished"
}

// proveFile completes a row whose lane reported success: the file must be at out and not empty, and its hash is read from
// it. A lane that said it rendered a file that is not there is a failure, not a result, and an empty file it left is removed.
func proveFile(r *BatchRow) {
	sum, n, err := fileSHA256(r.Out)
	switch {
	case r.Out == "":
		r.Verdict, r.Error = VerdictFailed, "the lane reported success but named no file"
	case err != nil:
		r.Verdict, r.Error = VerdictFailed, "the lane reported success but the file cannot be read: "+err.Error()
	case n == 0:
		// An empty file is not a picture, and a failed job leaves no file of its own at its path (the 21 zero-byte PNGs of 2026-10-09).
		_ = os.Remove(r.Out)
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
	// The header first: a canvas the job does not name, or one past resumeMaxPixels, is rendered again WITHOUT decoding it,
	// because a decode allocates the whole canvas and a stray 30000x30000 file at an out path must not take gigabytes of RAM
	// out of a resume pre-pass.
	hdr, err := pngcodec.DecodeConfig(bufio.NewReader(f))
	if err != nil || hdr.Width <= 0 || hdr.Height <= 0 {
		return false
	}
	if (w > 0 && hdr.Width != w) || (h > 0 && hdr.Height != h) || int64(hdr.Width)*int64(hdr.Height) > resumeMaxPixels {
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	img, err := pngcodec.Decode(bufio.NewReader(f))
	if err != nil {
		return false
	}
	b := img.Bounds()
	return b.Dx() == hdr.Width && b.Dy() == hdr.Height
}

// resumeMaxPixels bounds the canvas --resume will decode to prove a file: 64 Mpx, 8192x8192, at most about 256 MB for the
// decoded 8-bit canvas. Chosen, not measured: the harness's largest picture is a few Mpx, so a file past this is not one of
// its outputs and is rendered again, the safe direction.
const resumeMaxPixels = 64 << 20

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

// ResultsFile is the results.jsonl of a batch. A row is appended to <path>.partial as soon as its job is FINAL, and Close
// rewrites the rows whole in job order into <path>, atomically. Nothing touches <path> until then, so a record an earlier run
// left there (the node, the time and the hash of every picture are only in it) survives a batch the command refuses and one
// that dies: the dead one leaves <path>.partial, with every job that ended. A run that reopens the same name (the --resume
// that recovers a killed batch) first sets that record aside as <path>.partial.prev (one is kept, the newest), so the
// recovery run does not destroy the record it is recovering from. On the warm path the jobs end together when the warm
// session does, so their rows arrive then; the ticket path and the pre-flight failures arrive as they happen.
type ResultsFile struct {
	path string
	f    *os.File
	mu   sync.Mutex
	werr error
	// kept is where the record of an earlier batch was set aside (<path>.partial.prev); "" when there was none.
	kept string
}

// OpenResults creates (truncating) <path>.partial and the folder it is in. It is opened BEFORE the batch starts, so a path
// that cannot be written stops the command before any lane time is spent: a folder, and (where a rename over it fails) a
// read-only file, are refused now and not at the rename that ends the batch. <path> itself is not touched. A non-empty
// <path>.partial left by an earlier batch is set aside first (see ResultsFile.KeptAside); if it cannot be, the command is
// refused rather than destroy it.
func OpenResults(path string) (*ResultsFile, error) {
	if fi, err := os.Stat(path); err == nil {
		switch {
		case fi.IsDir():
			return nil, fmt.Errorf("--results %s: that path is a folder, not a file; name the file the rows are to be written to", path)
		case runtime.GOOS == "windows" && fi.Mode().Perm()&0o200 == 0:
			// Windows refuses to rename a file over a read-only one, so the batch would run for hours and fail at its end.
			return nil, fmt.Errorf("--results %s: the file is read-only, so the results could not be written over it when the batch ends; clear its read-only attribute or name another file", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("--results %s: %w", path, err)
	}
	partial := path + ".partial"
	kept, err := setPartialAside(partial)
	if err != nil {
		return nil, fmt.Errorf("--results %s: %w", path, err)
	}
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		restorePartial(partial, kept)
		return nil, fmt.Errorf("--results %s: %w", path, err)
	}
	return &ResultsFile{path: path, f: f, kept: kept}, nil
}

// setPartialAside moves the record an earlier batch left in partial to partial+".prev", replacing a .prev from before (one is
// kept: chosen). An empty or missing partial holds no record, so it is left to be truncated and never replaces a .prev that
// does. It returns where the record went, "" when there was none; an error when it is there and cannot be moved.
func setPartialAside(partial string) (string, error) {
	fi, err := os.Lstat(partial)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return "", nil
	}
	prev := partial + ".prev"
	_ = os.Remove(prev)
	if err := os.Rename(partial, prev); err != nil {
		return "", fmt.Errorf("an earlier batch left its record in %s and it could not be set aside as %s (%w); move it away and run again", partial, prev, err)
	}
	return prev, nil
}

// restorePartial puts a record that was set aside back under its own name.
func restorePartial(partial, kept string) {
	if kept != "" {
		_ = os.Remove(partial)
		_ = os.Rename(kept, partial)
	}
}

// KeptAside is where the record of an earlier, unfinished batch was set aside when this file was opened
// (<path>.partial.prev), or "" when there was none: the caller says so.
func (r *ResultsFile) KeptAside() string { return r.kept }

// Discard closes and removes the partial file: the batch was refused before it began and has no results to keep. A results
// file an earlier run left at the path is not touched, and the record of an earlier batch that was set aside is put back
// under its own name.
func (r *ResultsFile) Discard() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.f.Close()
	_ = os.Remove(r.path + ".partial")
	restorePartial(r.path+".partial", r.kept)
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

// Close writes every row of the batch in job order (a temp file in the same folder, renamed over <path>) and removes the
// partial file.
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
	_ = os.Remove(r.path + ".partial")
	if r.werr != nil {
		return fmt.Errorf("--results %s: a row could not be appended as its job finished (%w); the file written at the end is complete", r.path, r.werr)
	}
	return nil
}
