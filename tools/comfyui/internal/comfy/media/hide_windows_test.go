// Copyright 2026 dmmdea and contributors. Licensed under Apache-2.0. See LICENSE.

//go:build windows

package media

import (
	"context"
	"testing"
)

// createNoWindowFlag is CREATE_NO_WINDOW, spelled in the test so a wrong constant in the code under
// test cannot make the assertion agree with itself.
const createNoWindowFlag = 0x08000000

func TestSystemCommand_IsHidden(t *testing.T) {
	cmd := systemCommand(context.Background(), "ffprobe", "-v", "error")
	if len(cmd.Args) != 3 || cmd.Args[1] != "-v" || cmd.Args[2] != "error" {
		t.Fatalf("argv = %v, want [ffprobe -v error]", cmd.Args)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil: ffprobe would open a console window when this process has no console")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow is not set")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindowFlag == 0 {
		t.Errorf("CREATE_NO_WINDOW is not set (creation flags %#x)", cmd.SysProcAttr.CreationFlags)
	}
}
