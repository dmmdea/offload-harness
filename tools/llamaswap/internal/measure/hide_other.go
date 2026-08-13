// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

//go:build !windows

package measure

import "os/exec"

// HideWindow is a no-op off Windows: there is no console window to hide.
func HideWindow(cmd *exec.Cmd) { hideWindow(cmd) }

func hideWindow(cmd *exec.Cmd) {}
