package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A tracked call that panics must close its PAIR card failed, never
// "completed" (a panic leaves Run's named result zero), and the panic must
// still propagate.
func TestCloseCallFailsCardOnPanic(t *testing.T) {
	var ended int
	var got core.Result
	end := func(res core.Result) { ended++; got = res }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic must propagate past closeCall")
			}
		}()
		var res core.Result
		defer closeCall(end, &res)
		panic("boom")
	}()
	if ended != 1 || !got.Deferred || got.Reason != "panic: boom" {
		t.Fatalf("ended=%d deferred=%v reason=%q, want one failed close", ended, got.Deferred, got.Reason)
	}
}

// A normal return closes the card with the call's own outcome.
func TestCloseCallCarriesOutcome(t *testing.T) {
	var got core.Result
	end := func(res core.Result) { got = res }
	func() {
		res := core.Result{Deferred: true, Reason: "comfy unreachable", Meta: core.Meta{ErrClass: "timeout"}}
		defer closeCall(end, &res)
	}()
	if !got.Deferred || !strings.Contains(got.Reason, "comfy unreachable") || got.Meta.ErrClass != "timeout" {
		t.Fatalf("deferred=%v reason=%q class=%q: the tracker needs the whole outcome, the class included", got.Deferred, got.Reason, got.Meta.ErrClass)
	}
}

type recTracker struct {
	task, door string
	ended      int
	deferred   bool
}

func (r *recTracker) Begin(task, door string) (string, func(), func(core.Result)) {
	r.task, r.door = task, door
	return "call-test-1", func() {}, func(res core.Result) { r.ended++; r.deferred = res.Deferred }
}

// Run hands the tracker the call's door (so a fleet-served call can be
// skipped) and closes the card with the call's outcome.
func TestRunPassesDoorAndClosesCard(t *testing.T) {
	rt := &recTracker{}
	p := &Pipeline{}
	p.SetCallTracker(rt)
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Door: "fleet", Input: "a cat"})
	if rt.task != "generate_image" || rt.door != "fleet" || rt.ended != 1 || rt.deferred != res.Deferred {
		t.Fatalf("tracker = %+v (result deferred=%v)", rt, res.Deferred)
	}
}
