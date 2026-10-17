package agent

import "time"

// stallCallbackStaleLocked reports that a stall-timer callback that has just
// taken the monitor's lock was launched by an arm of the timer that has since
// been replaced. Call it with m.mu held, first thing in onStall.
//
// Timer.Reset cannot recall a callback that has already been launched. When the
// timer expires while Phase, ToolPhase or Progress holds the lock, the callback
// parks on it and takes it the moment the method lets go, after the method has
// moved the run on and re-armed the timer. Nothing in the callback says which arm
// launched it, so it judged the new phase against the new last-progress time and
// filed a stall with no silence in it ("no progress for 0s in tool (allowed 120s;
// 1 tok so far)"). In a prefill it went on to the seat probe and the busy hold,
// which entered the hold a few milliseconds into the phase.
//
// In the phases where the allowance is a bound counted from the last progress
// (admission, prefill, decoding, tool, re-pack) the timer is armed for
// last+allow, so a callback that finds less than the allowance gone since the
// last progress is the old arm's. Every re-look the monitor makes after a due
// fire (the probe's poll, the engine's poll) happens with the allowance already
// spent and `last` unmoved, so it is never taken for stale. Two phases count
// otherwise and are left to the code that owns them: the cold-load hold arms its
// timer for what is left of the hold's current part, and the busy hold for the
// next engine read.
func (m *Monitor) stallCallbackStaleLocked() bool {
	switch m.phase {
	case PhaseColdLoad, PhaseQueued:
		return false
	}
	// The arm that is current fires at last+allow by itself: Timer.Reset on an
	// AfterFunc timer either moves the pending call or schedules a new one, so
	// ignoring the old arm's callback leaves the run no less watched.
	return time.Since(m.last) < m.allow
}
