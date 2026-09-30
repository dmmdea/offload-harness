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
//	{"e":"d","job":"agd-…","base":"http://…","goal":"…","ts":170…,"pid":1234}   acked dispatch
//	{"e":"ok","job":"agd-…","note":"…","ts":170…,"pid":1234}                     closed
//
// Every event carries the unix second it was written and the pid of the process
// that wrote it (ADR 0064): the `ok` events used to carry neither, so the ledger
// could not say which process closed a job, or when. The close notes in use:
// "terminal observed" (this process saw the job end), "withdrawn" (the node
// confirmed the job was taken back before it started), "recovered to …" (the
// recovery pass filed the result), "node no longer holds the job …", and
// "expired unrecovered after …".
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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
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
	// PID is the process that wrote the event (ADR 0064), so "which process ran
	// the recovery" and "who closed this job" can be read back.
	PID int `json:"pid,omitempty"`
}

// The close notes the delegator itself writes. Recovery's own notes stay where
// they are used.
const (
	intentNoteTerminal  = "terminal observed"
	intentNoteWithdrawn = "withdrawn"
)

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
	// Stamped HERE, on the one path every event takes, so no writer can forget
	// them (a timestamp a caller already set, such as a test's back-dated
	// dispatch, is kept).
	if ev.TS == 0 {
		ev.TS = time.Now().Unix()
	}
	if ev.PID == 0 {
		ev.PID = os.Getpid()
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
// error, positive 404 denial, auth rejection at dispatch). Not called on
// cancellation or on an owned-job poll deadline unless the node confirmed a
// withdrawal (see withdrawn) — those are precisely the orphan shapes the
// recovery pass exists for.
func (l *intentLedger) done(jobID, note string) {
	l.append(intentEvent{E: "ok", Job: jobID, Note: note})
}

// withdrawn closes a job the node CONFIRMED it took back before starting it
// (DELETE /fleet/jobs/{id}, ADR 0064): nothing is left on the node for the
// recovery pass to collect, so the intent is settled — unlike a give-up the node
// did not confirm, which stays open.
func (l *intentLedger) withdrawn(jobID string) {
	l.done(jobID, intentNoteWithdrawn)
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
	// unauthorized is the intents a node refused THIS process's credentials for,
	// by base: reported once for the pass, after the loop.
	unauthorized := map[string]int{}
	for jobID, ev := range open {
		if ctx.Err() != nil {
			break
		}
		age := time.Since(time.Unix(ev.TS, 0))
		if ev.TS > 0 && age > intentMaxAge {
			ledger.done(jobID, "expired unrecovered after "+age.Truncate(time.Hour).String())
			continue
		}
		p, perr := pollJobOnce(ctx, cfg, ev.Base, jobID)
		state, data, jobErr, status := p.State, p.Data, p.JobErr, p.Status
		switch {
		case perr != nil:
			// Node unreachable right now: leave open; a later pass retries.
		case status == http.StatusNotFound:
			ledger.done(jobID, "node no longer holds the job (restarted?) — result lost")
		case status == http.StatusUnauthorized:
			// A 401 is the node refusing THIS process's credentials (a config with
			// no fleet_auth_token, a rotated one) — a fact about the caller, not
			// about the job. Closing the intent on it destroyed the only record of
			// work the node may still finish: 36 of 61 intents on 2026-09-29, none
			// of the 61 ever recovered. It stays OPEN (a process holding the right
			// token can still collect it; the 48 h expiry bounds it) and the pass
			// says so once, below.
			unauthorized[strings.TrimRight(strings.TrimSpace(ev.Base), "/")]++
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
	if len(unauthorized) > 0 {
		total, bases := 0, make([]string, 0, len(unauthorized))
		for base, n := range unauthorized {
			total += n
			bases = append(bases, base)
		}
		sort.Strings(bases)
		more := ""
		if len(bases) > 3 {
			bases, more = bases[:3], fmt.Sprintf(" and %d more", len(bases)-3)
		}
		log.Printf("delegate: orphan recovery: %d open intent(s) answered 401 from %s%s — this process's fleet_auth_token is missing or wrong for them; they were left OPEN, not closed, so a process with the right token can still recover them (they expire after %s)",
			total, strings.Join(bases, ", "), more, intentMaxAge)
	}
	if lines > intentWarnLines {
		log.Printf("delegate: intent ledger %s has %d lines — truncate it while no harness process is running if it bothers you (never compacted automatically; see intentWarnLines)", ledger.path, lines)
	}
	return recovered, nil
}

// jobPoll is one poll's answer. A struct rather than a widening return list
// because the wall (register D-116) is the fourth fact a poll now carries and
// the three callers read different subsets of it.
type jobPoll struct {
	State  string
	Data   json.RawMessage
	JobErr string
	// WallSec is the wall the node says the RUNNING job is executing under
	// (jobWire `wall_sec`, register D-116). 0 on a node too old to publish it,
	// or on a lane that reports none — never read as "no wall".
	WallSec int
	// Progress (0.131.0) is the node's liveness report for the running job
	// (jobWire `progress`): the delegator keeps polling while LastProgressMs
	// keeps moving inside AllowanceMs, bounded by CeilingSec. nil from a node
	// that reports none.
	Progress *core.LiveProgress
	Status   int
}

// pollJobOnce is runner.pollOnce lifted to package level so the recovery pass
// (which has no runner) polls by the SAME rules. The runner method delegates
// here — one poll implementation, two callers.
func pollJobOnce(ctx context.Context, cfg config.Config, base, jobID string) (jobPoll, error) {
	return pollJobOnceAt(ctx, cfg, strings.TrimRight(strings.TrimSpace(base), "/")+"/fleet/jobs/"+jobID)
}

// pollJobOnceAt polls an EXPLICIT job URL — the queue holder's results route
// (ADR 0030) shares the wire shape but not the push path's URL layout. The
// error covers transport-level failure only; an HTTP answer of ANY status
// comes back as a jobPoll with a nil error.
func pollJobOnceAt(ctx context.Context, cfg config.Config, u string) (jobPoll, error) {
	rctx, cancel := context.WithTimeout(ctx, pollRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return jobPoll{}, err
	}
	if cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
	}
	resp, err := fleetClient.Do(req)
	if err != nil {
		return jobPoll{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
	if err != nil {
		return jobPoll{}, err
	}
	var wire struct {
		State    string             `json:"state"`
		Data     json.RawMessage    `json:"data"`
		Error    string             `json:"error"`
		WallSec  int                `json:"wall_sec"`
		Progress *core.LiveProgress `json:"progress"`
	}
	if resp.StatusCode == http.StatusOK {
		if uerr := json.Unmarshal(body, &wire); uerr != nil {
			return jobPoll{}, fmt.Errorf("job poll %s: not JSON: %w", u, uerr)
		}
	}
	return jobPoll{State: wire.State, Data: wire.Data, JobErr: wire.Error, WallSec: wire.WallSec, Progress: wire.Progress, Status: resp.StatusCode}, nil
}
