package delegate

// The rows a cut leaves behind (ADR 0065 decision 2: the wire result, the ledger row and
// the corpus row say the same thing). A cut that ends a subtask which had already made an
// attempt, and one whose goroutine the call gave up on, were both invisible to the
// telemetry — the readbacks that look for the deadline wording in the rows (the per-call
// wall check, "the only refusal wording is call deadline reached") could not see them.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// TestAWaitCutAfterARefusalIsRecordedLikeAnyOtherCut: a node refuses the first dispatch
// (503), the subtask waits for capacity, and the call ends the wait. The published result
// is the call-deadline defer; the ledger and the corpus must say so too, under the job id
// the caller was given — a row of its own, because none of the refused attempt's rows can:
// theirs say "dispatch ... 503" and belong to a job no node ever held.
func TestAWaitCutAfterARefusalIsRecordedLikeAnyOtherCut(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	url := oneRefusalThenNoRoom(t)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 30

	results, sum, _ := runWithin(t, 5*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)
	pr := results[0]
	if sum.Deferred != 1 || sum.Failed != 0 || !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") || pr.JobID == "" {
		t.Fatalf("summary %+v result %+v, want the call-deadline defer with a job id", sum, pr)
	}

	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var closing, refused int
	for _, row := range rows {
		switch {
		case strings.HasPrefix(row.Reason, deadlinePrefix):
			closing++
			if row.JobID != pr.JobID || !row.Deferred {
				t.Fatalf("the closing row = job %q deferred %v, want the job id the caller was given (%q)", row.JobID, row.Deferred, pr.JobID)
			}
		case strings.Contains(row.Reason, "status 503"):
			refused++
			if row.JobID == pr.JobID {
				t.Fatalf("a refused attempt's row carries the job id the caller was given (%q): two rows would double-count one id", pr.JobID)
			}
		}
	}
	if closing != 1 || refused < 1 {
		t.Fatalf("ledger: %d closing row(s) with the deadline wording and %d refused-attempt row(s) among %d, want exactly 1 and at least 1", closing, refused, len(rows))
	}
	var corpus int
	for _, line := range corpusLines(t, cfg) {
		if line.JobID == pr.JobID {
			corpus++
			if line.Result == nil || !strings.HasPrefix(line.Result.Reason, deadlinePrefix+"1 unfinished") || !line.Deferred {
				t.Fatalf("the corpus row for the closing job = %+v, want the call-deadline defer", line)
			}
		}
	}
	if corpus != 1 {
		t.Fatalf("%d corpus row(s) carry the job id the caller was given, want exactly 1", corpus)
	}
}

// TestASettledOutcomeThatIsNotACutKeepsItsAttemptsRow: the closing row is for a CUT only.
// A capacity wait that ran out of its own TTL after a refused attempt (no deadline in
// sight) is still recorded by the attempt's row alone, exactly as before.
func TestASettledOutcomeThatIsNotACutKeepsItsAttemptsRow(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	url := oneRefusalThenNoRoom(t)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1

	results, _, _ := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(time.Hour), nil)
	if r := results[0].Result; !r.Deferred || r.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result %+v, want the capacity wait's own TTL defer", r)
	}
	rows, _ := ledger.ReadAll(cfg.LedgerPath)
	for _, row := range rows {
		if row.JobID == results[0].JobID && strings.HasPrefix(row.Reason, "capacity wait") {
			t.Fatalf("a wait that ended on its own TTL was recorded by a closing row too: %+v", row)
		}
	}
}

// TestAnAbandonedSubtaskIsPublishedWithAJobIDAndARow: a seat that ignores its context is
// abandoned, and the caller is told its late answer "is discarded". It used to be told with
// no job id and no row — so the pass row the goroutine wrote when it finally answered (a
// finished answer, in the ledger, under an id the caller never received) contradicted the
// wire and could not be reconciled with it. The result now carries the id of the attempt
// that had not returned, the call records its own row under it at return, and the
// goroutine's late row is the SAME id: the caller can find both.
func TestAnAbandonedSubtaskIsPublishedWithAJobIDAndARow(t *testing.T) {
	cfg := testCfg(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(c) {
			<-release // deaf to ctx
		}
		return localOK(), nil
	}
	results, sum, _ := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "fast one"}, {Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), unblock)
	if sum != (Summary{Succeeded: 1, Deferred: 1}) {
		t.Fatalf("summary %+v, want one success and one abandoned subtask", sum)
	}
	ab := results[1]
	if !strings.HasPrefix(ab.JobID, "agd-") || !strings.Contains(ab.Result.Reason, "did not stop") || !strings.Contains(ab.Result.Reason, ab.JobID) {
		t.Fatalf("the abandoned subtask = job %q reason %q, want its job id on the result and in the reason", ab.JobID, ab.Result.Reason)
	}
	if ab.Node != "" || ab.Seat != "" || !ab.Unplaced {
		t.Fatalf("the abandoned subtask names node %q seat %q: the call cannot say where it was running", ab.Node, ab.Seat)
	}
	rowsFor := func() (deferred, passed int) {
		rows, _ := ledger.ReadAll(cfg.LedgerPath)
		for _, row := range rows {
			if row.JobID != ab.JobID {
				continue
			}
			if row.Deferred && strings.HasPrefix(row.Reason, deadlinePrefix+"1 unfinished") {
				deferred++
			}
			if row.AcceptanceResult == "pass" {
				passed++
			}
		}
		return
	}
	if d, p := rowsFor(); d != 1 || p != 0 {
		t.Fatalf("at return: %d call-deadline row(s) and %d pass row(s) under the abandoned job id, want the call's own row and nothing else yet", d, p)
	}
	unblock()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, p := rowsFor(); p >= 1 {
			break
		}
	}
	if d, p := rowsFor(); d != 1 || p != 1 {
		t.Fatalf("after the seat answered: %d call-deadline row(s) and %d pass row(s) under the abandoned job id, want the call's row and the goroutine's own late row, both findable by the id the caller was given", d, p)
	}
	time.Sleep(100 * time.Millisecond) // let the goroutine finish before the temp dir goes
}

// TestADroppedLatePairFrameIsSaidOnce: the PAIR emitter is fenced once a run has returned,
// so an abandoned subtask's terminal frame is dropped — and PAIR's card for it stays open
// until PAIR's own staleness sweep. That is the design (decision 3), but a drop nobody sees
// is a silent one: it is logged, once per run, naming the job.
func TestADroppedLatePairFrameIsSaidOnce(t *testing.T) {
	logs := captureLog(t)
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(c) {
			<-release // deaf to ctx
		}
		return localOK(), nil
	}
	results, _, _ := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "fast one"}, {Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), unblock)
	if strings.Contains(logs.String(), "PAIR frame") {
		t.Fatalf("a PAIR frame was reported dropped before any late frame existed: %s", logs.String())
	}
	unblock()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && !strings.Contains(logs.String(), "PAIR frame"); time.Sleep(20 * time.Millisecond) {
	}
	out := logs.String()
	if n := strings.Count(out, "PAIR frame"); n != 1 {
		t.Fatalf("%d log line(s) about a dropped PAIR frame, want exactly 1: %s", n, out)
	}
	if !strings.Contains(out, results[1].JobID) {
		t.Fatalf("the log line does not name the job (%s): %s", results[1].JobID, out)
	}
	time.Sleep(100 * time.Millisecond)
}

// TestOnlyTheFirstDroppedPairFrameOfARunIsLogged: several abandoned subtasks report late in
// one run; the log says it once (naming the first job), not once per frame — a burst of
// identical lines would bury the results it warns about.
func TestOnlyTheFirstDroppedPairFrameOfARunIsLogged(t *testing.T) {
	logs := captureLog(t)
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	r := &runner{cfg: cfg, pair: pairworkloads.New(pairworkloads.FromConfig(cfg))}
	r.shutPair()
	for _, id := range []string{"agd-late-1", "agd-late-2", "agd-late-3"} {
		pr := PlacedResult{pairModel: "seat-m", pairEngine: "engine-e", pairCreated: 1}
		r.pairTerminal(id, &pr)
	}
	out := logs.String()
	if n := strings.Count(out, "PAIR frame"); n != 1 || !strings.Contains(out, "agd-late-1") {
		t.Fatalf("%d log line(s) for three dropped frames, want exactly 1 naming the first job: %s", n, out)
	}
	if n := len(c.snapshot()); n != 0 {
		t.Fatalf("%d frame(s) reached PAIR after the run was shut", n)
	}
}
