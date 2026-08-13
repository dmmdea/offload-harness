// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Regression test for the double-encoded logData payload.

package mirror

import (
	"strings"
	"testing"
	"time"
)

// escNL is a LITERAL backslash followed by 'n' — two bytes, not a newline.
// This is exactly what survives in RawEvent.Text on this deployment: the
// /api/events logData frame is double-encoded, so after the envelope unmarshal
// the log buffer still carries "\n" as two characters.
const escNL = `\n`

// TestParseSwapEventsSplitsEscapedNewlines pins the bug that made swap
// economics permanently empty: parseSwapEvents split on REAL newlines, but a
// double-encoded logData payload has none. The whole replayed log buffer
// therefore collapsed into ONE line, matched at most one marker, and
// swap_events_recorded stuck at 1 forever (verified live) — so `swaps` had no
// timeline to report and every percentile was null.
//
// The assertion is per-marker-line extraction, not merely "more than one".
func TestParseSwapEventsSplitsEscapedNewlines(t *testing.T) {
	// Marker shapes taken verbatim from parseLogMarker — these are the lines
	// this deployment actually emits (verified live on v249).
	lines := []string{
		"[INFO] matrix: model=embeddinggemma starting (no models running)",
		"[INFO] <embeddinggemma> Health check passed on http://127.0.0.1:9201/health",
		"[INFO] matrix: model=gemma-4-e4b starting (swapping out embeddinggemma)",
		"[INFO] <gemma-4-e4b> Health check passed on http://127.0.0.1:9202/health",
		"[INFO] <embeddinggemma> Stopping",
		"[INFO] <embeddinggemma> exited with code 0",
	}
	// Interleave non-marker noise: a real buffer is mostly lines that match
	// nothing, and they must be skipped without disturbing the markers.
	payload := strings.Join([]string{
		lines[0],
		"[INFO] proxy: request accepted",
		lines[1],
		lines[2],
		"[DEBUG] slot 0 released",
		lines[3],
		lines[4],
		lines[5],
	}, escNL)

	// Guard the fixture itself: if this ever contains a real newline the test
	// would pass for the wrong reason (the pre-fix code split it correctly).
	if strings.Contains(payload, "\n") {
		t.Fatal("fixture must contain NO real newlines — it models the double-encoded payload")
	}

	ev := RawEvent{ReceivedAt: time.Now(), Type: "logData", Text: payload}
	got := parseSwapEvents(ev)

	want := []swapEvent{
		{Model: "embeddinggemma", Event: "loading"},
		{Model: "embeddinggemma", Event: "ready"},
		{Model: "gemma-4-e4b", Event: "loading"},
		{Model: "gemma-4-e4b", Event: "ready"},
		{Model: "embeddinggemma", Event: "unloading"},
		{Model: "embeddinggemma", Event: "unloaded"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseSwapEvents returned %d events, want %d (one per marker line).\n"+
			"A count of 1 is the phantom-event regression: the escaped payload was not split.\ngot: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Model != w.Model || got[i].Event != w.Event {
			t.Errorf("event[%d] = {%s %s}, want {%s %s}", i, got[i].Model, got[i].Event, w.Model, w.Event)
		}
		if got[i].Source != "log" {
			t.Errorf("event[%d].Source = %q, want \"log\"", i, got[i].Source)
		}
		// Each event must carry its OWN line, not the whole buffer. A phantom
		// event's Line is the entire payload, which is how the dedup hash
		// collapsed everything into a single row.
		if strings.Contains(got[i].Line, escNL) {
			t.Errorf("event[%d].Line still holds the unsplit buffer: %q", i, got[i].Line)
		}
		if got[i].LineSHA == "" {
			t.Errorf("event[%d] has no line hash; dedup would break", i)
		}
	}

	// Distinct lines must hash distinctly, or insertSwapEvent dedups real
	// transitions away.
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.LineSHA] {
			t.Fatalf("duplicate LineSHA %q — distinct markers must not collide", e.LineSHA)
		}
		seen[e.LineSHA] = true
	}
}

// TestParseSwapEventsRealNewlinesStillWork guards the normalization against a
// regression in the other direction: a payload that already uses real newlines
// (structured builds, or a future server that stops double-encoding) must keep
// parsing line-by-line.
func TestParseSwapEventsRealNewlinesStillWork(t *testing.T) {
	payload := strings.Join([]string{
		"[INFO] matrix: model=qwen3-coder-30b starting (no models running)",
		"[INFO] <qwen3-coder-30b> Health check passed on http://127.0.0.1:9203/health",
		"[INFO] <qwen3-coder-30b> Stopping",
	}, "\n")

	got := parseSwapEvents(RawEvent{ReceivedAt: time.Now(), Type: "logData", Text: payload})
	if len(got) != 3 {
		t.Fatalf("real-newline payload produced %d events, want 3: %+v", len(got), got)
	}
	for i, wantEvent := range []string{"loading", "ready", "unloading"} {
		if got[i].Event != wantEvent {
			t.Errorf("event[%d].Event = %q, want %q", i, got[i].Event, wantEvent)
		}
		if got[i].Model != "qwen3-coder-30b" {
			t.Errorf("event[%d].Model = %q", i, got[i].Model)
		}
	}
}
