package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The local (in-process) path has no ACK hop, so a browse contract on a node that
// has not opted in DEFERS by class `config` — with no seat call — instead of running
// read-only and handing back a green result that never touched the browser.
func TestBrowseDoorRefusedWhenTheNodeHasNotOptedIn(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			t.Error("the seat was called on a node that has not opted into the browse door")
			return doneChat("x")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	c := testContract()
	c.AllowBrowse, c.BrowseHosts = true, []string{"example.com"}
	res := writeDoorPipeline(t, srv.URL, false).Run(context.Background(), agentTestRequest(t, c))
	wire := decodeWire(t, res)
	if !wire.Deferred || wire.DeferClass != core.DeferClassConfig {
		t.Fatalf("want a config-class defer, got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !strings.Contains(wire.Reason, "agent_allow_browse") {
		t.Errorf("reason %q does not name the key an operator must set", wire.Reason)
	}
}

// An opted-in node whose Build still refuses the grant (here: no resolvable home, so no
// audit trail) must defer, not run the contract without the browser it asked for and
// hand back a green result for work that never touched the page (review finding
// conventions:F6).
func TestBrowseGrantRefusedByBuildDefersInsteadOfRunningWithout(t *testing.T) {
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			t.Error("the seat ran a browse contract whose browse grant Build refused")
			return doneChat("x")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	p := writeDoorPipeline(t, srv.URL, false)
	p.cfg.AgentAllowBrowse = true
	p.cfg.BrowsePython, p.cfg.BrowseScript, p.cfg.BrowseDecisionURL = "py", "runner.py", "http://127.0.0.1:18720/v1/systemone"
	c := testContract()
	c.AllowBrowse, c.BrowseHosts = true, []string{"example.com"}
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, c)))
	if !wire.Deferred || wire.DeferClass != core.DeferClassConfig || !strings.Contains(wire.Reason, "not granted") {
		t.Fatalf("want a config-class 'not granted' defer, got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
}
