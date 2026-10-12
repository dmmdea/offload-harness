package winexec

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
)

// createNoWindowFlag is CREATE_NO_WINDOW, spelled in the test so a wrong constant in the code under
// test cannot make the assertion agree with itself.
const createNoWindowFlag = 0x08000000

func TestHideWindow_ReturnsTheSameCommand(t *testing.T) {
	for name, hide := range map[string]func(*exec.Cmd) *exec.Cmd{
		"HideWindow":            HideWindow,
		"HideWindowDetached":    HideWindowDetached,
		"HideWindowIfNoConsole": HideWindowIfNoConsole,
	} {
		cmd := exec.Command("whatever")
		if got := hide(cmd); got != cmd {
			t.Fatalf("%s returned a different *exec.Cmd", name)
		}
	}
}

func TestCommandAndCommandContext_AreHiddenOnWindows(t *testing.T) {
	for name, cmd := range map[string]*exec.Cmd{
		"Command":        Command("whatever", "a", "b"),
		"CommandContext": CommandContext(context.Background(), "whatever", "a", "b"),
	} {
		if len(cmd.Args) != 3 || cmd.Args[1] != "a" || cmd.Args[2] != "b" {
			t.Errorf("%s: argv = %v, want [whatever a b]", name, cmd.Args)
		}
		if runtime.GOOS == "windows" {
			assertHidden(t, name, cmd)
		} else if hasSysProcAttr(cmd) {
			t.Errorf("%s: outside Windows the command must be left as exec built it", name)
		}
	}
}
