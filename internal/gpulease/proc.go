package gpulease

// pidAlive reports whether pid is a live process.
//
// A package var so tests can substitute a deterministic "dead holder" without spawning
// real processes; the real work is in the per-platform pidAliveImpl.
//
// CORRECTNESS NOTE — this is load-bearing and was wrong once. The reclaim rule has a
// fast path: a holder that is "provably gone" is reclaimed IMMEDIATELY, bypassing both
// the heartbeat and the declared window. So a false "dead" reading does not merely
// degrade the lease, it hands the GPU away from a live holder.
//
// The previous implementation used os.FindProcess, which on Windows calls OpenProcess
// with PROCESS_QUERY_INFORMATION. Against a holder in another security context — an
// elevated process, or a service running as SYSTEM — that fails with ACCESS_DENIED,
// which was read as "no such process". The cross-security-context case is the entire
// reason this package exists, so the one scenario it was built for was the one it got
// backwards. It also disagreed with the Node side, which maps EPERM to ALIVE.
func pidAlive(pid int) bool { return pidAliveFn(pid) }

// PIDAlive is the exported view, so other packages inspecting the same lease (the
// read-only vision gate in internal/gpulock) share ONE definition of liveness. Two
// copies of this rule is how a live holder came to read as free in one package and
// held in another.
func PIDAlive(pid int) bool { return pidAliveFn(pid) }

var pidAliveFn = func(pid int) bool { return pidAliveImpl(pid) }
