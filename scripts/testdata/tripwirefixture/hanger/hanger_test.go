package hanger

import (
	"testing"
	"time"
)

// TestHangs makes nothing and runs for a bounded time, so that a tripwire stopped while it is running has a test binary,
// started from the folder it watches, to stop. Left alone it ends by itself.
func TestHangs(t *testing.T) {
	time.Sleep(40 * time.Second)
}
