package main

// The progress contract of a wrapped render helper (F8, 2026-10-09).
//
// A lease is renewed at the end of its term only while something vouches for the job, and for an owner
// nobody can tell apart (a recipe script, a session the registry never held) the only voucher is a
// progress file that keeps moving (internal/gpulease/term.go, ADR 0070). `gpu reserve -- node
// render/comfy-*.mjs ...` declared none and the helpers never wrote one, so a Qwen-Image still that was
// rendering normally had its lease labelled "expired ... open to a takeover" at the end of its first term
// (a 1,394 s render), and a waiter reading `gpu status` was told a healthy job was
// abandoned.
//
// The wrapper form now declares the contract itself when it wraps a helper that heartbeats: a file under
// <state root>/progress/ and a stall window, recorded on the lease like a --progress-file the caller
// passed, and handed to the command in GPU_LEASE_PROGRESS_FILE (render/lease-progress.mjs appends to it
// from the poll loop of every ComfyUI runner, and between the pictures of a batch). A caller that passes
// its own --progress-file keeps it: the same variable carries that path instead. Nothing here releases a
// lease, stops a command or changes who may take the card; a hung helper stops writing and the lease
// reads held-stalled, as a lease with a progress contract that stops moving always did.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// gpuProgressFileEnv is the variable the wrapper sets on its command: the absolute path of the lease's
// progress file. render/lease-progress.mjs (PROGRESS_FILE_ENV) reads it; a test pins the two names equal.
const gpuProgressFileEnv = "GPU_LEASE_PROGRESS_FILE"

// autoProgressStall is the stall window of a contract the wrapper declares. CHOSEN, not measured: the
// helpers beat at least every 30 s while ComfyUI answers (BEAT_EVERY_MS in render/lease-progress.mjs),
// and the poll loop itself tolerates a silent ComfyUI for COMFY_DEAD_SEC (240 s) before it gives up, so
// a window of 15 minutes is more than three times the longest quiet stretch a healthy helper has once it
// polls, and a ComfyUI cold start (the one stretch before the first beat) has to outlast it before the
// lease reads stalled. A variable so a test can see a file go stale.
var autoProgressStall = 15 * time.Minute

// mintedProgressMaxAge is how long a minted progress file may sit unwritten before the next wrapper that
// mints one removes it as left by a wrapper that was killed.
const mintedProgressMaxAge = 7 * 24 * time.Hour

// autoProgressFile decides whether the wrapper declares a progress contract for the command it wraps,
// and where its file lives. It declares one only when the caller declared none (a caller's own
// --progress-file is its contract, and an --unattended lease must have carried one already) and the
// command runs a helper that heartbeats. Pure: the caller creates the directory and the file.
func autoProgressFile(opts gpulease.Options, cmdArgs []string, progressDir string, pid int, now time.Time) (path, helper string, ok bool) {
	if opts.ProgressFile != "" {
		return "", "", false
	}
	helper, found := gpulease.HeartbeatHelperIn(cmdArgs)
	if !found {
		return "", "", false
	}
	name := fmt.Sprintf("%s%d-%d%s", gpulease.MintedProgressPrefix, pid, now.UnixNano(), gpulease.MintedProgressSuffix)
	return filepath.Join(progressDir, name), helper, true
}

// autoProgressSentence is the line the wrapper prints once, where the contract is declared.
func autoProgressSentence(path, helper string, stall time.Duration) string {
	return fmt.Sprintf("gpu reserve: declaring a progress contract for %s: %s, stall window %s. The helper heartbeats it while ComfyUI answers and still lists its prompt, so the lease renews for the whole render and reads held-stalled only when the file stops moving; a --progress-file of your own replaces it",
		helper, filepath.Base(path), stall)
}

// startAutoProgress creates the minted file just before the command starts, with the line that says what
// it is for. Created here and not at acquisition: the drain that follows an acquisition can wait for a
// long seat turn, and a file that existed through it would read stalled while the wrapper waits.
func startAutoProgress(path, helper string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line := fmt.Sprintf("{\"at\":%d,\"pid\":%d,\"detail\":%q}\n", now.UnixMilli(), os.Getpid(), "gpu reserve started "+helper)
	return os.WriteFile(path, []byte(line), 0o644)
}
