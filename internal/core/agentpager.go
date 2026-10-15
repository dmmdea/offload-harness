package core

// AgentPager is the wire form of the loop's context-pager report (internal/agent PagerReport).
// The key names are the loop report's own, so one decoder reads both a standalone trace file
// and a delegation-log row. The re-fetch rate is not carried: it is refetched_distinct over
// distinct_evicted, computed by a reader that pools runs, never averaged per run.
type AgentPager struct {
	// Basis is "measured" (something was evicted, so the re-fetch question was posed) or
	// "insufficient_data" (nothing was evicted; the counts below are real zeros, and the gate
	// is not read from this run).
	Basis            string `json:"basis"`
	Evictions        int    `json:"evictions"`
	EvictedBytes     int64  `json:"evicted_bytes"`
	DistinctEvicted  int    `json:"distinct_evicted"`
	Refetched        int    `json:"refetched_distinct"`
	CompactionPasses int    `json:"compaction_passes"`
}
