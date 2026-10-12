package mediaremote

import "testing"

// A standing said before the waiter's Notify is set is told when the waiter sets it: the first scan of a ticket can run before the
// CLI's wait loop has set Notify, and its line went nowhere; if that scan still counted the line as said, the waiter's own first
// look (the same standing) was dropped as a repeat and the human was never told the ticket was queued (the CI run of 0.182.0:
// TestCLIBlocksAndCancelsUndispatched, "timed out waiting: the human is told it is queued", on a slow runner).
func TestAStandingSaidBeforeTheNotifyIsSetIsToldOnceItIs(t *testing.T) {
	lt := &liveTicket{}
	lt.say("queued/1", "ticket mq-x: queued (position 1)")

	var got []string
	lt.mu.Lock()
	lt.notify = func(s string) { got = append(got, s) }
	lt.mu.Unlock()

	lt.say("queued/1", "ticket mq-x: queued (position 1)")
	if len(got) != 1 {
		t.Fatalf("the waiter's first look at a standing nobody was told is told once, got %d line(s): %v", len(got), got)
	}
	lt.say("queued/1", "ticket mq-x: queued (position 1)")
	if len(got) != 1 {
		t.Fatalf("a standing already told is not told again, got %d line(s): %v", len(got), got)
	}
	lt.say("node/node-a", "ticket mq-x: started on node-a")
	if len(got) != 2 || got[1] != "ticket mq-x: started on node-a" {
		t.Fatalf("a new standing is told, got %v", got)
	}
}
