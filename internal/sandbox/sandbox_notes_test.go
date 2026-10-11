package sandbox

import (
	"strings"
	"testing"
)

// The cage's notes (see noteMarker) are plain lines at the start of a run's stderr. These tests pin
// the few rules the platforms rely on, on every platform.

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
