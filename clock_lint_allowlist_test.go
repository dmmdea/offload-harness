package main

// The list of clock_lint_test.go. Regenerate the keys with CLOCK_LINT_PRINT=1 (the command is in that file), then write each
// reason after reading the code: the reason is the review.

// clockBoundAllowlist: every upper bound on a measured elapsed time under one second in a test file, as "file:elapsed>C" with
// its count, and its reason: what the bound rules out and how wide the margin to it is. Listed at the 0.188.0 tip. An entry
// that calls its margin thin names a bound that a stalled runner can trip; it is listed, not endorsed, and the clock-free
// assertion that proves the same rule is named beside it where the test has one. The fix for a thin entry is the one the
// starvation fixes made: wait on the event the product signals, then delete the entry.
var clockBoundAllowlist = map[string]clockAllow{
	"gpu_warm_watch_test.go:elapsed>500ms": {3, "three 'must fail at once' assertions (a 5xx with nothing loading, a bare 500 over a cold seat, a start that died): each rules out waiting out the recovery watch, which fastWarmRetry sets to 2 s (4x the bound; its grace is 5 s). Thin on a stalled runner; the request count of 1 asserted beside each is the clock-free proof of the same rule"},

	"internal/agent/liveness_busyhold_test.go:elapsed>600ms": {1, "an engine unreadable at the first look must stall by the old rule, at the decoding allowance (200 ms in msPolicy), and not be held up to the 10 s the monitor is created with: 3x the allowance. Thin; the StallError fields asserted next carry the rule without a clock"},
	"internal/agent/liveness_load_test.go:elapsed>100ms":     {1, "SettleLoad with no read in flight must return at once instead of waiting its 2 s maximum: 20x"},
	"internal/agent/liveness_load_test.go:elapsed>200ms":     {1, "five SampleLoad calls must return at once while the probe holds an engine read open until the test releases it; a caller blocked on that read would wait for the read's own bound (the monitor is created with 5 s)"},
	"internal/agent/liveness_test.go:elapsed>100ms":          {1, "the snapshot's last-progress tick is under 100 ms old, read right after a loop that ticks every 5 ms. The test already assumes no gap reaches the 200 ms allowance (the run would be cancelled first), so this is half of that allowance: the thinnest margin in the test"},
	"internal/agent/seatdown_episode_test.go:elapsed>300ms":  {1, "the second wait must count from the FIRST verdict (350 ms earlier), so at most 150 ms of the 500 ms bound is left; a wait that restarted would take 500 ms. Thin: 200 ms under the restart it rules out, and the Sleep(350ms) that sets it is itself a clock. sd.Waited >= 450 ms, asserted after it, pins the same rule from the counted side"},
	"internal/agent/seatdown_episode_test.go:elapsed>500ms":  {1, "the second verdict on the same step is the thrash verdict at once; a fresh wait would take the 5 s cold-load ceiling of recoveryPolicy: 10x"},
	"internal/agent/seatdown_test.go:elapsed>500ms":          {2, "two 500 ms bounds. The recovering case must come back before the 600 ms bound it would otherwise run to: thin, 100 ms; the err == nil beside it already proves the wait did not give up. The no-probe case must not wait on its 1 s ceiling: 2x"},
	"internal/agent/seatdown_test.go:elapsed>800ms":          {1, "Chat on a seat that is starting returns the typed failure at once instead of waiting on the 30 s seat-wait budget; budget.Spent() == 0, asserted next, is the clock-free proof: 37x"},

	"internal/cache/cache_test.go:elapsed>700ms": {1, "an absolute ceiling on the sibling fallback, on purpose and not a multiple of LockTimeout, so that a revert of the lock timeout to the old 1 s fails here. Thin: 300 ms under the revert it rules out, and it times a file open plus a Put on a disk the runner shares"},

	"internal/delegate/callwait_yield_test.go:elapsed>750ms": {1, "not a ceiling: the lower edge of a sampling window (el > heldTTL+150ms && el < 1300ms) in a loop that classifies samples by when they were taken; samples between windows are ignored by design, so a slow runner only moves samples out of a window, and the test fails only when a whole 550 ms window has no sample"},

	"internal/fleetnode/residency_stale_test.go:elapsed>75ms": {1, "a health read after a finished contract must not pay the 150 ms wait bound (shortWaitBound) of a /running that answers in 300 ms: 2x the bound. Thin"},

	"internal/gpulease/explain_test.go:elapsed>750ms": {1, "formatting a refusal must not wait on an epoch lock another process holds, whose wait is about 2 s: 2.7x. The orphan-marker assertions in the same test are clock-free"},
	"internal/gpulease/owner_test.go:elapsed>750ms":   {1, "Standing on a lease directory this process cannot write must not spin the epoch lock another process holds, whose wait is about 2 s: 2.7x. The OrphanMarkErr assertion after it is clock-free"},

	"internal/gpulock/shortcircuit_test.go:elapsed>100ms": {1, "a 6-hour text lease answers at once instead of polling out the 300 ms wait: a third of the wait it rules out. Thin, 200 ms"},

	"internal/llamaclient/lanes_probe_test.go:elapsed>300ms": {1, "the live lane base must not wait behind the dead base's probe, whose bound is 800 ms (3/8 of it); the live base answers from an httptest server"},
	"internal/llamaclient/lanes_probe_test.go:elapsed>500ms": {1, "one probe bound is 300 ms and two in series 600 ms; the ceiling sits between them, 200 ms over one bound. Thin: the expected time is the bound itself plus one dial, so a timer that fires 200 ms late reads as a second probe. hole.Dials() == 1, checked first, is the clock-free proof of the same rule"},
	"internal/llamaclient/lanes_probe_test.go:elapsed>600ms": {1, "six concurrent callers must share one probe: twice the 300 ms bound, over an expected time of one bound. Thin, 300 ms; hole.Dials() == 1 is the clock-free proof of the same rule"},

	"internal/modelaffinity/seatslot_fifo_test.go:elapsed>500ms": {1, "the first waiter takes a free slot at once instead of waiting out its 2 s budget: 4x"},
	"internal/modelaffinity/seatslot_test.go:elapsed>200ms":      {1, "a run alone on the seat must not wait, where a waiter would use its 1 s budget: 5x"},

	"internal/pipeline/pipeline_job_test.go:elapsed>500ms":             {1, "the empty-artifacts guard must fire before contending for the already-held media slot, which would burn the 5000 ms GPU wait the test sets on purpose so that 'returned fast' means something: 10x"},
	"internal/pipeline/pipeline_vision_coldload_test.go:elapsed>800ms": {1, "a resident seat costs one /running read and no wait; a warm would cost the 1 s load the fake swap simulates. Thin, 200 ms; passthroughGets == 0, asserted before it, is the clock-free proof of the same rule"},
}
