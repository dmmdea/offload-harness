// intent.go — the delegator-death durability half of the consolidated-queue
// decision (operator-approved Option A, 2026-08-27): each delegator PERSISTS
// its remote-dispatch intent before work leaves the box, and a later process
// RECOVERS results for jobs whose delegator died mid-poll. Push placement is
// unchanged — this is a ledger and one recovery path, not a queue inversion
// (Option B stays parked until real delegation volume argues for it).
//
// The ledger answers exactly one question after a crash: WHICH job ids were
// acked by WHICH nodes and never observed reaching a terminal state? Everything
// else (contract bytes, placements, retries) stays where it always lived — the
// node still holds the job in its in-memory store, so recovery is one poll,
// not a re-run. A node that restarted loses that store; recovery then records
// the loss honestly instead of pretending.
//
// File shape: <state-root>/delegate-intent.jsonl, append-only events —
//
//	{"e":"d","job":"agd-…","base":"http://…","goal":"…","ts":170…}   acked dispatch
//	{"e":"ok","job":"agd-…","note":"…"}                              terminal observed
//
// A job is OPEN when its newest event is "d". Recovered results land as
// <state-root>/delegate-recovered/<job>.json for the operator (surfaced by a
// log line per recovery); the poll happened, the work is not lost.
package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const (
	// intentMaxAge bounds recovery: a node's in-memory job store cannot
	// plausibly still hold a two-day-old job, and polling forever for it
	// would keep dead entries alive. Older OPEN entries are marked expired.
	intentMaxAge = 48 * time.Hour
	// intentWarnLines: past this the recovery pass LOGS the size instead of
	// compacting. Deliberate (round-1 diff review): the state root is shared
	// by MANY concurrent harness processes, and any read-then-rename compact
	// races their O_APPEND writes — an append landing in the rename window is
	// LOST, which is exactly the "acked but unrecoverable" hole this file
	// exists to close. At ~200 bytes per remote dispatch the file takes years
	// to matter; when it does, the operator truncates it cold.
	intentWarnLines = 20000
)

type intentEvent struct {
	E    string `json:"e"`
	Job  string `json:"job"`
	Base string `json:"base,omitempty"`
	Goal string `json:"goal,omitempty"`
	Note string `json:"note,omitempty"`
	TS   int64  `json:"ts,omitempty"`
}

// intentLedger is the per-delegator append-only intent file. A nil ledger is
// valid and inert: every method no-ops, so a box whose state root cannot be
// resolved keeps delegating exactly as before — durability is an addition,
// never a new way for dispatch to fail.
type intentLedger struct {
	mu   sync.Mutex
	path string
}

// openIntentLedger resolves the machine state root. Failure returns nil (inert).
func openIntentLedger(cfg config.Config) *intentLedger {
	root, err := gpulease.ResolveStateRoot(cfg.StateDir)
	if err != nil {
		return nil
	}
	if mkerr := os.MkdirAll(root, 0o755); mkerr != nil {
		return nil
	}
	return &intentLedger{path: filepath.Join(root, "delegate-intent.jsonl")}
}

func (l *intentLedger) append(ev intentEvent) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

// dispatched records an ACKED remote dispatch — called only after the node's
// 202, because an unacked job cannot be orphaned anywhere.
func (l *intentLedger) dispatched(jobID, base, goal string) {
	if len(goal) > 120 {
		cut := goal[:120]
		// Truncate on a rune boundary: a byte slice through a multi-byte
		// character would put invalid UTF-8 into the ledger line.
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		goal = cut
	}
	l.append(intentEvent{E: "d", Job: jobID, Base: base, Goal: goal, TS: time.Now().Unix()})
}

// done closes a job: a terminal answer was OBSERVED by this process (done,
// error, positive 404 denial, auth rejection). Not called on cancellation or
// on an owned-job poll deadline — those are precisely the orphan shapes the
// recovery pass exists for.
func (l *intentLedger) done(jobID, note string) {
	l.append(intentEvent{E: "ok", Job: jobID, Note: note})
}

// openIntents folds the ledger into the still-open set, newest base last.
func readOpenIntents(path string) (map[string]intentEvent, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]intentEvent{}, 0, nil
		}
		return nil, 0, err
	}
	open := map[string]intentEvent{}
	lines := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines++
		var ev intentEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Job == "" {
			continue
		}
		switch ev.E {
		case "d":
			open[ev.Job] = ev
		case "ok":
			delete(open, ev.Job)
		}
	}
	return open, lines, nil
}

var recoverOnce sync.Once

// maybeRecoverOrphans runs the recovery pass once per process, in the
// background, on the first delegate use. It never blocks or fails a Run.
func maybeRecoverOrphans(cfg config.Config) {
	recoverOnce.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			n, err := RecoverOrphans(ctx, cfg)
			if err != nil {
				log.Printf("delegate: orphan recovery: %v", err)
				return
			}
			if n > 0 {
				log.Printf("delegate: recovered %d orphaned remote result(s) — see <state-root>/delegate-recovered/", n)
			}
		}()
	})
}

// RecoverOrphans polls every still-open intent once and files what it finds.
// Returns how many results were recovered to disk. Exported for tests and for
// any future operator surface; the automatic trigger is maybeRecoverOrphans.
func RecoverOrphans(ctx context.Context, cfg config.Config) (int, error) {
	root, err := gpulease.ResolveStateRoot(cfg.StateDir)
	if err != nil {
		return 0, err
	}
	ledger := &intentLedger{path: filepath.Join(root, "delegate-intent.jsonl")}
	open, lines, err := readOpenIntents(ledger.path)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for jobID, ev := range open {
		if ctx.Err() != nil {
			break
		}
		age := time.Since(time.Unix(ev.TS, 0))
		if ev.TS > 0 && age > intentMaxAge {
			ledger.done(jobID, "expired unrecovered after "+age.Truncate(time.Hour).String())
			continue
		}
		state, data, jobErr, status, perr := pollJobOnce(ctx, cfg, ev.Base, jobID)
		switch {
		case perr != nil:
			// Node unreachable right now: leave open; a later pass retries.
		case status == http.StatusNotFound:
			ledger.done(jobID, "node no longer holds the job (restarted?) — result lost")
		case status == http.StatusUnauthorized:
			ledger.done(jobID, "auth rejected at recovery")
		case status == http.StatusOK && (state == "done" || state == "error"):
			outDir := filepath.Join(root, "delegate-recovered")
			if mkerr := os.MkdirAll(outDir, 0o755); mkerr != nil {
				log.Printf("delegate: recovery of %s: %v — leaving open for the next pass", jobID, mkerr)
				continue
			}
			envelope, _ := json.Marshal(map[string]any{
				"job_id": jobID, "base": ev.Base, "goal": ev.Goal,
				"state": state, "data": json.RawMessage(data), "error": jobErr,
				"dispatched_ts": ev.TS, "recovered_at": time.Now().Format(time.RFC3339),
			})
			if werr := os.WriteFile(filepath.Join(outDir, jobID+".json"), envelope, 0o644); werr != nil {
				log.Printf("delegate: recovery of %s: %v — leaving open for the next pass", jobID, werr)
				continue
			}
			ledger.done(jobID, "recovered to delegate-recovered/"+jobID+".json")
			recovered++
		default:
			// accepted/running: the node is still working it. Leave open.
		}
	}
	if lines > intentWarnLines {
		log.Printf("delegate: intent ledger %s has %d lines — truncate it while no harness process is running if it bothers you (never compacted automatically; see intentWarnLines)", ledger.path, lines)
	}
	return recovered, nil
}

// pollJobOnce is runner.pollOnce lifted to package level so the recovery pass
// (which has no runner) polls by the SAME rules. The runner method delegates
// here — one poll implementation, two callers.
func pollJobOnce(ctx context.Context, cfg config.Config, base, jobID string) (state string, data json.RawMessage, jobErr string, status int, perr error) {
	return pollJobOnceAt(ctx, cfg, strings.TrimRight(strings.TrimSpace(base), "/")+"/fleet/jobs/"+jobID)
}

// pollJobOnceAt polls an EXPLICIT job URL — the queue holder's results route
// (ADR 0030) shares the wire shape but not the push path's URL layout.
func pollJobOnceAt(ctx context.Context, cfg config.Config, u string) (state string, data json.RawMessage, jobErr string, status int, perr error) {
	rctx, cancel := context.WithTimeout(ctx, pollRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return "", nil, "", 0, err
	}
	if cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
	}
	resp, err := fleetClient.Do(req)
	if err != nil {
		return "", nil, "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
	if err != nil {
		return "", nil, "", 0, err
	}
	var wire struct {
		State string          `json:"state"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if resp.StatusCode == http.StatusOK {
		if uerr := json.Unmarshal(body, &wire); uerr != nil {
			return "", nil, "", 0, fmt.Errorf("job poll %s: not JSON: %w", u, uerr)
		}
	}
	return wire.State, wire.Data, wire.Error, resp.StatusCode, nil
}
