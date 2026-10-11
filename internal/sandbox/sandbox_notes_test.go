package sandbox

import (
	"strings"
	"testing"
)

// The cage's notes (see noteMarker) are plain lines at the start of a run's stderr. These tests pin
// the few rules the platforms rely on, on every platform, because the Linux worker writes the lines
// and the parent reads them back, and the Windows cage writes them for its own caller.

// TestNoteLinesRendersOneMarkedLinePerNote: marker, text, eol, in order, and nothing for no notes.
func TestNoteLinesRendersOneMarkedLinePerNote(t *testing.T) {
	if got := noteLines(nil, "\n"); got != "" {
		t.Errorf("no notes, no lines; got %q", got)
	}
	got := noteLines([]string{"first", "second"}, "\r\n")
	want := "[sandbox] first\r\n[sandbox] second\r\n"
	if got != want {
		t.Errorf("noteLines = %q, want %q", got, want)
	}
	if noteMarker != "[sandbox] " {
		t.Errorf("the marker is part of what callers see and the Windows timeout note already uses it; got %q", noteMarker)
	}
}

// TestAddfKeepsEveryNoteOnOneLine: an error text that carries a line break (a Win32 message can end
// with one) must not turn a note into two lines, or the second would read as the command's own
// stderr and the first would be cut short.
func TestAddfKeepsEveryNoteOnOneLine(t *testing.T) {
	var notes runNotes
	notes.addf("folder %s failed: %v", `C:\x y`, "denied.\r\nsecond line\nthird\r")
	if len(notes) != 1 {
		t.Fatalf("one addf, one note; got %d", len(notes))
	}
	if strings.ContainsAny(notes[0], "\r\n") {
		t.Errorf("a note is one line; got %q", notes[0])
	}
	if !strings.Contains(notes[0], `C:\x y`) {
		t.Errorf("the rest of the text, spaces in a path included, must survive; got %q", notes[0])
	}
}

// TestSkipLeadingNotes: the lines noteLines wrote are skipped, however they end, and nothing else is.
func TestSkipLeadingNotes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"no notes", "hello\n", "hello\n"},
		{"empty", "", ""},
		{"one note LF", "[sandbox] a\nhello\n", "hello\n"},
		{"two notes CRLF", "[sandbox] a\r\n[sandbox] b\r\nhello\r\n", "hello\r\n"},
		{"notes only", "[sandbox] a\n[sandbox] b\n", ""},
		{"notes only, no final line break", "[sandbox] a", ""},
		{"a note-looking line after the command's own text is the command's", "hello\n[sandbox] not ours\n", "hello\n[sandbox] not ours\n"},
		{"the marker must lead the line", " [sandbox] a\n", " [sandbox] a\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipLeadingNotes(tc.in); got != tc.want {
				t.Errorf("skipLeadingNotes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRefusedByReadsThroughNotes: a worker that wrote notes and then failed to exec has refused,
// and the caller must still be told it was a refusal rather than a command that exited 126 or 127.
// Output that merely contains the marker later is the command's, as before.
func TestRefusedByReadsThroughNotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"plain refusal", refusalMarker + "sandbox worker: landlock floor not met\n", true},
		{"refusal behind a note", noteMarker + ".git is not masked\n" + refusalMarker + "sandbox worker: exec failed\n", true},
		{"refusal behind two notes", noteMarker + "a\n" + noteMarker + "b\n" + refusalMarker + "x\n", true},
		{"a note and the command's own output", noteMarker + "a\nhello\n", false},
		{"the command's output", "hello\n", false},
		{"marker later in the command's output", "hello\n" + refusalMarker + "forged\n", false},
		{"marker after a note and output", noteMarker + "a\nhello\n" + refusalMarker + "forged\n", false},
		{"nothing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusedBy(tc.in); got != tc.want {
				t.Errorf("refusedBy(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
