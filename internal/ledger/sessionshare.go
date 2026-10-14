package ledger

import (
	"math"
	"sort"
)

// SessionShare is one session's card tokens split by WHERE they ran (register D-101, ADR 0087).
//
// It is the card-side placement split: of the tokens the session's own jobs put through the cards, how many ran
// on a fleet node (RemoteTokens) and how many on this box (LocalTokens). It is NOT the share gate's figure,
// cards over cards plus cloud, which needs the session's transcript for the cloud term and stays in the hook;
// the two numbers answer different questions and must not be compared.
//
// It is a FLOOR on the remote part. A row counts as remote only when it says so (a fleet job id or a node), and
// two kinds of remote work are missing from it: a cascade call routed to a node over the chat lane (the node
// writes no row, and the asker's row names neither the node nor a fleet job) reads as local, and a delegation sent
// with route "queue" writes no row on the asker's ledger at all (the claiming node's own ledger holds it, as a
// served row), so it is in neither number. A delegation pushed to a node is not a floor, whatever becomes of its
// started marker: the finished row's fleet job id comes from the node's ack, which is recorded before the marker.
//
// It reads ONE box's ledger. The asker's own totals come from the rows its delegator process wrote; the
// served_* part comes from the rows this box wrote as a fleet node, which carry the asker's session only since
// the origin headers (ADR 0087). A view across the whole fleet is a reader of several ledgers; this is not it.
type SessionShare struct {
	// Session is the origin_session of the rows; "" is every row that carries none (unattributed), never merged
	// into a named session.
	Session string `json:"session"`
	// Rows is the number of jobs the session asked for that this ledger holds (a dispatch marker and an inner
	// row are not jobs).
	Rows int `json:"rows"`
	// CardsTokens is the card work of those jobs, RemoteTokens + LocalTokens.
	CardsTokens int `json:"cards_tokens"`
	// RemoteTokens is the part that ran on a fleet node; LocalTokens the part that ran on this box.
	RemoteTokens int `json:"remote_tokens"`
	LocalTokens  int `json:"local_tokens"`
	// RemoteShare is RemoteTokens over CardsTokens, 0 when the session did no card work.
	RemoteShare float64 `json:"remote_share"`
	// ServedRows and ServedTokens are the jobs this box ran FOR the session as a fleet node (door "fleet"),
	// on this box's own ledger. They are never part of the session's own totals above, so a ledger shared by a
	// delegator and a node (one box that is both) does not count a job twice.
	ServedRows   int `json:"served_rows"`
	ServedTokens int `json:"served_tokens"`
}

// servedDoor is the door a fleet node stamps on a row it wrote for a job another box asked for (the fleet node's
// dispatchDoor). It is only as good as that stamp: a node keeps a door a payload supplies, and today none does.
const servedDoor = "fleet"

// SessionShares groups entries by session (see SessionShare). Dispatch markers and inner rows are not jobs and are
// skipped; the caller decides which run classes to keep (SessionSharesFile applies the production rule).
func SessionShares(entries []Entry) []SessionShare {
	return sessionSharesFrom(entries, ParentJobIDs(entries))
}

// sessionSharesFrom is SessionShares with the parent index of the WHOLE ledger, so a window of rows is judged
// against the jobs outside the window as the other readers do.
func sessionSharesFrom(entries []Entry, parents map[string]bool) []SessionShare {
	by := map[string]*SessionShare{}
	for _, e := range entries {
		if !CountsAsJob(e, parents) {
			continue
		}
		s := by[e.OriginSession]
		if s == nil {
			s = &SessionShare{Session: e.OriginSession}
			by[e.OriginSession] = s
		}
		// cards_tokens is always written since 0.124.0; an older row carries it as 0 and the work in the older
		// columns, which CardsTokensOf reads by the one rule.
		n := e.CardsTokens
		if n == 0 {
			n = CardsTokensOf(e)
		}
		switch {
		case e.Door == servedDoor:
			s.ServedRows++
			s.ServedTokens += n
		case e.FleetJobID != "" || e.Node != "":
			s.Rows++
			s.RemoteTokens += n
		default:
			s.Rows++
			s.LocalTokens += n
		}
	}
	out := make([]SessionShare, 0, len(by))
	for _, s := range by {
		s.CardsTokens = s.RemoteTokens + s.LocalTokens
		if s.CardsTokens > 0 {
			s.RemoteShare = math.Round(float64(s.RemoteTokens)/float64(s.CardsTokens)*1e4) / 1e4
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].CardsTokens+out[i].ServedTokens, out[j].CardsTokens+out[j].ServedTokens
		if a != b {
			return a > b
		}
		return out[i].Session < out[j].Session
	})
	return out
}

// SessionSharesFile reads the ledger at path and groups its rows by session (see SessionShares). since (unix
// seconds, 0 = all) keeps only rows from the window. Unless includeTraffic, test, bake and measurement traffic is
// left out and counted per class in the returned map (the way every production view of the ledger does, register
// F48). A missing ledger is an empty answer. Lock-free, like the other file readers.
func SessionSharesFile(path string, since int64, includeTraffic bool) ([]SessionShare, map[string]int, error) {
	all, err := ReadAll(path)
	if err != nil {
		return nil, nil, err
	}
	// The job and class indexes are the whole ledger's, so a row inside the window is judged by its parent job
	// even when the parent is older than the window.
	parents := ParentJobIDs(all)
	jobs := jobClasses(all)
	var kept []Entry
	var excluded map[string]int
	for _, e := range all {
		if since > 0 && e.TS < since {
			continue
		}
		if !includeTraffic {
			if c := classIn(e, jobs); c != "" {
				if CountsAsJob(e, parents) {
					if excluded == nil {
						excluded = map[string]int{}
					}
					excluded[c]++
				}
				continue
			}
		}
		kept = append(kept, e)
	}
	return sessionSharesFrom(kept, parents), excluded, nil
}
