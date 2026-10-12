// Copyright 2026 dmmdea and contributors. Licensed under Apache-2.0. See LICENSE.

//go:build !windows

package cobratree

import "os/exec"

// hideWindow is a no-op off Windows: there is no console window to hide.
func hideWindow(*exec.Cmd) {}
