package agent

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"time"
)

// rowHash is the chain link: the SHA-256 of one row exactly as written (the JSON
// bytes, without the newline).
func rowHash(row []byte) string {
	sum := sha256.Sum256(row)
	return hex.EncodeToString(sum[:])
}

// newRunID names one chained run: a UTC timestamp plus 8 random hex digits, so runs
// from concurrent processes never share a chain.
func newRunID() string {
	var r [4]byte
	_, _ = rand.Read(r[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(r[:])
}

// WithChain turns on the hash chain for this trail (register SF-08): every row it
// writes from now on carries run_id, seq and prev_sha256, and EndRun closes the run.
// Set once at build time, before any row. A nil trail stays nil.
func (l *AuditLog) WithChain(runID string) *AuditLog {
	if l != nil {
		l.runID = runID
	}
	return l
}

// EndRun closes a chained run with a run_end row carrying the run's decision count
// and the hash of its last row. Without it the run verifies as OPEN, which `verify
// --strict` fails. It does not make a cut tail visible on its own: removing the last
// rows AND the run_end leaves an open run, indistinguishable from a crash (hence
// --strict). A no-op on a nil or unchained trail, on a run that wrote no decision (a
// read-only run leaves the trail untouched), and on a second call.
func (l *AuditLog) EndRun() error {
	if l == nil || l.runID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.seq == 0 {
		return nil
	}
	if err := l.appendLocked(auditEntry{TS: time.Now().Unix(), Kind: "run_end", Count: l.seq, Head: l.prev}); err != nil {
		return err
	}
	l.closed = true
	return nil
}

// ChainBreak is one place a run's chain does not hold, named by run and seq, or, for
// a line that is not a JSON row, by its line number.
type ChainBreak struct {
	RunID   string `json:"run_id"`
	Seq     int    `json:"seq,omitempty"`
	Line    int    `json:"line,omitempty"`
	Problem string `json:"problem"`
}

// VerifyReport is what VerifyAuditFile found.
type VerifyReport struct {
	Runs      int          `json:"runs"`      // chained runs seen
	Chained   int          `json:"chained"`   // chained rows (run_end rows included)
	Unchained int          `json:"unchained"` // rows written with the chain off
	Open      []string     `json:"open"`      // runs with no run_end (a crash, or a removed tail)
	Late      []string     `json:"late"`      // runs with chained rows after their run_end (an abandoned tool)
	Breaks    []ChainBreak `json:"breaks"`    // the first break of each broken run
}

// VerifyAuditFile checks every chained run in an audit trail: seq runs 1, 2, 3 …
// without a gap, each row's prev_sha256 is the hash of the run's previous row as
// written, and the run_end's count and head match the rows before it. An edit, a
// deletion or a reorder inside a run breaks the chain at the first row after it; only
// the first break of each run is reported (later rows of a broken run cannot be
// judged). A row that still chains after its run's run_end is LATE (a tool goroutine
// abandoned at a timeout can write after the door closed the run), not broken.
// NOT detectable, by design of an unkeyed per-run chain: a cut tail together with its
// run_end (reported OPEN, like a crash; `verify --strict` fails open runs), a whole
// run removed, edits to a run_end's other fields, and a re-chained edit. Unchained
// rows are counted, not judged.
func VerifyAuditFile(path string) (VerifyReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return VerifyReport{}, err
	}
	defer f.Close()
	type runState struct {
		expect     int
		last       string
		ended, bad bool
	}
	rep := VerifyReport{Open: []string{}, Late: []string{}, Breaks: []ChainBreak{}}
	runs := map[string]*runState{}
	late := map[string]bool{}
	brk := func(id string, seq int, st *runState, problem string) {
		st.bad = true
		rep.Breaks = append(rep.Breaks, ChainBreak{RunID: id, Seq: seq, Problem: problem})
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e auditEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			rep.Breaks = append(rep.Breaks, ChainBreak{RunID: "(unparseable)", Line: line, Problem: fmt.Sprintf("line %d is not a JSON row: %v", line, err)})
			continue
		}
		if e.RunID == "" {
			rep.Unchained++
			continue
		}
		rep.Chained++
		st := runs[e.RunID]
		if st == nil {
			st = &runState{expect: 1}
			runs[e.RunID] = st
		}
		if st.bad {
			continue
		}
		switch {
		case e.Seq != st.expect:
			brk(e.RunID, e.Seq, st, fmt.Sprintf("seq %d where %d was expected: a row was removed, reordered or inserted", e.Seq, st.expect))
		case e.Prev != st.last:
			brk(e.RunID, e.Seq, st, fmt.Sprintf("prev_sha256 does not match row %d as written: that row was edited", e.Seq-1))
		case e.Kind == "run_end" && (e.Count != e.Seq-1 || e.Head != e.Prev):
			brk(e.RunID, e.Seq, st, fmt.Sprintf("run_end says %d rows ending at %.12s, the chain has %d ending at %.12s", e.Count, e.Head, e.Seq-1, e.Prev))
		}
		if st.bad {
			continue
		}
		if st.ended {
			late[e.RunID] = true // chains, but after the close
		}
		st.last, st.expect = rowHash(raw), e.Seq+1
		if e.Kind == "run_end" {
			st.ended = true
		}
	}
	if err := sc.Err(); err != nil {
		return rep, err
	}
	rep.Runs = len(runs)
	for id, st := range runs {
		if !st.ended && !st.bad {
			rep.Open = append(rep.Open, id)
		}
	}
	sort.Strings(rep.Open)
	for id := range late {
		rep.Late = append(rep.Late, id)
	}
	sort.Strings(rep.Late)
	return rep, nil
}

// EndAudit closes the run on a chained trail (SF-08). Doors call it once the run is
// over; a failure is logged, never fatal: the run already happened, and verify then
// reports the run open.
func (r *BuildResult) EndAudit() {
	if r == nil || r.Audit == nil {
		return
	}
	if err := r.Audit.EndRun(); err != nil {
		log.Printf("agent broker: closing the audit chain failed (verify will report this run open): %v", err)
	}
}
