//go:build windows

package main

import (
	"os/exec"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// hideWindow makes a spawned child window-less on Windows.
//
// This is not cosmetic. A visible console window gets closed by hand, and closing it
// kills the process behind it — a detached GPU holder that dies silently leaves the
// card unreserved while its operator believes it is protected, which is worse than
// never having reserved it. CREATE_NO_WINDOW plus HideWindow covers both the console
// allocation and the window itself. The holder is hidden on a console of its own whatever the
// launching terminal has (winexec.HideWindowDetached): sharing the operator's terminal would let
// closing it end the hold. The flags live in winexec, the one place every hidden child is built.
func hideWindow(cmd *exec.Cmd) {
	winexec.HideWindowDetached(cmd)
}
