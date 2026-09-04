package fleetview

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// maxDelegations bounds Overview.Delegations.
const maxDelegations = 100

// delegationRowFields are the delegation-log corpus keys this view keeps
// (internal/delegate/run.go delegationLogLine, ~line 2360). Decoded LOOSELY
// (map[string]any) on purpose: this package must not import internal/delegate
// just to read its telemetry rows, and a future column should never break
// this decode.
//
// Deliberately NOT here: "goal" (or anything else pulled from the row's
// "contract"). `/api/overview` is unauthenticated (see the Security section
// of docs/systems/fleet-overview.md) — the point of this row set is
// operational telemetry about a delegation (where it landed, whether it
// passed, how long it took), never the CONTENT of the contract that was
// dispatched. An earlier revision truncated contract.goal to 120 runes and
// published it as "goal"; that put contract text on the one route this
// package serves with no auth at all, which is the exact leak class this
// list exists to keep out.
var delegationRowFields = []string{
	"ts", "job_id", "node", "seat", "placement_reason",
	"deferred", "defer_class", "acceptance_pass", "wall_ms", "error",
}

// foldDelegationLog reads today's and yesterday's delegation-log corpus
// shards (day-sharded, BaseDir()/delegation-log/YYYY-MM-DD.jsonl — see
// internal/delegate/run.go corpusPath), keeps the last maxDelegations rows,
// and turns every deferred or acceptance-failed row into an Error (deduped
// on "deleg|<job_id>"). A missing file is not an error — a fresh install or
// a delegator that has never run has no corpus yet.
func (p *Poller) foldDelegationLog() {
	dir := filepath.Join(p.cfg.BaseDir(), "delegation-log")
	now := time.Now()
	var rows []map[string]any
	for _, day := range []string{now.AddDate(0, 0, -1).Format("2006-01-02"), now.Format("2006-01-02")} {
		rows = append(rows, readDelegationShard(filepath.Join(dir, day+".jsonl"))...)
	}
	if len(rows) > maxDelegations {
		rows = rows[len(rows)-maxDelegations:]
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.delegations = rows
	for _, row := range rows {
		deferred := boolv(row, "deferred")
		acceptancePass := boolv(row, "acceptance_pass")
		if !deferred && acceptancePass {
			continue
		}
		jobID := str(row, "job_id")
		key := "deleg|" + jobID
		if p.seenErr[key] {
			continue
		}
		p.seenErr[key] = true
		msg := str(row, "error")
		if msg == "" {
			msg = str(row, "placement_reason")
		}
		defClass := str(row, "defer_class")
		if defClass != "" {
			msg = defClass + ": " + msg
		}
		p.appendErrorLocked(Error{
			At: int64(num(row, "ts")), Severity: "warn", Node: str(row, "node"),
			Source: "delegation", Message: msg,
		})
	}
}

// readDelegationShard loosely decodes every JSONL line of path, keeping only
// delegationRowFields — never row["contract"] (see that var's doc comment for
// why "goal" specifically must never ride along). A missing file returns nil,
// nil error handling — never surfaced as a probe/job error, since a fresh
// install has no corpus yet.
func readDelegationShard(path string) []map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024) // corpus lines carry the whole contract; can run large
	for sc.Scan() {
		var full map[string]any
		if err := json.Unmarshal(sc.Bytes(), &full); err != nil {
			continue
		}
		row := map[string]any{}
		for _, k := range delegationRowFields {
			if v, ok := full[k]; ok {
				row[k] = v
			}
		}
		out = append(out, row)
	}
	return out
}
