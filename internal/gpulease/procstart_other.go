//go:build !windows && !linux

package gpulease

// processStart has no portable implementation on this platform, so it reports
// "unknown" rather than guessing.
//
// This DEGRADES SAFELY and the degradation is explicit in Reclaimable: the
// pid-recycle check is skipped when the recorded start time is zero or the lookup
// fails, leaving pid-liveness plus the heartbeat/expiry conjunction. That is the
// same protection the previous lock had, never less — a platform without process
// start times loses the recycle guard, not the lease.
func processStart(pid int) (int64, bool) { return 0, false }
