package agent

import (
	"context"
	"strings"
	"testing"
)

// The replay seam must be able to switch on the two optional rungs the
// compaction eval measures, and must stay on the default path when it does not.

func replayHasSearchFooter(msgs []Msg) bool {
	for _, m := range msgs {
		for _, ln := range strings.Split(m.Content, "\n") {
			if isRungFooter(ln) {
				return true
			}
		}
	}
	return false
}

func replayHasLogHeader(msgs []Msg) bool {
	for _, m := range msgs {
		if m.Role == "tool" && isLogCollapsed(m.Content) {
			return true
		}
	}
	return false
}

func TestReplayOptsSearchResultsReachesTheLadder(t *testing.T) {
	msgs := rungTranscript()
	budget := estimateTokens(msgs) * 70 / 100

	off := CompactReplay(msgs, budget, rungKeep, rungProtected, ReplayOpts{GCF: true, Skeleton: true})
	if replayHasSearchFooter(off) {
		t.Fatal("the search rung ran with ReplayOpts.SearchResults unset")
	}
	on := CompactReplay(msgs, budget, rungKeep, rungProtected, ReplayOpts{GCF: true, Skeleton: true, SearchResults: true})
	if !replayHasSearchFooter(on) {
		t.Fatal("ReplayOpts.SearchResults did not switch the search rung on")
	}
	want := rungCompact(msgs, budget, compactOpts{GCF: true, Skeleton: true, SearchResults: true})
	if !sameMsgContents(on, want) {
		t.Fatal("CompactReplay with SearchResults is not the ladder run with compactOpts.SearchResults")
	}
}

func TestReplayOptsLogCollapseReachesTheLadder(t *testing.T) {
	msgs := lcTranscript(lrServerLog(170), lrServerLog(170), "short")
	budget := estimateTokens(msgs) * 60 / 100

	off := CompactReplay(msgs, budget, 2, 3, ReplayOpts{GCF: true, Skeleton: true})
	if replayHasLogHeader(off) {
		t.Fatal("the log rung ran with ReplayOpts.LogCollapse unset")
	}
	on := CompactReplay(msgs, budget, 2, 3, ReplayOpts{GCF: true, Skeleton: true, LogCollapse: true})
	if !replayHasLogHeader(on) {
		t.Fatal("ReplayOpts.LogCollapse did not switch the log rung on")
	}
	in := make([]Msg, len(msgs))
	copy(in, msgs)
	want := compact(context.Background(), in, budget, 2, 3, compactOpts{GCF: true, Skeleton: true, LogCollapse: true})
	if !sameMsgContents(on, want) {
		t.Fatal("CompactReplay with LogCollapse is not the ladder run with compactOpts.LogCollapse")
	}
}

func sameMsgContents(a, b []Msg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Content != b[i].Content || a[i].Role != b[i].Role || a[i].ToolCallID != b[i].ToolCallID {
			return false
		}
	}
	return true
}
