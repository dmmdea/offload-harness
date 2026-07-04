// Package gpulock is the READ-ONLY Go view of the single-slot GPU lock owned by
// the Node render runners (render/gpu-lock.mjs). The 8GB GPU is shared between
// llama-swap (the VLM/text tiers) and ComfyUI/Chatterbox (the generation jobs);
// the runners serialize GPU-heavy work through one lock DIRECTORY (mkdir is
// atomic) holding a meta.json {pid, startedAt}.
//
// LO-1 evidence: while a generation job held that lock, llama-swap could not
// (re)load the vision model, so EVERY vision call 5xx'd and deferred to the
// expensive cloud model (295 of the 337 all-time defers landed inside one such
// hour). This package lets the pipeline SEE the lock before burning a doomed
// HTTP call: resolve the same path the runners use, report held/not-held plus
// holder age, and wait (bounded) for the slot to free.
//
// Invariants:
//   - Read-only: this package NEVER creates, reclaims, or removes the lock —
//     acquisition/stale-reclaim stay with the runners (gpu-lock.mjs owns them).
//   - Same staleness rule as gpu-lock.mjs isStale(): missing/unreadable meta =>
//     stale; a recorded holder pid that is dead => stale; older than the 1h TTL
//     (the no-pid fallback + pid-recycle backstop) => stale. Stale == not held.
package gpulock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// DefaultTTL mirrors DEFAULT_TTL_MS in render/gpu-lock.mjs (1h — a real video
// generation can take many minutes; the TTL is only the fallback when the
// holder pid is unknown, and the backstop against pid recycling).
const DefaultTTL = time.Hour

// DefaultLockName is the basename of the shared lock dir under the OS temp dir,
// matching defaultLockPath() in render/gpu-lock.mjs.
const DefaultLockName = "local-offload-gpu.lock"

// Path resolves the shared GPU lock directory exactly the way the Node render
// runners do (gpu-lock.mjs defaultLockPath): explicit override first (the
// gpu_lock_path config field — the pipeline also threads it to the runners as
// the GPU_LOCK env so both sides always contend on ONE path), then the GPU_LOCK
// env, then <os-tmpdir>/local-offload-gpu.lock. Note the default is TMPDIR-
// relative (per the .mjs), NOT exe-dir-relative like the render script paths.
func Path(override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("GPU_LOCK"); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), DefaultLockName)
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

// lockMeta matches the meta.json the runners write on acquisition.
type lockMeta struct {
	PID       int   `json:"pid"`
	StartedAt int64 `json:"startedAt"`
}

// pidAlive reports whether pid is a live process, mirroring the .mjs rule
// (process.kill(pid, 0); EPERM still means alive). On Windows a successful
// OpenProcess (os.FindProcess) means alive; a failure means no such process.
// All gen runners run as the same user, so an access-denied misread (foreign
// elevated pid => reported dead) is not a case the lock can reach in practice.
// A package var so tests can stub a deterministic "dead holder".
var pidAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false // Windows: OpenProcess failed => no such process
	}
	defer p.Release()
	if runtime.GOOS == "windows" {
		return true // FindProcess only opens live processes on Windows
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

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
	if m.PID > 0 && !pidAlive(m.PID) {
		return Info{} // holder is dead => stale (the .mjs would reclaim it)
	}
	age := now.Sub(mfi.ModTime())
	if age > ttl {
		return Info{} // TTL cap: no-pid fallback + pid-recycle backstop
	}
	if age < 0 {
		age = 0
	}
	return Info{Held: true, Age: age, PID: m.PID}
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
