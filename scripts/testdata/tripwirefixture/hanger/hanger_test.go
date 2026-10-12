package hanger

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestHangs makes nothing and runs for a bounded time, so that a tripwire stopped while it is running has a test binary,
// started from the folder it watches, to stop. It also ignores the polite signals (the interrupt and the terminate request), as
// a test that traps them would: only the last step of the tripwire's stop, a kill, ends it. Left alone it ends by itself.
func TestHangs(t *testing.T) {
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
	time.Sleep(40 * time.Second)
}
