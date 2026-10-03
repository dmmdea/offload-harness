package netguard

import (
	"sync"
	"testing"
)

// TestTheTailnetSuffixIsSafeForConcurrentLoads: every config load installs the zone, and
// two loads can run at once in one process (two `gpu reserve` calls driven in-process by
// a test, a server re-reading its config while a request handler loads it). Under -race
// an unsynchronized zone is a data race; the Linux race gate on 0.161.0 caught it on
// the card-lease tests, eight times in one run.
func TestTheTailnetSuffixIsSafeForConcurrentLoads(t *testing.T) {
	t.Cleanup(func() { _ = SetTailnetSuffix("") })
	const zone = "tailnnnnnn.ts.net"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := SetTailnetSuffix(zone); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = TailnetURL("http://seat." + zone + ":8080")
			_ = TailnetSuffix()
		}()
	}
	wg.Wait()
	if got := TailnetSuffix(); got != zone {
		t.Fatalf("zone after concurrent loads = %q, want %q", got, zone)
	}
	if err := TailnetURL("http://seat." + zone + ":8080"); err != nil {
		t.Fatalf("a host under the installed zone is refused: %v", err)
	}
}
