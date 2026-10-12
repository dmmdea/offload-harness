//go:build windows

package sandbox

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// The cage starts console programs (go, python, node, git ...) for an agent, with every handle it
// reads piped and no console of its own to hand on. A console program started that way by a process
// with no console gets a NEW console window on the operator's desktop (a Windows Terminal window by
// default). The cage must therefore start its child with CREATE_NO_WINDOW and a hidden show state.
//
// The test runs the real cage from a helper process that has a hidden console (CREATE_NO_WINDOW), so
// that even a cage that does NOT hide its child cannot put a window on the desktop: the child then
// inherits the helper's hidden console. What gives the difference away is the child's console: with
// CREATE_NO_WINDOW it is alone on a console of its own, without it it shares the helper's.

var procGetConsoleProcessListCage = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

func consolePIDs() []uint32 {
	buf := make([]uint32, 128)
	n, _, _ := procGetConsoleProcessListCage.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || int(n) > len(buf) {
		return nil
	}
	return buf[:n]
}

// TestCageConsoleHelper is the helper the test below starts; run on its own it does nothing.
//
//	outer   run the cage, which starts this test binary again in the "inner" mode
//	inner   print the console this process sits on and the show state it was started with
func TestCageConsoleHelper(t *testing.T) {
	args := flag.Args()
	if len(args) == 0 {
		t.Skip("helper for TestWindowsCageChildOpensNoConsoleWindow")
	}
	switch args[0] {
	case "inner":
		var si windows.StartupInfo
		if err := windows.GetStartupInfo(&si); err != nil {
			t.Fatalf("GetStartupInfo: %v", err)
		}
		pids := make([]string, 0, 4)
		for _, p := range consolePIDs() {
			pids = append(pids, strconv.FormatUint(uint64(p), 10))
		}
		fmt.Printf("PROBE pids=%s flags=%#x show=%d self=%d\n", strings.Join(pids, ","), si.Flags, si.ShowWindow, os.Getpid())
	case "outer":
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		wt := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := Run(ctx, Spec{
			Argv:               []string{self, "-test.run=^TestCageConsoleHelper$", "-test.count=1", "inner"},
			Worktree:           wt,
			WorktreeWritable:   true,
			Scratch:            filepath.Join(wt, ".scratch"),
			AllowedExecutables: []string{filepath.Base(self)},
		})
		fmt.Printf("OUTER %d err=%v exit=%d refused=%v\n%s\nSTDERR %s\n", os.Getpid(), err, res.ExitCode, res.Refused, res.Stdout, res.Stderr)
	default:
		t.Fatalf("unknown helper mode %q", args[0])
	}
}

var cageProbeLine = regexp.MustCompile(`PROBE pids=([0-9,]*) flags=(0x[0-9a-f]+) show=(\d+) self=(\d+)`)

func TestWindowsCageChildOpensNoConsoleWindow(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// the helper owns a hidden console of its own, whatever console this test process has
	helper := winexec.HideWindowDetached(exec.Command(self, "-test.run=^TestCageConsoleHelper$", "-test.count=1", "outer"))
	b, err := helper.Output()
	if err != nil {
		t.Fatalf("the helper failed: %v\n%s", err, b)
	}
	out := string(b)
	m := cageProbeLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the cage's child printed no probe line (the cage may have refused it):\n%s", out)
	}
	helperPID := strconv.Itoa(helper.Process.Pid)
	pids := strings.Split(m[1], ",")
	for _, p := range pids {
		if p == helperPID {
			t.Errorf("the cage's child shares the console of the process that started it (console processes %v, cage host %s): started without CREATE_NO_WINDOW it opens a console window whenever that process has no console", pids, helperPID)
		}
	}
	if len(pids) != 1 || pids[0] != m[4] {
		t.Errorf("the cage's child must be alone on a console of its own, got console processes %v (child %s)", pids, m[4])
	}
	flags, err := strconv.ParseUint(strings.TrimPrefix(m[2], "0x"), 16, 32)
	if err != nil {
		t.Fatal(err)
	}
	if flags&windows.STARTF_USESHOWWINDOW == 0 {
		t.Errorf("the cage's child was started without STARTF_USESHOWWINDOW (startup flags %#x): a program that opens a window of its own would show it", flags)
	}
	if m[3] != strconv.Itoa(windows.SW_HIDE) {
		t.Errorf("the cage's child was started with show state %s, want SW_HIDE (%d)", m[3], windows.SW_HIDE)
	}
	if flags&windows.STARTF_USESTDHANDLES == 0 {
		t.Errorf("the cage's child lost STARTF_USESTDHANDLES (startup flags %#x): its output would not reach the cage", flags)
	}
}
