package fleetnode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// takeUploadSlot is the slot wait the stt upload door and the media-job door share. Its context arm is the
// guard for a caller that has gone while it waited: the request is neither admitted nor answered, and no slot
// is taken or released on its behalf. It was unpinned: mutating the arm to `return true` survived the whole
// package, and would have let the deferred release free a slot the request never took, so the door admitted
// more bodies than its bound and the real owner's release then blocked forever (review of 0.172.0, C5S2).
//
// A real HTTP/1.1 socket cancels the request context while the handler is still waiting only when the
// server is watching the connection, so this pins the arm in process, with a request whose context is
// cancelled while every slot is taken.
func TestAnUploadWaiterWhoseRequestIsCancelledLeavesWithoutASlotOrAnAnswer(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	for name, slots := range map[string]chan struct{}{"media-job": s.mediaJobSlots, "stt upload": s.sttUploadSlots} {
		t.Run(name, func(t *testing.T) {
			for i := 0; i < cap(slots); i++ {
				slots <- struct{}{}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/fleet/x", strings.NewReader("")).WithContext(ctx)
			rec := httptest.NewRecorder()
			const wait = 30 * time.Second // the production slot wait: the cancel, not the timer, must end this
			type outcome struct {
				got     bool
				elapsed time.Duration
			}
			done := make(chan outcome, 1)
			go func() {
				start := time.Now()
				got := s.takeUploadSlot(rec, req, slots, wait, "uploads")
				done <- outcome{got, time.Since(start)}
			}()
			time.Sleep(100 * time.Millisecond) // the waiter is parked on the full slots
			cancel()
			select {
			case o := <-done:
				if o.got {
					t.Fatal("takeUploadSlot reported a slot for a request whose caller had gone: the deferred release would free a slot it never took")
				}
				if o.elapsed > 5*time.Second {
					t.Errorf("the cancelled waiter returned after %v: it waited out the timer instead of leaving on the cancel", o.elapsed)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("a waiter whose request was cancelled never returned")
			}
			if rec.Code == http.StatusServiceUnavailable || rec.Body.Len() != 0 || rec.Header().Get("Retry-After") != "" {
				t.Errorf("nobody is listening for an answer, yet one was written: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
			}
			if n := len(slots); n != cap(slots) {
				t.Fatalf("%d of %d slots held after the waiter left: it took or released one that was not its own", n, cap(slots))
			}
			// and a waiter that stays still gets the slot when one frees, so nothing was leaked or wedged
			for i := 0; i < cap(slots); i++ {
				<-slots
			}
			if !s.takeUploadSlot(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/fleet/x", nil), slots, time.Second, "uploads") {
				t.Fatal("a free slot was not granted after a cancelled waiter left")
			}
			<-slots
		})
	}
}
