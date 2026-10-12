//go:build windows

package winexec

import (
	"os/exec"
	"syscall"
	"testing"
)

func hasSysProcAttr(cmd *exec.Cmd) bool { return cmd.SysProcAttr != nil }

// asParentWithConsole poses as a parent that has (or has not) a console for the length of a test.
func asParentWithConsole(t *testing.T, has bool) {
	t.Helper()
	old := processHasConsole
	processHasConsole = func() bool { return has }
	t.Cleanup(func() { processHasConsole = old })
}

// assertHidden is the rule HideWindow keeps: the child's own window is hidden (SW_HIDE) always, and
// the child gets a console of its own (CREATE_NO_WINDOW) exactly when the parent has none to hand on.
// A parent with a console hands it on and no window appears; a child moved off that console would no
// longer get the operator's Ctrl-C.
func assertHidden(t *testing.T, name string, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil {
		t.Fatalf("%s: SysProcAttr is nil: the child would open a console window when its parent has no console", name)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Errorf("%s: HideWindow is not set", name)
	}
	hasFlag := cmd.SysProcAttr.CreationFlags&createNoWindowFlag != 0
	if want := !processHasConsole(); hasFlag != want {
		t.Errorf("%s: CREATE_NO_WINDOW set = %v, want %v (parent has a console: %v; creation flags %#x)", name, hasFlag, want, processHasConsole(), cmd.SysProcAttr.CreationFlags)
	}
}

func TestHideWindow_NoConsoleParent_GivesTheChildNoConsoleWindow(t *testing.T) {
	asParentWithConsole(t, false)
	cmd := HideWindow(exec.Command("whatever"))
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&createNoWindowFlag == 0 {
		t.Fatalf("a parent with no console must start the child hidden with CREATE_NO_WINDOW: %+v", cmd.SysProcAttr)
	}
}

func TestHideWindow_ConsoleParent_KeepsTheChildOnTheConsole(t *testing.T) {
	asParentWithConsole(t, true)
	cmd := HideWindow(exec.Command("whatever"))
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatalf("the child's own window must still be hidden: %+v", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindowFlag != 0 {
		t.Fatalf("CREATE_NO_WINDOW moves the child off the parent's console, so the operator's Ctrl-C no longer reaches it: %#x", cmd.SysProcAttr.CreationFlags)
	}
}

func TestHideWindowDetached_AlwaysOwnsNoConsole(t *testing.T) {
	for _, has := range []bool{false, true} {
		asParentWithConsole(t, has)
		cmd := HideWindowDetached(exec.Command("whatever"))
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&createNoWindowFlag == 0 {
			t.Errorf("parent has a console = %v: a child that outlives its parent must get its own hidden console: %+v", has, cmd.SysProcAttr)
		}
	}
}

func TestHideWindowIfNoConsole_LeavesTheCommandAloneUnderAConsole(t *testing.T) {
	asParentWithConsole(t, true)
	cmd := HideWindowIfNoConsole(exec.Command("whatever"))
	if cmd.SysProcAttr != nil {
		t.Fatalf("with a console to inherit the operator's own command must be left as exec built it (it may be a window the operator wants): %+v", cmd.SysProcAttr)
	}
	asParentWithConsole(t, false)
	cmd = HideWindowIfNoConsole(exec.Command("whatever"))
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&createNoWindowFlag == 0 {
		t.Fatalf("with no console the command must be hidden: %+v", cmd.SysProcAttr)
	}
}

func TestCommandDetachedAndCommandIfNoConsole_AreTheHelpersApplied(t *testing.T) {
	asParentWithConsole(t, true)
	d := CommandDetached("whatever", "a")
	if len(d.Args) != 2 || d.Args[1] != "a" || d.SysProcAttr == nil || d.SysProcAttr.CreationFlags&createNoWindowFlag == 0 {
		t.Errorf("CommandDetached: args %v attr %+v", d.Args, d.SysProcAttr)
	}
	n := CommandIfNoConsole("whatever", "a")
	if len(n.Args) != 2 || n.Args[1] != "a" || n.SysProcAttr != nil {
		t.Errorf("CommandIfNoConsole under a console: args %v attr %+v", n.Args, n.SysProcAttr)
	}
}

func TestHideWindow_KeepsWhatTheCallerSet(t *testing.T) {
	const newProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	for _, has := range []bool{false, true} {
		asParentWithConsole(t, has)
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
}

func TestHideWindow_Twice_IsTheSameAsOnce(t *testing.T) {
	asParentWithConsole(t, false)
	cmd := HideWindow(HideWindow(exec.Command("whatever")))
	assertHidden(t, "HideWindow twice", cmd)
	if cmd.SysProcAttr.CreationFlags != createNoWindowFlag {
		t.Errorf("creation flags = %#x, want exactly CREATE_NO_WINDOW", cmd.SysProcAttr.CreationFlags)
	}
}
