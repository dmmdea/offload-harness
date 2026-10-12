package hanger

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

const childEnv = "TRIPWIRE_FIXTURE_HANGER_CHILD"

// TestHangs makes nothing and runs for a bounded time, so that a tripwire stopped while it is running has a test binary,
// started from the folder it watches, to stop. It ignores the polite signals (the interrupt and the terminate request), as a
// test that traps them would, and it starts one copy of itself that does the same and is not waited for: go test ends its own
// test binary when it is interrupted, but not that binary's child, so only a stop that reaches the whole process group, and
// ends with a kill, leaves nothing running. Left alone every copy ends by itself.
func TestHangs(t *testing.T) {
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
	if os.Getenv(childEnv) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHangs$", "-test.timeout=90s") // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — a copy of the test binary itself, a fixed argument list
		cmd.Env = append(os.Environ(), childEnv+"=1")
		hideWindow(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Not waited for, on purpose: the copy outlives this test binary when go test kills it.
	}
	time.Sleep(40 * time.Second)
}
