package fleetview

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// maxDelegations bounds Overview.Delegations.
const maxDelegations = 100

// delegationRowFields are the delegation-log corpus keys this view keeps
// (internal/delegate/run.go delegationLogLine, ~line 2360) plus "goal",
// pulled from contract.goal and truncated to 120 runes. Decoded LOOSELY
// (map[string]any) on purpose: this package must not import internal/delegate
// just to read its telemetry rows, and a future column should never break
// this decode.
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
// delegationRowFields plus a truncated "goal" pulled from row["contract"].
// A missing file returns nil, nil error handling — never surfaced as a
// probe/job error, since a fresh install has no corpus yet.
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
		row["goal"] = truncateGoal(goalFromContract(full))
		out = append(out, row)
	}
	return out
}

func goalFromContract(full map[string]any) string {
	c, ok := full["contract"].(map[string]any)
	if !ok {
		return ""
	}
	return str(c, "goal")
}

// truncateGoal caps goal at 120 runes, cutting on a rune boundary — mirrors
// intentLedger.dispatched's rule (internal/delegate/intent.go).
func truncateGoal(goal string) string {
	if utf8.RuneCountInString(goal) <= 120 {
		return goal
	}
	r := []rune(goal)
	return string(r[:120])
}
