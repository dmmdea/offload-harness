package ledger

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
//
// A REFUSAL row (Entry.Refusal) is not a job either: it is a node's capacity refusal at dispatch, a place in
// line. The refused placement did no work, the delegator waited and placed the subtask again, and the subtask's
// outcome is another row, so every job counter skips the refusal row the way it skips a marker. The failure
// register counts it apart, as a refused attempt (isRefusedAttempt), so queue pressure stays visible without
// reading as a job or a failure. ParentJobIDs still lists a refusal row's id: no inner row names one, because
// the writer marks only a parentless `agent_delegate` row.

// PhaseStarted is Entry.Phase on the dispatch marker a delegator writes when it
// hands a job to a seat (ADR 0064). A marker is a record that a job BEGAN, not a
// job: it carries the job's id but no outcome, and the finished row with the same
// id is the job. Every reader above skips it, and ParentJobIDs must too — a
// marker looks exactly like a parent row (a job id, no parent), so an inner row
// whose real parent never landed would otherwise be dropped as an inner row of a
// parent that is only a marker, and the job would vanish from every count.
const PhaseStarted = "started"

// ParentJobIDs returns the job ids of the rows that can be an inner row's
// parent: every row that carries a job id and is not itself an inner row.
func ParentJobIDs(entries []Entry) map[string]bool {
	ids := map[string]bool{}
	for _, e := range entries {
		if e.JobID != "" && e.ParentJobID == "" && e.Phase != PhaseStarted {
			ids[e.JobID] = true
		}
	}
	return ids
}

// CountsAsJob reports whether a row is a job for a counter: every row except a
// dispatch marker, a refusal row and an inner row whose parent is present.
func CountsAsJob(e Entry, parents map[string]bool) bool {
	return !e.Refusal && isOwnRecord(e, parents)
}

// isRefusedAttempt reports whether a row is a refused attempt for the reader that counts those apart (the
// failure register): a refusal row that is a record of its own. An inner row is never counted, whatever else it
// carries, so a refusal mark on one does not make it a refused attempt.
func isRefusedAttempt(e Entry, parents map[string]bool) bool {
	return e.Refusal && isOwnRecord(e, parents)
}

// isOwnRecord is the job rule without the refusal mark: every row except a dispatch marker and an inner row
// whose parent is present.
func isOwnRecord(e Entry, parents map[string]bool) bool {
	if e.Phase == PhaseStarted {
		return false
	}
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
