// contextbench_gen_log.go - the incident-log triage task generator.
//
// Ported to Go from contextbench/tasks/log_triage/generate.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0). The service, level and word
// tables, the line construction and the order of random draws follow the original so
// the same seed streams the same operations; the code and comments are written for
// this repository and no upstream prose is carried over.
//
// The task: a service log streams in batches, then questions arrive. A lookup asks
// for the message of the line with a given request id; a count asks how many lines
// match a level and a service. Both need the exact log, not a summary of it.
package compeval

import (
	"fmt"
	"strings"
	"time"
)

var (
	cbLogServices = []string{"auth-svc", "cache-svc", "api-gw", "db-proxy", "worker", "billing"}
	cbLogLevels   = []string{"INFO", "WARN", "ERROR"}
	// cbLogLevelWeights skew toward INFO so ERROR is rarer and the counts are not trivial.
	cbLogLevelWeights = []float64{0.6, 0.25, 0.15}
	cbLogWords        = []string{
		"timeout", "retry", "connection", "refused", "latency", "spike", "cache",
		"miss", "token", "expired", "deadlock", "rollback", "throttled", "queue",
		"backlog", "handshake", "reset", "upstream", "downstream", "shard",
		"replica", "lag", "heartbeat", "evicted", "pool", "exhausted", "quota",
	}
	cbLogEpoch = time.Date(2026, 6, 20, 14, 0, 0, 0, time.UTC)
)

type cbLogLine struct {
	level, svc, req, msg, text string
}

// GenerateLog builds one log_triage instance.
func GenerateLog(seed int64, k CBKnobs) *CBInstance {
	rng := newPyRand(uint64(seed))
	lines, perBatch, queries := k.Lines, k.LinesPerBatch, k.Queries
	log := make([]cbLogLine, lines)
	for i := 0; i < lines; i++ {
		level := cbLogLevels[rng.weightedIndex(cbLogLevelWeights)]
		svc := cbLogServices[rng.choiceIndex(len(cbLogServices))]
		req := pyHashTag(seed, "req", i)
		nWords := rng.randint(4, 9)
		ws := make([]string, nWords)
		for j := range ws {
			ws[j] = cbLogWords[rng.choiceIndex(len(cbLogWords))]
		}
		msg := strings.Join(ws, " ")
		tag := pyHashTag(seed, "msg", i)
		ts := cbLogEpoch.Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05Z")
		log[i] = cbLogLine{
			level: level, svc: svc, req: req, msg: fmt.Sprintf("%s #%s", msg, tag),
			text: fmt.Sprintf("%s [%s] %s req=%s %s #%s", ts, level, svc, req, msg, tag),
		}
	}

	var gold []CBLogQuery
	nLookup := queries / 2
	if nLookup < 1 {
		nLookup = 1
	}
	nCount := queries - nLookup
	if nCount < 1 {
		nCount = 1
	}
	take := nLookup
	if take > len(log) {
		take = len(log)
	}
	for _, i := range rng.sampleIndexes(len(log), take) {
		gold = append(gold, CBLogQuery{
			QID: len(gold), Type: "lookup",
			Prompt: fmt.Sprintf("What is the message of the log line with req=%s?", log[i].req),
			Answer: log[i].msg,
		})
	}
	for j := 0; j < nCount; j++ {
		level := cbLogLevels[rng.choiceIndex(len(cbLogLevels))]
		svc := cbLogServices[rng.choiceIndex(len(cbLogServices))]
		count := 0
		for _, ln := range log {
			if ln.level == level && ln.svc == svc {
				count++
			}
		}
		gold = append(gold, CBLogQuery{
			QID: len(gold), Type: "count",
			Prompt: fmt.Sprintf("How many [%s] log lines are from service \"%s\"?", level, svc),
			Answer: fmt.Sprint(count),
		})
	}

	var ops []CBOp
	idx := 0
	for start := 0; start < len(log); start += perBatch {
		end := start + perBatch
		if end > len(log) {
			end = len(log)
		}
		id := fmt.Sprintf("%04d#%s", idx, pyHashTag(seed, "lb", idx))
		body := fmt.Sprintf("<<<LOG-BATCH %s BEGIN -- offload this whole block this turn>>>\n", id)
		for _, ln := range log[start:end] {
			body += ln.text + "\n"
		}
		body += fmt.Sprintf("<<<LOG-BATCH %s END>>>", id)
		ops = append(ops, CBOp{Index: idx, Header: fmt.Sprintf("log batch (lines %d-%d)", start, end-1), Body: body})
		idx++
	}
	for _, q := range gold {
		body := fmt.Sprintf("QUERY %d: %s\n(Answer with an ANSWER block for qid=%d.)", q.QID, q.Prompt, q.QID)
		ops = append(ops, CBOp{Index: idx, Header: fmt.Sprintf("query %d", q.QID), Body: body})
		idx++
	}
	texts := make([]string, len(log))
	for i, ln := range log {
		texts[i] = ln.text
	}
	return &CBInstance{
		Family: CBFamilyLog,
		Seed:   seed,
		Ops:    ops,
		Gold:   CBGold{Log: gold},
		Meta: map[string]any{
			"task_family": CBFamilyLog, "seed": seed, "lines": lines, "lines_per_batch": perBatch,
			"queries": len(gold), "n_lookup": nLookup, "n_count": nCount,
			"n_batches": (len(log) + perBatch - 1) / perBatch, "n_actions": len(ops),
			"approx_log_tokens": approxTokens4(texts),
		},
	}
}
