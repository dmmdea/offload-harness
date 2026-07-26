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
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// DefaultTTL mirrors DEFAULT_TTL_MS in render/gpu-lock.mjs (1h — a real video
// generation can take many minutes; the TTL is only the fallback when the
// holder pid is unknown, and the backstop against pid recycling).
const DefaultTTL = time.Hour

// DefaultLockName is the basename of the shared lock dir under the OS temp dir,
// matching defaultLockPath() in render/gpu-lock.mjs.
const DefaultLockName = "local-offload-gpu.lock"

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
	if override != "" {
		return override
	}
	if v := os.Getenv("GPU_LOCK"); v != "" {
		return v
	}
	root, err := gpulease.ResolveStateRoot(stateDir)
	if err != nil {
		// Read-only inspection must never fail the caller. Fall back to the
		// package default so a bad state_dir degrades to "cannot see the lease"
		// rather than breaking the vision path outright.
		return filepath.Join(os.TempDir(), DefaultLockName)
	}
	return filepath.Join(root, "gpu", "lease")
}

// Info is one point-in-time inspection of the lock.
type Info struct {
	// Held is true when the lock dir exists AND its meta is not stale.
	Held bool
	// Age is how long the current holder has held the lock (time since the
	// meta.json mtime — the same clock gpu-lock.mjs uses). Zero when not held.
	Age time.Duration
	// PID is the recorded holder pid (0 when unknown / not held).
	PID int
}

// lockMeta matches the meta.json every lease participant writes.
//
// The holder pid moved from a flat "pid" to a nested "holder.pid" when Go and Node
// adopted one shared record. Reading only the old shape yielded PID == 0, which skips
// the dead-holder check entirely and decides held/free on mtime alone — a lease held
// by a live process could then read as free. Both shapes are accepted so a mixed
// deployment (this fleet is hand-deployed and has drifted before) degrades to the old
// behaviour rather than to a wrong answer.
type lockMeta struct {
	Holder struct {
		PID int `json:"pid"`
	} `json:"holder"`
	LegacyPID   int   `json:"pid"`
	StartedAt   int64 `json:"startedAt"`
	ExpiresAtMs int64 `json:"expires_at_ms"`
}

// pid returns the holder pid from whichever schema the record uses.
func (m lockMeta) pid() int {
	if m.Holder.PID > 0 {
		return m.Holder.PID
	}
	return m.LegacyPID
}

// pidAlive delegates to gpulease so the two views of liveness cannot disagree.
//
// The previous local copy assumed every participant runs as the same user and so
// treated an access-denied OpenProcess as "no such process". That assumption is
// exactly what the machine-wide lease exists to break: a holder may be an elevated or
// SYSTEM-registered process, and misreading it as dead makes this package report a
// live holder's lease as FREE — the vision gate would then fire straight into a busy
// GPU. A package var so tests can stub a deterministic "dead holder".
var pidAlive = gpulease.PIDAlive

// Inspect reports whether the lock at lockPath is currently held, using the
// same staleness rule as gpu-lock.mjs with the default 1h TTL. A stale lock
// (dead holder / no meta / over-TTL) reports NOT held — reclaiming it is the
// runners' job, never ours.
func Inspect(lockPath string) Info { return inspectAt(lockPath, DefaultTTL, time.Now()) }

// inspectAt is Inspect with an injectable TTL and clock (unit-testable).
func inspectAt(lockPath string, ttl time.Duration, now time.Time) Info {
	fi, err := os.Stat(lockPath)
	if err != nil || !fi.IsDir() {
		return Info{} // no lock dir => free
	}
	mp := filepath.Join(lockPath, "meta.json")
	mfi, err := os.Stat(mp)
	if err != nil {
		return Info{} // no meta => stale (mjs: isStale(null) === true)
	}
	b, rerr := os.ReadFile(mp)
	var m lockMeta
	if rerr != nil || json.Unmarshal(b, &m) != nil {
		return Info{} // unreadable/corrupt meta => stale, same as the .mjs
	}
	if p := m.pid(); p > 0 && !pidAlive(p) {
		return Info{} // holder is dead => stale (the .mjs would reclaim it)
	}
	age := now.Sub(mfi.ModTime())
	if age > ttl {
		return Info{} // TTL cap: no-pid fallback + pid-recycle backstop
	}
	if age < 0 {
		age = 0
	}
	return Info{Held: true, Age: age, PID: m.pid()}
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
