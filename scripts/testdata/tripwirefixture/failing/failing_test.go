package failing

import "testing"

// TestFails fails on purpose and makes nothing: the tripwire must say the run failed, not that it was clean.
func TestFails(t *testing.T) {
	t.Fatal("this test fails on purpose")
}
