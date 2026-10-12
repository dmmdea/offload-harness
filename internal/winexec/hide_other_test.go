//go:build !windows

package winexec

import (
	"os/exec"
	"testing"
)

func hasSysProcAttr(cmd *exec.Cmd) bool { return cmd.SysProcAttr != nil }

// assertHidden is never called outside Windows (the caller checks the OS first); it exists so the
// shared test compiles everywhere.
func assertHidden(t *testing.T, name string, cmd *exec.Cmd) {
	t.Helper()
	t.Fatalf("%s: assertHidden is a Windows check", name)
}

func TestHideWindow_IsANoopOutsideWindows(t *testing.T) {
	for name, cmd := range map[string]*exec.Cmd{
		"HideWindow":            HideWindow(exec.Command("whatever")),
		"HideWindowDetached":    HideWindowDetached(exec.Command("whatever")),
		"HideWindowIfNoConsole": HideWindowIfNoConsole(exec.Command("whatever")),
		"CommandDetached":       CommandDetached("whatever"),
		"CommandIfNoConsole":    CommandIfNoConsole("whatever"),
	} {
		if cmd.SysProcAttr != nil {
			t.Errorf("%s: SysProcAttr = %+v, want nil: there is no console window to hide here", name, cmd.SysProcAttr)
		}
	}
}
