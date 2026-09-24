package mediaops

import (
	"os"
	"os/exec"
)

// ResolveBinary answers "is this executable binding usable?" the way a spawned
// child process actually resolves it, not the way a bare os.Stat does. Two shapes:
//
//   - An explicit path (contains a separator, or otherwise names a real file in
//     the current directory) is stat'd directly.
//   - A bare command name — the shipped config default ("ffmpeg", "node", ...) —
//     is looked up on PATH via exec.LookPath, which on Windows also tries each
//     PATHEXT extension (.exe, .cmd, ...) the way CreateProcess/cmd.exe would.
//
// Before this helper existed, every FFmpeg call site had its own copy of one
// half of this (mediacap.binaryPresent used LookPath; internal/mediaops.RunMedia
// and the JS render scripts used only os.Stat/existsSync) — so a box whose config
// left ffmpeg_path at its bare "ffmpeg" default failed EVERY caller that used the
// os.Stat-only half, even with ffmpeg correctly installed and on PATH (F-38,
// 2026-09-24: reproduced identically on the Lenovo and the Aorus — see
// render/audio-qa.mjs's resolveFfmpeg for the JS side of the same bug class).
//
// Returns ("", false) for an empty binding. On success, returns the path that
// will actually run — the input unchanged for a working explicit path, or the
// PATH-resolved absolute path for a bare name.
func ResolveBinary(bin string) (string, bool) {
	if bin == "" {
		return "", false
	}
	if fi, err := os.Stat(bin); err == nil && !fi.IsDir() {
		return bin, true
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p, true
	}
	return "", false
}
