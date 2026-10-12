//go:build !windows

package winexec

import "os/exec"

// HideWindow is a no-op outside Windows: there is no console window to hide. It returns cmd.
func HideWindow(cmd *exec.Cmd) *exec.Cmd { return cmd }

// HideWindowDetached is a no-op outside Windows. It returns cmd.
func HideWindowDetached(cmd *exec.Cmd) *exec.Cmd { return cmd }

// HideWindowIfNoConsole is a no-op outside Windows. It returns cmd.
func HideWindowIfNoConsole(cmd *exec.Cmd) *exec.Cmd { return cmd }
