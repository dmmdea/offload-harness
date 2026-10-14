package agent

import "testing"

// The pager and prefill instruments live on the Loop and accumulate for its life. The
// standalone queue drainer reuses one Loop for every goal, so a per-goal trace needs a way to
// start the instruments over between goals. ResetRunInstruments is that, for SERIAL reuse only.

func TestResetRunInstrumentsStartsTheNextRunAtZero(t *testing.T) {
	l := NewLoop(&fakeClient{}, nil, 1)
	l.prefill.Observe(&ServeStats{CacheN: 5, PromptN: 9})
	l.prefillSamples = append(l.prefillSamples, PrefillSample{Tokens: 9, MS: 1})
	body := pagerBody("reset", 2000)
	l.pager.NoteCompaction([]Msg{{Role: "tool", Content: body}}, nil)
	l.pager.NoteFetched(body)

	l.ResetRunInstruments()

	pf := l.prefill.Report()
	if pf.Basis != "insufficient_data" || pf.ObservedSteps != 0 || pf.PrefillTokens != 0 {
		t.Errorf("prefill after reset = %+v, want unmeasured", pf)
	}
	if len(l.prefillSamples) != 0 {
		t.Errorf("prefill samples survived the reset: %v", l.prefillSamples)
	}
	pg := l.pager.Report()
	if pg.Basis != "insufficient_data" || pg.Evictions != 0 || pg.Refetched != 0 {
		t.Errorf("pager after reset = %+v, want nothing evicted", pg)
	}
	if pg.CompactionPasses == nil || *pg.CompactionPasses != 0 {
		t.Errorf("compaction_passes after reset = %v, want a present 0", pg.CompactionPasses)
	}
}

// TestResetRunInstrumentsForgetsEvictedPayloads: a payload evicted in one goal must not count
// as re-fetched when a later goal reads the same bytes; that is two runs, not one agent coming
// back for what compaction took.
func TestResetRunInstrumentsForgetsEvictedPayloads(t *testing.T) {
	l := NewLoop(&fakeClient{}, nil, 1)
	body := pagerBody("carry", 2000)
	l.pager.NoteCompaction([]Msg{{Role: "tool", Content: body}}, nil)
	l.ResetRunInstruments()
	l.pager.NoteFetched(body)
	if pg := l.pager.Report(); pg.Refetched != 0 {
		t.Fatalf("a payload evicted before the reset counted as re-fetched after it: %+v", pg)
	}
}
