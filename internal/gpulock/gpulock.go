// Package gpulock is the READ-ONLY Go view of the GPU lease that internal/gpulease
// owns and that every GPU consumer takes. The GPU is shared between llama-swap (the
// VLM/text tiers) and ComfyUI/Chatterbox (the generation jobs).
//
// LO-1 evidence: while a generation job held the lease, llama-swap could not (re)load
// the vision model, so EVERY vision call 5xx'd and DEFERRED — 295 of the 337 all-time
// defers landed inside one such hour. A defer is not a cloud API call: the harness
// never calls a cloud model (see main.go), it returns a structured defer and the work
// falls back to Opus, the calling session. The cost is that the harness stops doing
// the one thing it exists for, silently. This package lets the pipeline SEE the lease
// before burning a doomed HTTP call: resolve the same path every other consumer uses,
// report held/not-held plus holder age, and wait (bounded) for the slot to free.
//
// Invariants:
//   - Read-only: this package NEVER creates, reclaims, or removes the lease —
//     acquisition and stale-reclaim belong to internal/gpulease.
//   - It resolves the SAME path and parses the SAME record as every other
//     participant, and shares one definition of process liveness with them. Each of
//     those was independently wrong once, and each time the symptom was identical:
//     this package reported a live holder's lease as FREE.
package gpulock

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// DefaultTTL mirrors DEFAULT_TTL_MS in render/gpu-lock.mjs (1h — a real video
// generation can take many minutes; the TTL is only the fallback when the
// holder pid is unknown, and the backstop against pid recycling).
const DefaultTTL = time.Hour

// Path resolves the shared GPU lease directory exactly the way every other consumer
// does: explicit override first (the gpu_lock_path config field — the pipeline also
// threads it to the runners as the GPU_LOCK env so both sides always contend on ONE
// path), then the GPU_LOCK env, then <state-root>/gpu/lease.
//
// THE DEFAULT MOVED, AND THIS MUST MOVE WITH IT. The lease used to live under the OS
// temp dir. When it became machine-wide (%ProgramData%) this function was left behind,
// so the vision gate watched a path nothing writes and WaitFree answered "free" every
// time — silently reinstating the LO-1 regression it exists to prevent (a vision call
// fired into a busy GPU 5xx's and defers, so the harness stops offloading at exactly
// the moment offloading matters). Resolution is delegated rather than duplicated so
// the two can never drift apart again.
func Path(override, stateDir string) string {
	// ONE resolver for every consumer. Re-deriving the location here is what split the
	// lease in two: this package honoured the override while the acquirer ignored it,
	// so the gate watched a directory nothing wrote.
	dir, err := gpulease.LeaseDir(override, stateDir)
	if err != nil {
		// Read-only inspection must never fail the caller, but it must not silently
		// pretend either: an unresolvable location degrades to a path that provably
		// does not exist, so Inspect reports NOT held and the caller proceeds — the
		// same outcome as today, without inventing a plausible-looking tmpdir path
		// that could collide with a real lock.
		return filepath.Join(os.TempDir(), "local-offload-gpu-UNRESOLVED")
	}
	return dir
}

// Info is one point-in-time inspection of the lease.
type Info struct {
	// Held is true when a claim exists AND the shared staleness rule says it is live.
	Held bool
	// Age is how long the current holder has held the card, from the acquirer's own
	// stamp. NOT time-since-last-heartbeat, which reads as a few seconds for any
	// long-running holder and made the defer message report "(0s)" for a job that had
	// owned the card for an hour.
	Age time.Duration
	// PID is the recorded holder pid (0 when unknown / not held).
	PID int
	// Class is the holder's lease class (media/text), so a caller can say WHICH kind of
	// work holds the card instead of assuming it is a generation job.
	Class string
	// ExpiresAt is the holder's DECLARED window end (zero when it declared none).
	// WaitFree uses it to stop polling a reservation that provably cannot free
	// inside the caller's wait: before 0.113.27 every vision call burned its
	// full vision_gpu_wait_sec (90 s by default) against a multi-hour hold, so
	// a long reservation cost 90 s of dead wait PER CALL for its whole life.
	ExpiresAt time.Time
}

// Inspect reports whether the lock at lockPath is currently held, using the
// same staleness rule as gpu-lock.mjs with the default 1h TTL. A stale lock
// (dead holder / no meta / over-TTL) reports NOT held — reclaiming it is the
// runners' job, never ours.
func Inspect(lockPath string) Info { return inspectAt(lockPath, DefaultTTL, time.Now()) }

// inspectAt is Inspect with an injectable TTL and clock, retained for the existing
// unit tests. The TTL argument is now ADVISORY ONLY: staleness is decided by
// gpulease.Reclaimable, not here.
//
// THE THIRD RULE IS GONE. This package used to carry its own staleness logic —
// (pid dead) OR (meta.json mtime older than 1h) — which was a THIRD implementation
// alongside Go's and Node's, and it disagreed with both: gpulease reclaims on
// (provably gone) OR (recycled pid) OR (heartbeat stale AND declared window expired).
// A holder that heartbeats for over an hour is live to gpulease and stale here, so this
// gate would report a busy card as free. That is the same failure mode as the path split
// (C5) and the schema split, one layer down. Delegating means there is one rule.
func inspectAt(lockPath string, _ time.Duration, _ time.Time) Info {
	// Delegate WHOLESALE, not just the rule. Reconstructing the judgement here from the
	// record — even using the shared Reclaimable — was still a second reader, and it
	// broke the moment the heartbeat moved into a per-epoch file: a record-only view
	// sees a frozen RenewedAtMs and calls a live, renewing holder stale as soon as its
	// declared window lapses. One inspection path, one answer.
	i := gpulease.InspectDir(lockPath)
	return Info{Held: i.Held, Age: i.Age, PID: i.PID, Class: string(i.Class), ExpiresAt: i.ExpiresAt}
}

// WaitFree polls the lock every poll (min bound 1ms; the pipeline passes 2s)
// until it is free or wait has elapsed (or ctx is done), returning the FINAL
// inspection: Held=false means the slot freed (proceed); Held=true means the
// caller should defer, with Age available for the defer reason.
func WaitFree(ctx context.Context, lockPath string, wait, poll time.Duration) Info {
	if poll <= 0 {
		poll = time.Millisecond
	}
	deadline := time.Now().Add(wait)
	for {
		info := Inspect(lockPath)
		if !info.Held {
			return info
		}
		// NEVER WAIT FOR SOMETHING THAT CANNOT HAPPEN (0.113.27) — the same
		// rule, and the same reasoning, as gpulease.Acquire's short-circuit.
		// A TEXT reservation carries an operator's declared duration
		// (`gpu reserve --for 8h`), so if it outlasts our whole window there
		// is nothing to wait for: the vision lane used to burn its full
		// vision_gpu_wait_sec on every call for the entire hold.
		//
		// A MEDIA lease is deliberately excluded: its expiry is a timeout
		// CEILING, not a promise — a video job declares 25 minutes and
		// routinely finishes in three — so short-circuiting on it would defer
		// calls that were about to be served. A holder with no declared
		// window is polled exactly as before.
		if info.Class == string(gpulease.ClassText) && !info.ExpiresAt.IsZero() && info.ExpiresAt.After(deadline) {
			return info
		}
		remain := time.Until(deadline)
		if remain <= 0 || ctx.Err() != nil {
			return info
		}
		if poll < remain {
			remain = poll
		}
		select {
		case <-ctx.Done():
			return info
		case <-time.After(remain):
		}
	}
}
