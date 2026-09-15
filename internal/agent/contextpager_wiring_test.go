package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The R2-13 gate has to be REACHABLE, in both directions.
//
// The instrument (contextpager.go) landed complete — evictions, re-fetches, a
// pointer rate, its own verdict — and with NO production caller. Nothing ever
// called NoteEvicted or NoteFetched outside its own unit test, so every real
// run reported `insufficient_data` forever and the "build nothing until the
// re-fetch rate clears 10 %" gate could neither close the pager family nor open
// it. A measurement that cannot be taken is not a measurement.
//
// These tests run the REAL loop and assert the report the run publishes. Delete
// the NoteCompaction call in loop.go and the first one fails on Basis; delete
// the NoteFetched call and the second fails on Refetched.

// pagerBody builds a distinctive tool body of n bytes, tagged so two different
// payloads can never hash alike.
func pagerBody(tag string, n int) string {
	return tag + ":" + strings.Repeat(tag+"-line-of-evictable-content ", n/28)
}

// pagerRun drives four tool steps through a window too small to hold them, with
// the FIRST payload re-fetched at step 4 — after compaction has had to evict it.
// Returns the run's result.
func pagerRun(t *testing.T) Result {
	t.Helper()
	const (
		bigA   = 9000
		filler = 5000
	)
	a := pagerBody("alpha", bigA)
	bodies := map[string]string{
		"a": a,
		"b": pagerBody("bravo", filler),
		"c": pagerBody("charlie", filler),
		"d": a, // step 4 fetches the SAME content back
	}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read", `{"k":"a"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "read", `{"k":"b"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c3", "read", `{"k":"c"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c4", "read", `{"k":"d"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	tools := []Tool{{
		ToolSpec: ToolSpec{Name: "read", Description: "read a blob", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec: func(_ context.Context, args string) (string, error) {
			var in struct {
				K string `json:"k"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			return bodies[in.K], nil
		},
	}}
	res, err := NewLoop(client, tools, 10).
		WithContextTokens(4096).   // a window far too small for four big bodies
		WithToolResultCap(64_000). // the loop cap must not truncate the payloads first
		WithoutExemplars().
		Run(context.Background(), "read the blobs")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestRunPublishesAMeasuredPagerReport: a run whose transcript overflowed its
// window must publish a MEASURED pager report — the compaction path feeds the
// instrument. Without the caller this reads "insufficient_data", which is the
// exact state that silently disarmed the gate.
func TestRunPublishesAMeasuredPagerReport(t *testing.T) {
	res := pagerRun(t)
	if res.Pager.Basis != "measured" {
		t.Fatalf("pager basis = %q (evictions=%d, distinct=%d) — a run that compacted a 4k window "+
			"around four large tool bodies evicted something; an unfed instrument reports insufficient_data forever: %+v",
			res.Pager.Basis, res.Pager.Evictions, res.Pager.DistinctEvicted, res.Pager)
	}
	if res.Pager.Evictions < 1 || res.Pager.EvictedBytes <= 0 {
		t.Fatalf("pager recorded no evicted bytes: %+v", res.Pager)
	}
	if res.Pager.RefetchRate == nil {
		t.Fatalf("a measured report must carry a rate: %+v", res.Pager)
	}
	if !strings.Contains(res.Pager.Verdict, "GATE") {
		t.Fatalf("a measured report must carry a gate verdict, got %q", res.Pager.Verdict)
	}
}

// TestRunCountsAReFetchOfEvictedContent: step 4 fetches back the very payload
// step 1 fetched and compaction evicted, so the run must count ONE distinct
// re-fetch. Without the NoteFetched caller the count stays 0 and the gate reads
// a confident 0 % — the conclusion that closes the item on a question nothing
// asked.
func TestRunCountsAReFetchOfEvictedContent(t *testing.T) {
	res := pagerRun(t)
	if res.Pager.Refetched != 1 {
		t.Fatalf("refetched_distinct = %d, want 1 (the alpha payload was fetched, evicted, then fetched again): %+v",
			res.Pager.Refetched, res.Pager)
	}
	if res.Pager.RefetchRate == nil || *res.Pager.RefetchRate <= 0 {
		t.Fatalf("a counted re-fetch must move the rate off zero: %+v", res.Pager)
	}
}

// TestPagerReportRidesTheRunResultAsJSON: the number has to be able to LEAVE the
// process, or the gate is unreadable however well it is measured.
func TestPagerReportRidesTheRunResultAsJSON(t *testing.T) {
	b, err := json.Marshal(pagerRun(t).Pager)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"evictions"`, `"refetch_rate"`, `"basis"`, `"verdict"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("pager report JSON missing %s: %s", key, b)
		}
	}
}
