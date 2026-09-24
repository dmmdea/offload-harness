package pipeline

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A tracked call that panics must close its PAIR card failed, never
// "completed" (a panic leaves Run's named result zero), and the panic must
// still propagate.
func TestCloseCallFailsCardOnPanic(t *testing.T) {
	var ended int
	var deferred bool
	var reason string
	end := func(d bool, r string) { ended++; deferred, reason = d, r }
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
	if ended != 1 || !deferred || reason != "panic: boom" {
		t.Fatalf("ended=%d deferred=%v reason=%q, want one failed close", ended, deferred, reason)
	}
}

// A normal return closes the card with the call's own outcome.
func TestCloseCallCarriesOutcome(t *testing.T) {
	var got string
	var gotDeferred bool
	end := func(d bool, r string) { gotDeferred, got = d, r }
	func() {
		res := core.Result{Deferred: true, Reason: "comfy unreachable"}
		defer closeCall(end, &res)
	}()
	if !gotDeferred || !strings.Contains(got, "comfy unreachable") {
		t.Fatalf("deferred=%v reason=%q", gotDeferred, got)
	}
}
