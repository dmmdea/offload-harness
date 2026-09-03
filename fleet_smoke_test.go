package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

func TestSmokeContractIsGroundedAndCheap(t *testing.T) {
	spec := smokeContract("lenovo-ampere6")
	c, err := delegate.PrepareContract(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxSteps != 1 || c.TimeoutSec != 60 {
		t.Fatalf("smoke must be one step / 60 s, got %d/%d", c.MaxSteps, c.TimeoutSec)
	}
	if !strings.Contains(strings.Join(c.Acceptance, " "), "contains:PONG-lenovo-ampere6") {
		t.Fatalf("acceptance must anchor on a token that only the doc carries: %v", c.Acceptance)
	}
	if len(c.Context) != 1 || !strings.Contains(c.Context[0].Text, "PONG-lenovo-ampere6") {
		t.Fatal("the token must be IN the context doc, not only in the goal (parrot-passable otherwise)")
	}
	if strings.Contains(c.Goal, "PONG-lenovo-ampere6") {
		t.Fatal("goal must not carry the token — an echo of the goal would pass")
	}
}

func TestRenderSmokeTable(t *testing.T) {
	out := renderSmokeTable([]smokeRow{{Base: "http://a:18811", Node: "a", Seat: "s", Placement: "remote", WallMs: 1234, Verdict: "PASS"}, {Base: "http://b:18811", Verdict: "FAIL", Detail: "probe: dial refused"}})
	if !strings.Contains(out, "PASS") || !strings.Contains(out, "dial refused") || !strings.Contains(out, "1.2s") {
		t.Fatalf("table:\n%s", out)
	}
}
