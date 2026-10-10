package core

import (
	"strings"
	"testing"
)

// recordingAttribution is a RemoteAttribution that remembers how the call ended.
type recordingAttribution struct {
	NopAttribution
	finished []Result
}

func (r *recordingAttribution) Finish(res Result) { r.finished = append(r.finished, res) }

// A lane that panics after BeginRemote closes the call before the panic goes on, with the panic's text:
// the card says what happened instead of waiting for the orphan sweep to call it a process that exited.
func TestCloseOnPanicFinishesTheCallAndPanicsOn(t *testing.T) {
	h := &recordingAttribution{}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		func() {
			defer CloseOnPanic(h)
			panic("poll exploded")
		}()
	}()
	if recovered != "poll exploded" {
		t.Fatalf("the panic must go on with its own value, got %v", recovered)
	}
	if len(h.finished) != 1 {
		t.Fatalf("the call must be finished exactly once, got %d", len(h.finished))
	}
	res := h.finished[0]
	if res.OK || !res.Deferred || !strings.HasPrefix(res.Reason, "panic: poll exploded") || res.DeferClass != DeferClassInfrastructure {
		t.Fatalf("a panic finishes the call as a deferred infrastructure failure carrying the panic: %+v", res)
	}
}

// A normal return is not touched: the lane already finished or discarded the handle.
func TestCloseOnPanicIsSilentOnANormalReturn(t *testing.T) {
	h := &recordingAttribution{}
	func() {
		defer CloseOnPanic(h)
	}()
	if len(h.finished) != 0 {
		t.Fatalf("a call that returned normally was finished by the guard: %+v", h.finished)
	}
}
