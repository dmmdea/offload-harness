package agent

// Context Pager instrument (memory-frontier R2-13) — INSTRUMENT ONLY.
//
// # What it answers, and the kill condition it is built to reach
//
// Compaction evicts tool-result payloads from the transcript to fit the budget. The open
// question is whether the agent then goes and FETCHES THE SAME THING AGAIN — because if it
// does, there is a case for a pager (keep evicted payloads addressable and page them back);
// and if it does not, compaction is discarding exactly the right things and the entire
// pager family closes for free.
//
// R2-13 states the gate up front: build nothing further until the re-fetch rate clears 10%
// AND agent_run is a daily workload. **Sub-10% is a clean free kill**, and that is a perfectly
// good outcome — it also proves compaction is working.
//
// So this records, and deliberately does nothing else. No eviction store, no paging, no
// retrieval. Adding those before the measurement would be building the thing the measurement
// exists to justify.
//
// # Why hashes and not payloads
//
// Only a content hash and a size are kept. Storing evicted payloads would (a) be the pager
// itself, smuggled in as telemetry, and (b) put tool output — file contents, command output —
// into a durable side channel, which is exactly the redaction/brand-isolation liability that
// killed round 1's full trace store.
//
// # Pull-only, in-run
//
// The record lives for the run and is reported with it. Nothing is written into the
// transcript, so this cannot suffer the failure that killed Transcript Commons: a marker left
// in a transcript cannot be reliably attended by the model.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// PagerStats tracks evicted payloads and whether their content came back.
//
// Mutex-guarded for the same reason PrefillStats is: `--serve` shares one *Loop across
// concurrent HTTP handlers.
type PagerStats struct {
	mu sync.Mutex
	// evicted maps content-hash -> how many times that content was evicted.
	evicted map[string]int
	// refetched maps content-hash -> how many times it reappeared AFTER being evicted.
	refetched map[string]int
	evictions int
	bytes     int64
}

// minPayloadBytes ignores trivia. A 40-byte tool result reappearing is not evidence for a
// paging subsystem, and counting it would inflate the rate the gate reads.
const minPayloadBytes = 512

func payloadKey(s string) string {
	// Whitespace-normalised so a reflowed but identical payload is recognised as the same
	// content. Without this the re-fetch rate reads LOW for the wrong reason, which would
	// close the gate on an artefact of formatting rather than on agent behaviour.
	h := sha256.Sum256([]byte(strings.Join(strings.Fields(s), " ")))
	return hex.EncodeToString(h[:8])
}

// NoteEvicted records that a payload left the transcript.
func (p *PagerStats) NoteEvicted(content string) {
	if p == nil || len(content) < minPayloadBytes {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.evicted == nil {
		p.evicted = map[string]int{}
	}
	p.evicted[payloadKey(content)]++
	p.evictions++
	p.bytes += int64(len(content))
}

// NoteFetched records a tool result entering the transcript, and counts it as a RE-fetch only
// if identical content was evicted earlier in this run.
//
// Ordering is the whole measurement: content seen for the first time is not a re-fetch, and
// counting it as one would manufacture the very signal the gate is testing for.
func (p *PagerStats) NoteFetched(content string) {
	if p == nil || len(content) < minPayloadBytes {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.evicted == nil {
		return
	}
	k := payloadKey(content)
	if p.evicted[k] == 0 {
		return
	}
	if p.refetched == nil {
		p.refetched = map[string]int{}
	}
	p.refetched[k]++
}

// PagerReport is the read side, carrying its own gate verdict.
type PagerReport struct {
	Evictions       int   `json:"evictions"`
	EvictedBytes    int64 `json:"evicted_bytes"`
	DistinctEvicted int   `json:"distinct_evicted"`
	Refetched       int   `json:"refetched_distinct"`
	// RefetchRate is distinct re-fetched over distinct evicted. A POINTER: with nothing
	// evicted there is no rate, and a 0.0 would read as a measured "compaction is perfect" —
	// the conclusion that closes the item. Branch on Basis first.
	RefetchRate *float64 `json:"refetch_rate"`
	Basis       string   `json:"basis"` // "measured" | "insufficient_data"
	Verdict     string   `json:"verdict"`
}

// pagerGate is R2-13's stated threshold: below this, the pager family closes.
const pagerGate = 0.10

func (p *PagerStats) Report() PagerReport {
	if p == nil {
		return PagerReport{Basis: "insufficient_data", Verdict: "no run recorded"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := PagerReport{
		Evictions: p.evictions, EvictedBytes: p.bytes,
		DistinctEvicted: len(p.evicted), Refetched: len(p.refetched),
		Basis: "insufficient_data",
	}
	if len(p.evicted) == 0 {
		// Nothing was evicted, so the question was never posed. This is NOT "0% re-fetch".
		r.Verdict = "nothing evicted this run — the re-fetch question was not exercised"
		return r
	}
	r.Basis = "measured"
	v := float64(len(p.refetched)) / float64(len(p.evicted))
	r.RefetchRate = &v
	if v < pagerGate {
		r.Verdict = "BELOW GATE — compaction is discarding content the agent does not come back for; no pager needed on this evidence"
	} else {
		r.Verdict = "ABOVE GATE — evicted content is being re-fetched; a pager may be worth designing IF agent_run is a daily workload"
	}
	return r
}
