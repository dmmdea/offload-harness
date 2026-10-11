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
// The wrapper form now declares the contract itself when the command it wraps RUNS a helper that
// heartbeats: the helper is an argument (`node render/comfy-generate.mjs ...`), or sits in the command line
// of an interpreter (`cmd /c "node ...comfy-generate.mjs"`), or in a script the command runs
// (`cmd /c render-both.cmd`, `bash render-card-v3.sh`: the form the long renders of this workspace take,
// gpulease.HeartbeatHelperBehind). The contract is a file under <state root>/progress/ and a stall window,
// recorded on the lease like a --progress-file the caller passed, and handed to the command in
// GPU_LEASE_PROGRESS_FILE (render/lease-progress.mjs appends to it from the poll loop of every ComfyUI
// runner, and between the pictures of a batch). A caller that passes its own --progress-file keeps it: the
// same variable carries that path instead. `--progress-file auto` asks for the minted file for a command
// the scan cannot read (a compiled driver, a script that builds the helper's name at run time). Nothing
// here releases a lease, stops a command or changes who may take the card; a hung helper stops writing and
// the lease reads held-stalled, as a lease with a progress contract that stops moving always did. A
// detached hold wraps no command, so it declares nothing by itself: name its file with --progress-file.

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
// --progress-file is its contract, and an --unattended lease must have carried one already) and either the
// command runs a helper that heartbeats (it reads the command line and the scripts the command runs,
// bounded: gpulease.HeartbeatHelperBehind) or the caller asked for the minted file (--progress-file
// auto). what names the reason, for the line the wrapper prints and the first line of the file. The
// directory and the file are the caller's to create; the scan only reads.
func autoProgressFile(opts gpulease.Options, cmdArgs []string, progressDir string, pid int, now time.Time, asked bool) (path, what string, ok bool) {
	if opts.ProgressFile != "" {
		return "", "", false
	}
	helper, where, found := gpulease.HeartbeatHelperBehind(cmdArgs)
	switch {
	case found:
		what = helper
		if where != "" {
			what += " (named in " + where + ")"
		}
	case asked:
		what = "the wrapped command"
	default:
		return "", "", false
	}
	name := fmt.Sprintf("%s%d-%d%s", gpulease.MintedProgressPrefix, pid, now.UnixNano(), gpulease.MintedProgressSuffix)
	return filepath.Join(progressDir, name), what, true
}

// autoProgressSentence is the line the wrapper prints once, where the contract is declared.
func autoProgressSentence(path, what string, stall time.Duration, asked bool) string {
	if asked {
		return fmt.Sprintf("gpu reserve: --progress-file auto: minted %s for %s, stall window %s, handed to the command in %s. A render helper that heartbeats writes to it while ComfyUI answers and still lists its prompt; a command that writes nothing to it reads held-stalled once the window has passed",
			filepath.Base(path), what, stall, gpuProgressFileEnv)
	}
	return fmt.Sprintf("gpu reserve: declaring a progress contract for %s: %s, stall window %s. The helper heartbeats it while ComfyUI answers and still lists its prompt, so the lease renews for the whole render and reads held-stalled only when the file stops moving; a --progress-file of your own replaces it",
		what, filepath.Base(path), stall)
}

// startAutoProgress creates the minted file just before the command starts, with the line that says what
// it is for. Created here and not at acquisition: the drain that follows an acquisition can wait for a
// long seat turn, and a file that existed through it would read stalled while the wrapper waits.
func startAutoProgress(path, what string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line := fmt.Sprintf("{\"at\":%d,\"pid\":%d,\"detail\":%q}\n", now.UnixMilli(), os.Getpid(), "gpu reserve started "+what)
	return os.WriteFile(path, []byte(line), 0o644)
}
