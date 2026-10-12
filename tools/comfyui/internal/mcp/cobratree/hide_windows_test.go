// Copyright 2026 dmmdea and contributors. Licensed under Apache-2.0. See LICENSE.

//go:build windows

package cobratree

import (
	"context"
	"testing"
)

// createNoWindowFlag is CREATE_NO_WINDOW, spelled in the test so a wrong constant in the code under test
// cannot make the assertion agree with itself.
const createNoWindowFlag = 0x08000000

// The companion CLI runs once per MCP tool call, in a server its host normally starts without a console.
// A console program started that way gets a new visible console window unless it is started hidden.
func TestNewCLICommandIsHiddenOnWindows(t *testing.T) {
	cmd := newCLICommand(context.Background(), "companion-cli", []string{"a", "b"})
	if len(cmd.Args) != 3 || cmd.Args[0] != "companion-cli" || cmd.Args[1] != "a" || cmd.Args[2] != "b" {
		t.Fatalf("argv = %v, want [companion-cli a b]", cmd.Args)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil: the companion CLI would open a console window when its parent has no console")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow is not set")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindowFlag == 0 {
		t.Errorf("CREATE_NO_WINDOW is not set (creation flags %#x)", cmd.SysProcAttr.CreationFlags)
	}
}
