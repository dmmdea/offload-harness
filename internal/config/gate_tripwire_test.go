package config

import "testing"

// Reopen tripwires for the memory-frontier Phase 2 gates.
//
// # What this is, and what it is deliberately NOT
//
// Several Phase 2 gates were CLOSED on arguments that hold only while a specific default is
// off. Those arguments were written down; the conditions that would invalidate them were not
// enforced anywhere. That is the failure this guards: an analysis quietly becoming false
// because a flag moved, with nobody noticing that a closed item should have reopened.
//
// This is NOT a policy test. It does not claim these defaults are correct, and flipping one
// is a legitimate decision. It claims only that flipping one INVALIDATES A RECORDED
// CONCLUSION, so the change must be accompanied by reopening that gate.
//
// So the correct response to a failure here is: make the config change, update this test, and
// reopen the named gate. It is a tripwire, not a lock.
//
// Guarding the DEFAULTS rather than a local config.json is deliberate: config.json is not
// tracked in the repo, so CI cannot read it, and a default is the stronger thing anyway --
// it governs every fresh install rather than one box.

func TestGateReopenTripwires(t *testing.T) {
	c := Default()

	// R2-8 C1 (prefix histogram) was recorded as insufficient_data rather than
	// measured-negative, on the argument that the only source of dynamic front-of-prompt
	// content is exemplar injection, which is off. With shots > 0 the prefix bucket space
	// stops being a closed set derived from in-repo templates and the histogram becomes
	// genuinely informative again.
	if c.ExemplarShots != 0 {
		t.Fatalf("ExemplarShots default is now %d (was 0).\n"+
			"REOPEN memory-frontier gate R2-8 C1 (Prefix Telescope / Prefix Librarian): with exemplar\n"+
			"injection on, prompt prefixes carry per-call content and the recurrence histogram is\n"+
			"informative again. Update this test alongside the disposition record.", c.ExemplarShots)
	}

	// The "router labels only" sub-gate was recorded as a closed loop with no entry point:
	// no router -> no skips -> no labels -> no router. Either of the next two flags is the
	// operator decision that breaks that loop and makes the sub-gate runnable.
	if c.ShadowEnabled {
		t.Fatal("ShadowEnabled default is now true (was false).\n" +
			"REOPEN the memory-frontier 'router labels only' sub-gate: shadow capture is the A4 feeder,\n" +
			"and its absence was the stated reason that sub-gate had no entry point.")
	}

	if c.KNNPreFilterEnabled {
		t.Fatal("KNNPreFilterEnabled default is now true (was false).\n" +
			"REOPEN TWO items: (1) the 'router labels only' sub-gate (this is the B1 feeder), and\n" +
			"(2) the T2-C Embed Memo value case -- the memo was shipped documented as near-inert\n" +
			"BECAUSE this defaults false. With the pre-filter on it does real work and its hit rate\n" +
			"becomes a number worth reading.")
	}
}
