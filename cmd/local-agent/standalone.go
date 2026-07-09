package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/trajectory"
)

// goalItem is one entry in the standalone goal queue. The queue is JSONL: each
// line is {"id":"...","goal":"..."} OR — for convenience — a bare line of goal
// text (then the id defaults to the line number). Blank lines and #-comments skip.
type goalItem struct {
	ID   string `json:"id"`
	Goal string `json:"goal"`
}

// readGoalQueue parses the JSONL goal queue leniently. A bufio.Reader (not a
// Scanner) is used so a single very long goal line cannot abort the whole drain
// with ErrTooLong. Each non-blank, non-#-comment line is parsed three ways: a
// well-formed object with a goal is used as-is; a non-JSON line is taken as bare
// goal text; a well-formed object with an EMPTY goal is operator error and skipped
// (never fed raw JSON to the model).
func readGoalQueue(path string) ([]goalItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var goals []goalItem
	r := bufio.NewReader(f)
	n := 0
	for {
		raw, rerr := r.ReadString('\n')
		if line := strings.TrimSpace(raw); line != "" && !strings.HasPrefix(line, "#") {
			n++
			var g goalItem
			switch jerr := json.Unmarshal([]byte(line), &g); {
			case jerr != nil:
				g = goalItem{Goal: line} // a bare line of goal text
			case strings.TrimSpace(g.Goal) == "":
				fmt.Fprintf(os.Stderr, "[standalone] WARN: queue entry %d is a JSON object with an empty goal — skipped\n", n)
				g.Goal = "" // mark for skip below
			}
			if g.Goal != "" {
				if g.ID == "" {
					g.ID = fmt.Sprintf("g%d", n)
				}
				goals = append(goals, g)
			}
		}
		if rerr != nil {
			break // io.EOF (the final line, if any, was handled above) or a read error
		}
	}
	return goals, nil
}

// traceRecord is the per-goal trace written to disk for supervision + audit.
type traceRecord struct {
	ID         string      `json:"id"`
	Goal       string      `json:"goal"`
	StopReason string      `json:"stop_reason"`
	Steps      int         `json:"steps"`
	Output     string      `json:"output"`
	Error      string      `json:"error,omitempty"`
	Transcript []agent.Msg `json:"transcript"`
}

// safeID sanitizes a goal id for use as a trace filename. The id is operator-
// supplied (not model-supplied), but guard against path escape AND Windows
// reserved device names (con/nul/com1…), which would silently lose the trace.
func safeID(id string, idx int) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, id)
	clean = strings.Trim(clean, ".")
	base := strings.ToLower(clean) // the device check is on the name before any extension
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if clean == "" || isWinReserved(base) {
		return fmt.Sprintf("g%d", idx)
	}
	return clean
}

// isWinReserved reports whether name is a Windows reserved device name (which a
// file of that name resolves to, regardless of extension).
func isWinReserved(name string) bool {
	switch name {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(name) == 4 && (strings.HasPrefix(name, "com") || strings.HasPrefix(name, "lpt")) && name[3] >= '1' && name[3] <= '9'
}

// standaloneOpts carries the standalone drainer's configuration (the envelope
// itself is baked into the pre-built loop passed alongside).
type standaloneOpts struct {
	queuePath      string
	tracesDir      string
	askQueuePath   string
	worktree       string // resolved RW worktree (empty if read-only) — for the outside-worktree guard
	checkpointPath string // --resume completion log
	goalTimeout    time.Duration
	resume         bool

	// P6 flywheel capture (off by default; sampled; best-effort — never affects the run).
	captureEnabled   bool
	captureRate      float64
	captureQueuePath string
	envelope         []string // capabilities granted this run (read/write/fetch/shell)
}

// toolSeq extracts the ordered tool-call names from a transcript — the trajectory
// shape a P6 judge scores (which tools, in what order).
func toolSeq(transcript []agent.Msg) []string {
	var seq []string
	for _, m := range transcript {
		for _, tc := range m.ToolCalls {
			seq = append(seq, tc.Name)
		}
	}
	return seq
}

// readCheckpoint returns the set of goal ids marked COMPLETED (StopReason "done")
// in the checkpoint file — empty if absent. Only successful goals are recorded, so
// a resumed run retries anything that errored or hit its step budget.
func readCheckpoint(path string) map[string]bool {
	done := map[string]bool{}
	if path == "" {
		return done
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return done
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.ID != "" {
			done[e.ID] = true
		}
	}
	return done
}

// appendCheckpoint records a completed goal id so a --resume re-run skips it.
// Best-effort: a checkpoint write failure must not abort the drain (at worst the
// goal re-runs on a later resume).
func appendCheckpoint(path, id string) {
	if path == "" {
		return
	}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		defer f.Close()
		b, _ := json.Marshal(map[string]string{"id": id})
		_, _ = f.Write(append(b, '\n'))
	}
}

// runStandalone drains the goal queue UNATTENDED: each goal runs through the SAME
// loop (the fixed pre-authorization envelope = the capability flags this run was
// started with), bounded by the per-goal wall-clock budget (and the optional
// whole-drain deadline carried by ctx). The broker deny-and-queues anything
// outside the envelope, parking it in the ask-queue for later human review; every
// goal's full transcript is written to a trace file. One goal's failure/timeout
// never aborts the run — only ctx cancellation (Ctrl-C / the total-timeout) stops
// the drain. With --resume, goals already completed (per the checkpoint) are
// skipped and each completion is recorded, so a crashed drain resumes instead of
// re-executing side-effecting goals; without it, a re-run reprocesses the queue.
// Resume is goal-granular, NOT transactional: only StopReason "done" goals are
// checkpointed, so an interrupted goal re-runs IN FULL on resume (over any partial
// side effects), and the skip matches on the goal id — give each goal an explicit
// id (bare-text goals fall back to positional ids that shift if the queue changes).
func runStandalone(ctx context.Context, loop *agent.Loop, o standaloneOpts) error {
	// Traces + checkpoint + the trajectory capture queue are supervision/resume/
	// learning records — keep them OUTSIDE a writable worktree, or a write/shell
	// goal could forge or destroy them via the cage path (mirrors the builder's
	// audit/ask-queue guard).
	guarded := []struct{ name, path string }{{"traces dir", o.tracesDir}, {"checkpoint", o.checkpointPath}}
	if o.captureEnabled {
		guarded = append(guarded, struct{ name, path string }{"trajectory queue", o.captureQueuePath})
	}
	if o.worktree != "" {
		for _, p := range guarded {
			if p.path == "" {
				continue
			}
			if pAbs, e := filepath.Abs(p.path); e == nil {
				if rel, e2 := filepath.Rel(o.worktree, pAbs); e2 == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return fmt.Errorf("%s %q is inside the worktree %q (a goal could clobber the record); choose a path outside it", p.name, p.path, o.worktree)
				}
			}
		}
	}
	goals, err := readGoalQueue(o.queuePath)
	if err != nil {
		return fmt.Errorf("reading goal queue: %w", err)
	}
	if len(goals) == 0 {
		fmt.Fprintln(os.Stderr, "[standalone] goal queue empty — nothing to do")
		return nil
	}
	if err := os.MkdirAll(o.tracesDir, 0o755); err != nil {
		return fmt.Errorf("creating traces dir: %w", err)
	}

	var doneSet map[string]bool
	if o.resume {
		doneSet = readCheckpoint(o.checkpointPath)
		fmt.Fprintf(os.Stderr, "[standalone] resume ON — %d completed goal(s) will be skipped (checkpoint: %s)\n", len(doneSet), o.checkpointPath)
	} else {
		fmt.Fprintln(os.Stderr, "[standalone] note: NO resume (--resume off) — a re-run reprocesses the whole queue from the top")
	}

	asksBefore := countJSONLines(o.askQueuePath)
	var done, skipped, budget, failed int
	fmt.Fprintf(os.Stderr, "[standalone] draining %d goal(s); per-goal budget=%s; traces=%s\n", len(goals), o.goalTimeout, o.tracesDir)
	for i, g := range goals {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "[standalone] interrupted — stopping the drain")
			break
		}
		gid := safeID(g.ID, i+1)
		if o.resume && doneSet[gid] {
			skipped++
			fmt.Fprintf(os.Stderr, "[standalone] %s skipped (already completed)\n", gid)
			continue
		}
		gctx, cancel := context.WithTimeout(ctx, o.goalTimeout)
		res, rerr := loop.Run(gctx, g.Goal)
		cancel()

		tr := traceRecord{ID: gid, Goal: g.Goal, StopReason: res.StopReason, Steps: res.Steps, Output: res.Output, Transcript: res.Transcript}
		if rerr != nil {
			tr.Error = rerr.Error()
		}
		tracePath, werr := writeTrace(o.tracesDir, gid, tr)
		if werr != nil {
			fmt.Fprintf(os.Stderr, "[standalone] WARN: trace write failed for %s: %v\n", gid, werr)
		}

		// P6 flywheel: sampled, best-effort trajectory capture (off unless enabled;
		// no inference here, off the request path — a failure never affects the run).
		if o.captureEnabled {
			it := trajectory.Item{
				TS: time.Now().Unix(), Schema: trajectory.SchemaVersion,
				ID: gid, Goal: g.Goal, Envelope: o.envelope,
				Tools: toolSeq(res.Transcript), Steps: res.Steps,
				StopReason: res.StopReason, Output: res.Output,
				TracePath: tracePath,
			}
			if _, cerr := trajectory.Capture(o.captureQueuePath, o.captureRate, it); cerr != nil {
				fmt.Fprintf(os.Stderr, "[standalone] WARN: trajectory capture failed for %s: %v\n", gid, cerr)
			}
		}

		switch {
		case rerr != nil:
			failed++
			fmt.Fprintf(os.Stderr, "[standalone] %s FAILED (%s): %v\n", gid, res.StopReason, rerr)
		case res.StopReason == "budget":
			budget++
			fmt.Fprintf(os.Stderr, "[standalone] %s hit the step budget (steps=%d)\n", gid, res.Steps)
		default:
			done++
			if o.resume {
				appendCheckpoint(o.checkpointPath, gid) // record completion so a re-run skips it
			}
			fmt.Fprintf(os.Stderr, "[standalone] %s done (steps=%d)\n", gid, res.Steps)
		}
	}
	asksQueued := countJSONLines(o.askQueuePath) - asksBefore
	hint := ""
	if asksQueued > 0 && o.askQueuePath != "" {
		hint = " (review: " + o.askQueuePath + ")"
	}
	fmt.Fprintf(os.Stderr, "[standalone] complete: done=%d skipped=%d budget=%d failed=%d | asks queued=%d%s | traces=%s\n",
		done, skipped, budget, failed, asksQueued, hint, o.tracesDir)
	return nil
}

// writeTrace writes the per-goal trace and RETURNS the path it wrote, so callers
// (e.g. the P6 capture hook) reference the real file rather than reconstruct the
// name by convention.
func writeTrace(dir, id string, tr traceRecord) (string, error) {
	path := filepath.Join(dir, id+".json")
	b, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		return path, err
	}
	return path, os.WriteFile(path, b, 0o644)
}

// countJSONLines returns the number of newline-terminated lines in a file (0 if
// absent). Used to count asks parked during this run.
func countJSONLines(path string) int {
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}
