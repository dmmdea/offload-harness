package mediaremote

// The spill mode of an image batch (BatchOpts.Spill, `generate-image --batch --spill`; ADR 0083 decision 13). See the header of
// batch.go for what it does and why the warm side runs in chunks. This file holds the choice (spillChoice), the shared queue
// (spillQueue) and the two kinds of worker.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// spillMinChunk is the fewest jobs a warm chunk takes while more are queued: a chunk is one ComfyUI session, whose checkpoint
// load is paid per chunk, so a chunk of one is only a ticket's cold start. Chosen, not measured. The tail of a batch can end
// up with the warm side holding up to this many jobs while a node idles.
const spillMinChunk = 4

// spillLocalBusyAsks is how many times in a row the warm side asks for a held lane (each ask already waits the pipeline's own
// window) before it stops taking chunks and leaves the rest of the queue to the nodes. Chosen.
const spillLocalBusyAsks = 3

// spillPause is how long the warm side waits after a held lane before it asks again, so the nodes can take what it gave back.
const spillPause = 5 * time.Second

// spillNode is a roster node whose recipe matched this machine's.
type spillNode struct {
	base, shown string
}

// spillPlan is who shares the queue: the nodes that render this machine's recipe, in the order the placer prefers them, and how
// many remote workers there are (one per node by default; Jobs may ask for fewer or, with several workers on one node, more).
type spillPlan struct {
	nodes   []spillNode
	workers int
	// localBusy: this machine's lane was held when the batch began, so its warm session joins the queue when the lane frees.
	localBusy bool
}

// spillChoice decides a Spill batch whose lane read free: ModeSpill when at least one roster node renders the recipe, else the
// warm batch with the reason (spillNote) and the nodes' misses (cluster).
func spillChoice(ctx context.Context, cfg config.Config, req core.Request, queued int, o BatchOpts,
	warmOr func(string) (string, int, modeWhy)) (string, int, modeWhy) {
	fall := func(note string) (string, int, modeWhy) {
		m, n, w := warmOr("")
		w.spillNote = note
		return m, n, w
	}
	local, okID := identityOrLogFor(ctx, cfg, req)
	if !okID {
		return fall("this machine's recipe could not be fingerprinted (a weight file is missing or unreadable), so no node can be matched to it: the batch runs on this machine's warm session alone")
	}
	bases, err := candidateBases(cfg, o.Remotes)
	if err != nil {
		return fall("the fleet cannot be used for this batch (" + err.Error() + "): the batch runs on this machine's warm session alone")
	}
	cands, rows := candidatesFor(ctx, cfg, bases, local, needOf(req))
	nodes := spillNodesOf(cands, rows)
	if len(nodes) == 0 {
		cluster := make([]ClusterRow, 0, len(rows))
		var parts []string
		for _, r := range rows {
			cluster = append(cluster, r.row)
			parts = append(parts, r.row.Node+": "+r.row.Why)
		}
		note := "no node of the roster renders this recipe, so there is nothing to spill to: the batch runs on this machine's warm session alone"
		if len(parts) > 0 {
			note += " (" + strings.Join(parts, "; ") + ")"
		}
		m, n, w := warmOr("")
		w.cluster, w.spillNote = cluster, note
		return m, n, w
	}
	return spillPlanOf(nodes, queued, o, false)
}

// spillNodesOf lists the nodes of the roster that render the recipe: the ones free now, in the order the placer prefers them, then
// the ones that matched but are busy right now (a node that matched is still a node of the recipe: its worker asks again when it is
// free). rows is put in the roster's order.
func spillNodesOf(cands []candidate, rows []rowAt) []spillNode {
	var nodes []spillNode
	for _, c := range cands {
		nodes = append(nodes, spillNode{base: c.base, shown: c.shown})
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].idx < rows[b].idx })
	for _, r := range rows {
		if r.matched {
			nodes = append(nodes, spillNode{base: r.base, shown: r.row.Node})
		}
	}
	return nodes
}

// spillPlanOf is the ModeSpill answer for the matching nodes: one worker per node (Jobs may ask for fewer, or more), and the line
// that says so. localBusy says this machine's lane was held when the batch began: the nodes start at once and the warm session joins
// when the lane frees.
func spillPlanOf(nodes []spillNode, queued int, o BatchOpts, localBusy bool) (string, int, modeWhy) {
	workers := o.Jobs
	if workers == 0 {
		workers = len(nodes)
	}
	if workers > batchInFlightCap {
		workers = batchInFlightCap
	}
	if workers > queued {
		workers = queued
	}
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.shown
	}
	line := fmt.Sprintf("--spill: this machine's lane is free and %d roster node(s) render this recipe (%s): %d job(s) share one queue; the warm session here takes chunks of it, %d node worker(s) take one job at a time",
		len(nodes), strings.Join(names, ", "), queued, workers)
	if localBusy {
		line = fmt.Sprintf("--spill: this machine's lane is busy and %d roster node(s) render this recipe (%s): %d job(s) share one queue; %d node worker(s) take one job at a time now, and the warm session here joins and takes chunks when the lane frees (checked every %s)",
			len(nodes), strings.Join(names, ", "), queued, workers, scanEvery)
	}
	return ModeSpill, workers, modeWhy{line: line, spill: &spillPlan{nodes: nodes, workers: workers, localBusy: localBusy}}
}

// spillMissNote says, for a Spill batch that did not spill, what ran instead. A reason the choice itself found (spillChoice)
// is used in preference to this.
func spillMissNote(mode string) string {
	switch mode {
	case ModeSerial:
		return "the jobs run one at a time through the single-render call (this machine has no image lane for the recipe, or the family is only the nodes'): nothing to spill"
	}
	return "no node could be used (no delegate_remotes, a lease this process inherited, no queue directory, or the lane is busy and no node renders the recipe): the batch runs as --route auto does without --spill"
}

// spillQueue is the shared job queue: the indexes of the jobs nobody has taken, and a count of the jobs somebody holds.
type spillQueue struct {
	mu      sync.Mutex
	pending []int
	held    int
	// workers is the number of workers still pulling, this machine's included: the divisor of a warm chunk.
	workers int
	done    chan struct{}
	closed  bool
	// unit: the warm side takes ONE job per chunk. A generative edit has no warm session (render/comfy-edit.mjs renders one still per
	// process), so there is nothing a bigger chunk would share.
	unit bool
}

func newSpillQueue(todo []int, workers int) *spillQueue {
	return &spillQueue{pending: append([]int(nil), todo...), workers: workers, done: make(chan struct{})}
}

// settleLocked closes done when no job is waiting and no worker holds one: whoever sleeps in a back-off then has nothing to wake for.
func (q *spillQueue) settleLocked() {
	if !q.closed && len(q.pending) == 0 && q.held == 0 {
		q.closed = true
		close(q.done)
	}
}

// chunk is the number of jobs the warm side takes of n waiting: a share of what is left (n over the workers), at least
// spillMinChunk, at most n.
func spillChunk(n, workers int) int {
	if workers < 1 {
		workers = 1
	}
	c := (n + workers - 1) / workers
	if c < spillMinChunk {
		c = spillMinChunk
	}
	if c > n {
		c = n
	}
	return c
}

// takeChunk gives the warm side its next chunk, nil when the queue is empty.
func (q *spillQueue) takeChunk() []int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil
	}
	n := spillChunk(len(q.pending), q.workers)
	if q.unit {
		n = 1
	}
	out := append([]int(nil), q.pending[:n]...)
	q.pending = q.pending[n:]
	q.held += n
	return out
}

// takeOne gives a node worker its next job.
func (q *spillQueue) takeOne() (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return 0, false
	}
	i := q.pending[0]
	q.pending = q.pending[1:]
	q.held++
	return i, true
}

// giveBack returns jobs a worker took and could not place, to the front of the queue (they were the oldest).
func (q *spillQueue) giveBack(idx []int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(append([]int(nil), idx...), q.pending...)
	q.held -= len(idx)
}

// resolved says n jobs a worker held have their row.
func (q *spillQueue) resolved(n int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.held -= n
	q.settleLocked()
}

// leave says a worker stopped pulling.
func (q *spillQueue) leave() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.workers--
	q.settleLocked()
}

// rest takes what no worker took, in job order.
func (q *spillQueue) rest() []int {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.pending
	q.pending = nil
	sort.Ints(out)
	return out
}

// runSpill runs the queue: one warm worker here and the plan's node workers, until every job has a row. Jobs that nobody could
// take (the warm side stopped and every node was out of reach) are left queued, with the reason, for a --resume.
func (s *batchState) runSpill(ctx context.Context, cfg config.Config, runner Runner, jobs []BatchJob, todo []int, warm WarmFunc, plan *spillPlan) {
	q := newSpillQueue(todo, 1+plan.workers)
	q.unit = jobs[todo[0]].Edit != nil
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.spillLocal(ctx, runner, jobs, todo, warm, q, plan.localBusy)
	}()
	for k := 0; k < plan.workers; k++ {
		node := plan.nodes[k%len(plan.nodes)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.spillRemote(ctx, cfg, runner, jobs, q, node)
		}()
	}
	wg.Wait()
	rest := q.rest()
	if len(rest) == 0 {
		return
	}
	why := "not started: neither this machine's warm session nor a roster node had a place for this job (the lines above say why); run the batch again with --resume to render it"
	if ctx.Err() != nil {
		why = notStartedInterrupted
	}
	for _, i := range rest {
		s.final(i, BatchRow{Out: jobs[i].Out, Seed: jobs[i].Seed, Verdict: VerdictQueued, Error: why})
	}
}

// spillLaneFree waits until this machine's lane reads free (the probe the router and the ticket scan use, asked every scanEvery),
// the queue has nothing left to wait for, ctx ends, or the queue's own ceiling (BatchHoldWindow) passes. It reports whether the lane
// is free and there is still a queue to work.
func (s *batchState) spillLaneFree(ctx context.Context, runner Runner, req core.Request, q *spillQueue) bool {
	deadline := time.Now().Add(BatchHoldWindow())
	for ctx.Err() == nil {
		select {
		case <-q.done:
			return false
		default:
		}
		if v := core.ProbeLane(ctx, runner, req); v.Free && !v.Unjudged {
			return true
		}
		if time.Now().After(deadline) {
			s.say(fmt.Sprintf("spill: this machine's lane stayed held for %s; the rest of the queue is left to the nodes", shortDuration(BatchHoldWindow())))
			return false
		}
		if !pauseOrDone(ctx, scanEvery, q.done) {
			return false
		}
	}
	return false
}

// spillLocal is the warm side: it takes a chunk, renders it in one warm session, and takes the next, until the queue is empty. When
// the lane was busy at the start (localBusy) it first waits for the lane while the node workers pull, and again after a chunk that
// found the lane taken again.
func (s *batchState) spillLocal(ctx context.Context, runner Runner, jobs []BatchJob, todo []int, warm WarmFunc, q *spillQueue, localBusy bool) {
	defer q.leave()
	busy := 0
	probe := batchRequest(jobs[todo[0]], s.o.Family, s.o.Placement)
	waitLane := localBusy
	for ctx.Err() == nil {
		if waitLane {
			if !s.spillLaneFree(ctx, runner, probe, q) {
				return
			}
			s.say("spill: this machine's lane is free; its warm session joins the queue")
			waitLane = false
		}
		chunk := q.takeChunk()
		if len(chunk) == 0 {
			return
		}
		sub := make([]BatchJob, len(chunk))
		for k, i := range chunk {
			sub[k] = jobs[i]
		}
		s.say(fmt.Sprintf("spill: this machine's warm session takes %d job(s)", len(chunk)))
		rows, deferred, err := warm(ctx, sub, 0)
		if deferred && err != nil {
			// The lane was held past the pipeline's window and nothing ran: the chunk goes back for the nodes.
			q.giveBack(chunk)
			if localBusy {
				// The lane was taken again between the probe and the lease: wait for it as before, the nodes keep pulling.
				s.say(fmt.Sprintf("spill: this machine's lane was taken again (%v); the %d job(s) went back to the queue, waiting for the lane", err, len(chunk)))
				waitLane = true
				continue
			}
			if busy++; busy >= spillLocalBusyAsks {
				s.say(fmt.Sprintf("spill: this machine's lane stayed held (%v); the rest of the queue is left to the nodes", err))
				return
			}
			s.say(fmt.Sprintf("spill: this machine's lane is held (%v); the %d job(s) went back to the queue, asking again in %s", err, len(chunk), spillPause))
			if !sleepCtx(ctx, spillPause) {
				return
			}
			continue
		}
		busy = 0
		// The CLI's warm call keeps the items of every chunk in order; WarmJobs lines up with them chunk by chunk.
		s.b.WarmJobs = append(s.b.WarmJobs, chunk[:min(len(rows), len(chunk))]...)
		for k, i := range chunk {
			s.final(i, warmRowFor(ctx, jobs[i], rows, k, false, err, s.self))
		}
		q.resolved(len(chunk))
		if err != nil {
			// The session stopped (ComfyUI unusable, a full disk): another chunk would meet the same. The nodes take the rest.
			if s.b.Err == nil {
				s.b.Err = err
			}
			s.say(fmt.Sprintf("spill: this machine's warm session stopped (%v); the rest of the queue is left to the nodes", err))
			return
		}
	}
}

// spillRemote is a node's worker: one job at a time through the single-render call pinned to that node. A node with no place
// for the job gives it back and is asked again after a pause; a node that can never take it, or that stays out of reach for the
// queue's own ceiling, stops pulling.
func (s *batchState) spillRemote(ctx context.Context, cfg config.Config, runner Runner, jobs []BatchJob, q *spillQueue, node spillNode) {
	defer q.leave()
	var since time.Time
	waits := 0
	for ctx.Err() == nil {
		i, ok := q.takeOne()
		if !ok {
			return
		}
		req := batchRequest(jobs[i], s.o.Family, s.o.Placement)
		w := Wait{Window: -1, RosterFile: s.o.RosterFile}
		if s.o.Notify != nil {
			w.Notify = func(line string) { s.say(fmt.Sprintf("job %d: %s", i, line)) }
		}
		start := time.Now()
		res := batchCall(ctx, cfg, runner, req, RouteRemote, []string{node.base}, w)
		if !res.OK && res.Deferred && ctx.Err() == nil {
			if res.Meta.ErrClass == core.ErrClassNoNodeConfigured {
				q.giveBack([]int{i})
				s.say(fmt.Sprintf("spill: node %s cannot take this batch (%s); its worker stops and the job goes back to the queue", node.shown, res.Reason))
				return
			}
			if noPlaceNow(res) {
				q.giveBack([]int{i})
				if since.IsZero() {
					since = time.Now()
				}
				if time.Since(since) >= ticketMaxWait {
					s.ceiling.Store(true)
					s.say(fmt.Sprintf("spill: node %s had no place for a job for %s; its worker stops", node.shown, shortDuration(ticketMaxWait)))
					return
				}
				pause := batchWaitSteps[min(waits, len(batchWaitSteps)-1)]
				waits++
				s.say(fmt.Sprintf("spill: node %s has no place for job %d yet (%s); the job went back to the queue, asking again in %s", node.shown, i, res.Reason, pause))
				if !pauseOrDone(ctx, pause, q.done) {
					return
				}
				continue
			}
		}
		since, waits = time.Time{}, 0
		s.final(i, s.rowFromResult(ctx, jobs[i], res, start))
		q.resolved(1)
	}
}

// pauseOrDone waits d and reports true, or reports false at once when ctx ends or the queue is settled (nothing left to ask for).
func pauseOrDone(ctx context.Context, d time.Duration, done <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-done:
		return false
	}
}
