package gpulease

import (
	"os"
	"testing"
	"time"
)

// A call that resumed a place and is WAITING (in-process, for the card slot) is as present as a
// call can be, but a token's life is its last poll and the wait polls nothing: after the 30 s grace
// the place read as absent and later waiters skipped it. TouchToken is the call saying "still
// here" without writing a place that no longer exists.
func TestTouchTokenKeepsAPlaceHeld(t *testing.T) {
	m, now := tokenManager(t)
	tok, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(20 * time.Second)
	if !m.TouchToken(tok.ID) {
		t.Fatal("a live token must accept a touch")
	}
	*now = now.Add(20 * time.Second) // 40 s since it was left, 20 s since it was touched
	got, ok := m.ResumeToken(tok.ID)
	if !ok || !m.TokenLive(got) {
		t.Fatalf("a touched place must still hold against later callers 40 s after it was left: %+v ok=%v", got, ok)
	}
	if !got.Since().Equal(tok.Since()) {
		t.Errorf("a touch must keep the arrival time: %v != %v", got.Since(), tok.Since())
	}
	*now = now.Add(20 * time.Second) // 20 s untouched ... and then past the grace
	*now = now.Add(15 * time.Second)
	if got, _ := m.ResumeToken(tok.ID); m.TokenLive(got) {
		t.Error("a place not touched for longer than the grace is absent again")
	}
}

// A touch never writes a place: a token that was spent, pruned or consumed by a waiter is gone, and
// the call that held it must not bring it back.
func TestTouchTokenNeverRecreatesAPlace(t *testing.T) {
	m, now := tokenManager(t)
	if m.TouchToken("tk-neverexisted1") {
		t.Error("a token that never existed cannot be touched")
	}
	if m.TouchToken("not a token id") {
		t.Error("a malformed id cannot be touched")
	}
	tok, _ := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	m.DropToken(tok.ID)
	if m.TouchToken(tok.ID) {
		t.Error("a dropped token cannot be touched")
	}
	if _, err := os.Stat(tokenPath(m.tokensDir(), tok.ID)); !os.IsNotExist(err) {
		t.Errorf("a touch brought a dropped token back: %v", err)
	}
	if len(m.Tokens()) != 0 {
		t.Errorf("tokens = %+v, want none", m.Tokens())
	}
}

// The place a touch refreshes is the one a waiter would otherwise have to skip.
func TestATouchedTokenStillBlocksAnEarlierWaiterOnlyWhileItIsHeld(t *testing.T) {
	m, now := tokenManager(t)
	tok, _ := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
	*now = now.Add(time.Second)
	later := Waiter{Devices: []string{"gpu-aaaa"}, SinceMs: now.UnixMilli()}
	if !m.tokenBlocks(later) {
		t.Fatal("a live token ahead of a later waiter blocks it")
	}
	*now = now.Add(29 * time.Second)
	m.TouchToken(tok.ID)
	*now = now.Add(29 * time.Second) // 59 s after it was left, 29 s after the touch
	if !m.tokenBlocks(later) {
		t.Error("a token touched 29 s ago still holds its place")
	}
	*now = now.Add(2 * time.Second)
	if m.tokenBlocks(later) {
		t.Error("a token untouched past the grace no longer blocks anyone")
	}
}

// The call that holds a place touches it from one goroutine while another path of the same call
// spends it (a grant, or a lease wait that consumes it): whichever order they interleave in, a
// spent place stays spent. A touch is a read-modify-write of the token file, so without a guard a
// touch that read the token before the drop and renamed after it would bring a spent place back for
// the grace, and every later caller would queue behind a caller who is gone.
func TestATouchRacingADropNeverRevivesTheToken(t *testing.T) {
	m, now := tokenManager(t)
	for i := 0; i < 150; i++ {
		tok, err := m.LeaveToken(ClassMedia, Options{Devices: []string{"gpu-aaaa"}}, *now)
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
					m.TouchToken(tok.ID)
				}
			}
		}()
		time.Sleep(50 * time.Microsecond)
		m.DropToken(tok.ID)
		close(stop)
		<-done
		if _, err := os.Stat(tokenPath(m.tokensDir(), tok.ID)); !os.IsNotExist(err) {
			t.Fatalf("round %d: a touch racing the drop revived the spent token (%v)", i, err)
		}
	}
}
