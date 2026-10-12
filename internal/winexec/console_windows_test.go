//go:build windows

package winexec

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The tests here start real processes and read what Windows says about their console. The helper
// modes run inside this very test binary (TestConsoleHelper). Every helper is started either with its
// own hidden console (CREATE_NO_WINDOW) or with no console at all (DETACHED_PROCESS), so neither the
// helpers nor anything they start can put a window on the desktop: a child that inherits a hidden
// console has no window, and a child started hidden has none either.

const detachedProcess = 0x00000008 // DETACHED_PROCESS

var procGetConsoleProcessListTest = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// consolePIDs: the processes on this process's console, nil when it has none.
func consolePIDs() []uint32 {
	buf := make([]uint32, 128)
	n, _, _ := procGetConsoleProcessListTest.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || int(n) > len(buf) {
		return nil
	}
	return buf[:n]
}

func joinPIDs(p []uint32) string {
	s := make([]string, len(p))
	for i, v := range p {
		s[i] = strconv.FormatUint(uint64(v), 10)
	}
	return strings.Join(s, ",")
}

// TestConsoleHelper is the helper the tests below start; run on its own it does nothing.
//
//	has        print whether this process thinks it has a console, and the raw list
//	pids       print the processes on this process's console
//	shared     start a child with Command (a parent with a console must hand it on) and print its answer
//	detached   start a child with CommandDetached and print its answer
//	wait       say READY and sleep: the child the Ctrl-C modes start
//	ctrlc      start a "wait" child with Command, send Ctrl-C to this console, print whether the child ended
//	ctrlc-detached   the same with CommandDetached
func TestConsoleHelper(t *testing.T) {
	args := flag.Args()
	if len(args) == 0 {
		t.Skip("helper for the console tests")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(mk func(string, ...string) *exec.Cmd) {
		out, err := mk(self, "-test.run=^TestConsoleHelper$", "-test.count=1", "pids").Output()
		if err != nil {
			fmt.Printf("CHILDERR %v\n", err)
			return
		}
		fmt.Print(string(out))
	}
	switch args[0] {
	case "has":
		fmt.Printf("HAS %v LIST %s\n", processHasConsole(), joinPIDs(consolePIDs()))
	case "pids":
		fmt.Printf("PIDS %s SELF %d\n", joinPIDs(consolePIDs()), os.Getpid())
	case "shared":
		fmt.Printf("OUTER %d\n", os.Getpid())
		child(Command)
	case "detached":
		fmt.Printf("OUTER %d\n", os.Getpid())
		child(CommandDetached)
	case "wait":
		fmt.Println("READY")
		time.Sleep(30 * time.Second)
	case "ctrlc", "ctrlc-detached":
		mk := Command
		if args[0] == "ctrlc-detached" {
			mk = CommandDetached
		}
		signal.Notify(make(chan os.Signal, 1), os.Interrupt) // this helper sits on the console the event goes to, and must outlive it
		c := mk(self, "-test.run=^TestConsoleHelper$", "-test.count=1", "wait")
		stdout, err := c.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		ready := make(chan bool, 1)
		go func() { ready <- bufio.NewScanner(stdout).Scan() }()
		select {
		case <-ready:
		case <-time.After(20 * time.Second):
			t.Fatal("the child never said READY")
		}
		// CTRL_C_EVENT to process group 0: every process on this console that has not switched Ctrl-C off
		if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case <-done:
			fmt.Println("CHILD exited")
		case <-time.After(5 * time.Second):
			fmt.Println("CHILD alive")
			_ = c.Process.Kill()
			<-done
		}
	default:
		t.Fatalf("unknown helper mode %q", args[0])
	}
}

// runHelper starts the helper in the given mode with the given creation flags and returns its pid and output.
func runHelper(t *testing.T, creationFlags uint32, mode string) (pid int, out string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestConsoleHelper$", "-test.count=1", mode)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: creationFlags}
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper %q: %v\n%s", mode, err, b)
	}
	return cmd.Process.Pid, string(b)
}

func TestProcessHasConsole_ReadsTheRealConsoleState(t *testing.T) {
	// a CREATE_NO_WINDOW process owns a console that has no window: it has a console
	_, withHidden := runHelper(t, createNoWindowFlag, "has")
	if !strings.Contains(withHidden, "HAS true") {
		t.Errorf("a process with a hidden console must report a console: %q", withHidden)
	}
	// a DETACHED_PROCESS process has none
	_, without := runHelper(t, detachedProcess, "has")
	if !strings.Contains(without, "HAS false") {
		t.Errorf("a detached process must report no console: %q", without)
	}
}

var pidsLine = regexp.MustCompile(`PIDS ([0-9,]*) SELF (\d+)`)

func childConsole(t *testing.T, out string) (list []string, self string) {
	t.Helper()
	m := pidsLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no PIDS line in the helper's output: %q", out)
	}
	if m[1] != "" {
		list = strings.Split(m[1], ",")
	}
	return list, m[2]
}

func TestCommand_ChildSharesTheParentsConsole(t *testing.T) {
	pid, out := runHelper(t, createNoWindowFlag, "shared")
	list, self := childConsole(t, out)
	if !containsPID(list, strconv.Itoa(pid)) || !containsPID(list, self) {
		t.Errorf("a child started by Command from a parent that has a console must share it, so the operator's Ctrl-C reaches both: console processes %v, parent %d, child %s", list, pid, self)
	}
}

func TestCommandDetached_ChildLeavesTheParentsConsole(t *testing.T) {
	pid, out := runHelper(t, createNoWindowFlag, "detached")
	list, self := childConsole(t, out)
	if containsPID(list, strconv.Itoa(pid)) || len(list) != 1 || list[0] != self {
		t.Errorf("a detached child must own a console of its own (hidden): console processes %v, parent %d, child %s", list, pid, self)
	}
}

// The user-visible half of sharing the console: the operator's Ctrl-C reaches the child. The helper owns a
// hidden console, starts a child, and sends CTRL_C_EVENT to that console (the helper catches it itself); a
// child started with Command is on the console and ends, a CommandDetached one is not and keeps running.
func TestCtrlC_ReachesAChildOfCommand_AndNotACommandDetachedOne(t *testing.T) {
	_, shared := runHelper(t, createNoWindowFlag, "ctrlc")
	if !strings.Contains(shared, "CHILD exited") {
		t.Errorf("the operator's Ctrl-C must reach a child started by Command from a parent that has a console, or an interrupted run leaves it running with no window to show it: %q", shared)
	}
	_, detached := runHelper(t, createNoWindowFlag, "ctrlc-detached")
	if !strings.Contains(detached, "CHILD alive") {
		t.Errorf("a CommandDetached child sits on a console of its own and must not get its parent's Ctrl-C: %q", detached)
	}
}

func containsPID(list []string, pid string) bool {
	for _, p := range list {
		if p == pid {
			return true
		}
	}
	return false
}
