package verifier

import (
	"errors"
	"testing"
)

// Check is the gate that decides retry vs escalate vs defer, so each verdict
// field is pinned explicitly: a wrong Terminal burns the slow 26B tier on an
// input no local tier can fix, and a wrong Retry re-prompts a model that
// returned nothing.
func TestCheck(t *testing.T) {
	parseErr := errors.New("unexpected end of JSON input")

	tests := []struct {
		name      string
		content   string
		truncated bool
		parseErr  error
		want      Verdict
	}{
		{
			name:    "good output accepts",
			content: `{"decision":"yes"}`,
			want:    Verdict{OK: true},
		},
		{
			name:    "empty output is worth a re-prompt",
			content: "",
			want:    Verdict{Retry: true, Reason: "empty model output"},
		},
		{
			name:    "whitespace-only counts as empty",
			content: " \n\t ",
			want:    Verdict{Retry: true, Reason: "empty model output"},
		},
		{
			name:      "truncation is terminal, never a retry",
			content:   `{"summary":"half a resp`,
			truncated: true,
			want:      Verdict{Retry: false, Terminal: true, Reason: "output truncated (hit token limit)"},
		},
		{
			name:     "unparseable output is worth a re-prompt",
			content:  "Sure! Here is your JSON:",
			parseErr: parseErr,
			want:     Verdict{Retry: true, Reason: "unparseable output: " + parseErr.Error()},
		},
		{
			// Truncated output is unparseable by construction; truncation is the
			// root cause, so it must win — a retry cannot fix an oversized input.
			name:      "truncated beats a parse error",
			content:   `{"summary":"half a resp`,
			truncated: true,
			parseErr:  parseErr,
			want:      Verdict{Retry: false, Terminal: true, Reason: "output truncated (hit token limit)"},
		},
		{
			// Documents the current ordering: emptiness is checked first, so an
			// empty-and-truncated response asks for a retry rather than deferring.
			name:      "empty beats truncated",
			content:   "",
			truncated: true,
			want:      Verdict{Retry: true, Reason: "empty model output"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.content, tc.truncated, tc.parseErr)
			if got != tc.want {
				t.Errorf("Check(%q, %v, %v)\n got: %+v\nwant: %+v", tc.content, tc.truncated, tc.parseErr, got, tc.want)
			}
		})
	}
}

// An accepted verdict must never also ask for a retry or a defer — the pipeline
// branches on these independently.
func TestCheckOKIsExclusive(t *testing.T) {
	got := Check(`{"label":"bug"}`, false, nil)
	if !got.OK {
		t.Fatalf("expected OK verdict, got %+v", got)
	}
	if got.Retry || got.Terminal || got.Reason != "" {
		t.Errorf("OK verdict must carry no retry/terminal/reason, got %+v", got)
	}
}

// Every rejection must explain itself: Reason surfaces in the defer payload the
// caller reads to decide what to do instead.
func TestCheckFailuresAlwaysGiveAReason(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		truncated bool
		parseErr  error
	}{
		{"empty", "", false, nil},
		{"truncated", "abc", true, nil},
		{"parse error", "abc", false, errors.New("boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.content, tc.truncated, tc.parseErr)
			if got.OK {
				t.Fatalf("expected rejection, got %+v", got)
			}
			if got.Reason == "" {
				t.Errorf("rejection with no Reason: %+v", got)
			}
		})
	}
}
