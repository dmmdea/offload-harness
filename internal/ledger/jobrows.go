package ledger

import (
	"bufio"
	"encoding/json"
	"os"
)

// Inner rows and the job rule (register C-62, 0.143.0).
//
// A delegator-local job writes an INNER `agent` row (ParentJobID = the
// delegator's job id) beside the delegator's own `agent_delegate` row, which
// is the job's record (it alone knows the acceptance verdict). Every reader
// that counts JOBS — calls, outcomes, defer reasons, per-task tallies,
// TokensOut — counts the parent and skips the inner row. One exception: an
// ORPHAN inner row, whose parent row never landed (a failed write, a process
// killed between the two writes, a delegator whose ledger could not open), is
// the only record of that job and counts as the job itself.

// ParentJobIDs returns the job ids of the rows that can be an inner row's
// parent: every row that carries a job id and is not itself an inner row.
func ParentJobIDs(entries []Entry) map[string]bool {
	ids := map[string]bool{}
	for _, e := range entries {
		if e.JobID != "" && e.ParentJobID == "" {
			ids[e.JobID] = true
		}
	}
	return ids
}

// CountsAsJob reports whether a row is a job for a counter: every row except
// an inner row whose parent is present.
func CountsAsJob(e Entry, parents map[string]bool) bool {
	return e.ParentJobID == "" || !parents[e.ParentJobID]
}

// JobRows returns the rows a job counter should read: inner rows whose parent
// row is present are dropped; orphan inner rows stay (they are their job's
// only record). The input is not modified.
func JobRows(entries []Entry) []Entry {
	parents := ParentJobIDs(entries)
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if CountsAsJob(e, parents) {
			out = append(out, e)
		}
	}
	return out
}

// parentJobIDsInFile is ParentJobIDs over a ledger file, streamed (the
// summary's first pass). A missing file has no parents.
func parentJobIDsInFile(path string) (map[string]bool, error) {
	ids := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ids, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var e struct {
			JobID       string `json:"job_id"`
			ParentJobID string `json:"parent_job_id"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.JobID != "" && e.ParentJobID == "" {
			ids[e.JobID] = true
		}
	}
	return ids, sc.Err()
}
