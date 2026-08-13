// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//go:build !windows

package cli

import "os/exec"

// hideSpawnedWindow is a no-op off Windows: a child process started from a CLI
// on Linux or macOS never allocates a window to hide.
func hideSpawnedWindow(cmd *exec.Cmd) {}
