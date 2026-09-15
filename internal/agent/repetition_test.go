package agent

import (
	"strings"
	"testing"
)

// loopBlock is the exact degenerate tail the Lenovo 4B produced on the
// METHODOLOGY.md digest (2026-09-14): a four-line block under `numbers:`,
// repeated until the completion budget ran out.
const loopBlock = "- 100 words cap for summary (enforced).\n" +
	"- 5 quotes cap (enforced).\n" +
	"- 1 sentence cap for verdict (enforced).\n" +
	"- 1 sentence per mechanism (enforced).\n"

func repeat(s string, n int) string { return strings.Repeat(s, n) }

// distinctList is a LEGITIMATE long list — 30 items, none repeated. A detector
// that keys on length rather than on repetition would flag this, and flagging
// it would trim a correct answer.
func distinctList(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString("- item ")
		b.WriteString(string(rune('a' + (i-1)%26)))
		b.WriteString(" number ")
		b.WriteByte(byte('0' + i/10))
		b.WriteByte(byte('0' + i%10))
		b.WriteString(" is distinct.\n")
	}
	return b.String()
}

func TestDetectRepetitionLoop(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		want      bool
		wantCount int
		wantFirst string
	}{
		{
			name:      "the live 4B loop: a four-line block twenty times, tail cut mid-line",
			text:      "numbers:\n" + repeat(loopBlock, 20) + "- 1 sentence cap for ",
			want:      true,
			wantCount: 20,
			wantFirst: "- 100 words cap for summary (enforced).",
		},
		{
			name:      "one line repeated six times",
			text:      "mechanisms:\n" + repeat("- Prefill TPS = prompt_tokens / seconds.\n", 6),
			want:      true,
			wantCount: 6,
			wantFirst: "- Prefill TPS = prompt_tokens / seconds.",
		},
		{
			name:      "exactly four repeats is the threshold and must trigger",
			text:      "numbers:\n" + repeat(loopBlock, 4),
			want:      true,
			wantCount: 4,
			wantFirst: "- 100 words cap for summary (enforced).",
		},
		{
			name: "a legitimate list of 30 distinct items",
			text: "numbers:\n" + distinctList(30),
			want: false,
		},
		{
			name: "three repeats are not a loop",
			text: "numbers:\n" + repeat(loopBlock, 3),
			want: false,
		},
		{
			name: "three repeats of a single line are not a loop",
			text: "numbers:\n" + repeat("- 5 quotes cap (enforced).\n", 3),
			want: false,
		},
		{
			name: "ordinary prose",
			text: "The methodology section describes three phases. Each phase is timed server-side.\nThe warmup run is discarded.",
			want: false,
		},
		{
			name: "empty",
			text: "",
			want: false,
		},
		{
			name: "repeated blank lines are not a loop",
			text: "summary:\nshort.\n\n\n\n\n\n\nmechanisms:\n- one\n",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, ok := DetectRepetitionLoop(c.text)
			if ok != c.want {
				t.Fatalf("detected = %v, want %v (rep=%+v)", ok, c.want, rep)
			}
			if !c.want {
				return
			}
			if rep.Count != c.wantCount {
				t.Errorf("count = %d, want %d", rep.Count, c.wantCount)
			}
			if rep.First != c.wantFirst {
				t.Errorf("first line = %q, want %q", rep.First, c.wantFirst)
			}
		})
	}
}

// TestTrimRepetitionLoopKeepsOneCopy: the caller must see the answer AND what
// happened to it — one copy of the repeated block, then the marker, and nothing
// of the twenty that followed.
func TestTrimRepetitionLoopKeepsOneCopy(t *testing.T) {
	text := "summary:\nExo-Bench measures TPS.\nnumbers:\n" + repeat(loopBlock, 20) + "- 1 sentence cap for "
	out, note, ok := TrimRepetitionLoop(text)
	if !ok {
		t.Fatal("the live loop must be detected")
	}
	if !strings.HasPrefix(out, "summary:\nExo-Bench measures TPS.\nnumbers:\n") {
		t.Fatalf("the answer before the loop must survive byte for byte: %q", clip(out, 120))
	}
	if n := strings.Count(out, "- 5 quotes cap (enforced)."); n != 1 {
		t.Fatalf("the repeated block must appear exactly once, got %d", n)
	}
	if !strings.Contains(out, "[repetition trimmed x19]") {
		t.Fatalf("the trim must be marked with the dropped count: %q", out)
	}
	if len(out) >= len(text) {
		t.Fatalf("trimmed output (%d) must be shorter than the input (%d)", len(out), len(text))
	}
	if !strings.HasPrefix(note, `repetition loop (20x "- 100 words cap for summary (enfor`) {
		t.Fatalf("stop note = %q", note)
	}
}

// TestTrimRepetitionLoopIsAByteForByteNoOpOnACleanAnswer: every answer that is
// not a loop must pass through untouched — the guard runs on EVERY final.
func TestTrimRepetitionLoopIsAByteForByteNoOpOnACleanAnswer(t *testing.T) {
	text := "summary:\nExo-Bench measures TPS.\nnumbers:\n" + distinctList(30)
	out, note, ok := TrimRepetitionLoop(text)
	if ok || note != "" || out != text {
		t.Fatalf("clean answer touched: ok=%v note=%q changed=%v", ok, note, out != text)
	}
}

// TestRepetitionNoteClipsTheQuotedLine: the note is one grep-able line, so the
// quoted evidence is bounded at 40 characters however long the looped line is.
func TestRepetitionNoteClipsTheQuotedLine(t *testing.T) {
	long := "- " + strings.Repeat("a very long looped list item ", 6) + "\n"
	_, note, ok := TrimRepetitionLoop("numbers:\n" + repeat(long, 5))
	if !ok {
		t.Fatal("a five-fold repeat of a long line is a loop")
	}
	quoted := strings.TrimSuffix(note[strings.Index(note, `"`)+1:strings.LastIndex(note, `"`)], "…")
	if len([]rune(quoted)) > 40 {
		t.Fatalf("quoted evidence is %d chars, want <= 40: %q", len(quoted), quoted)
	}
}
