//go:build windows

package winexec

import (
	"os/exec"
	"syscall"
	"testing"
)

func hasSysProcAttr(cmd *exec.Cmd) bool { return cmd.SysProcAttr != nil }

func assertHidden(t *testing.T, name string, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil {
		t.Fatalf("%s: SysProcAttr is nil: the child would open a console window when its parent has no console", name)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Errorf("%s: HideWindow is not set", name)
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindowFlag == 0 {
		t.Errorf("%s: CREATE_NO_WINDOW is not set (creation flags %#x)", name, cmd.SysProcAttr.CreationFlags)
	}
}

func TestHideWindow_KeepsWhatTheCallerSet(t *testing.T) {
	const newProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	cmd := exec.Command("whatever")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: newProcessGroup, CmdLine: "whatever /x"}
	HideWindow(cmd)
	assertHidden(t, "HideWindow", cmd)
	if cmd.SysProcAttr.CreationFlags&newProcessGroup == 0 {
		t.Errorf("the caller's creation flag was dropped: %#x", cmd.SysProcAttr.CreationFlags)
	}
	if cmd.SysProcAttr.CmdLine != "whatever /x" {
		t.Errorf("the caller's CmdLine was dropped: %q", cmd.SysProcAttr.CmdLine)
	}
}

func TestHideWindow_Twice_IsTheSameAsOnce(t *testing.T) {
	cmd := HideWindow(HideWindow(exec.Command("whatever")))
	assertHidden(t, "HideWindow twice", cmd)
	if cmd.SysProcAttr.CreationFlags != createNoWindowFlag {
		t.Errorf("creation flags = %#x, want exactly CREATE_NO_WINDOW", cmd.SysProcAttr.CreationFlags)
	}
}
