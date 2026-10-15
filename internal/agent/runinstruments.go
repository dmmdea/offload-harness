package agent

// ResetRunInstruments starts the Loop's per-run instruments (the pager gate, the compaction pass
// count and the prefill accounting) over, so the next Run's report covers that run alone.
//
// Those instruments live on the Loop and accumulate for its whole life, which is right for a
// Loop built for one run (the agent door builds one per request) and wrong for a Loop that drains
// a queue: the second goal's report would be the sum of both, and a corpus pooling the per-goal
// traces would count the first goal twice. The standalone drainer calls this before each goal.
//
// Call it only between runs on a Loop used serially. A Loop shared by concurrent Runs (--serve)
// keeps one set of accumulators for all of them by design, and a reset there would wipe a run
// that is still in flight.
func (l *Loop) ResetRunInstruments() {
	l.pager.reset()
	l.prefill.reset()
	l.prefillSamples = nil
}
