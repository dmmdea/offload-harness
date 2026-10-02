// Package leakgate is the pure scanner behind the H-55 leak gate.
//
// The repository is public, so the names of the operator's machines, people and
// brands must never reach the tracked tree. The gate scans the tracked files
// (and their names) for a list of denied entries and fails on any hit. The list
// itself is a secret: the committed form of it is a file of keyed digests
// (HMAC-SHA256), so the gate can say "this file contains a denied name" without
// the repository ever holding the name.
//
// This package is the whole matcher and nothing else. It reads no environment
// variable and starts no process: the key is passed in, the file system is a
// small interface (so an unreadable file, a deleted file or a 17 MiB file is a
// unit test, not a disk experiment), and the git plumbing lives in the command
// that calls it. The pieces:
//
//   - entries and the list file (this file), the matcher and tokenizer (match.go);
//   - the fold that makes encoded spellings visible (fold.go, foldtable.go);
//   - the PNG chunk walk (png.go) and the tree scan with its fail-closed rules
//     (scan.go);
//   - the keyless shape rules (shape.go);
//   - the digest file, its MAC and key selection (digest.go) and key resolution
//     (key.go).
package leakgate

import (
	"fmt"
	"strings"
)

// Mode is how an entry matches. See the package documentation of the design:
//
//	sub     any window of one alphanumeric run (lower case)
//	word    a sub window with letter boundaries (a camel cut, a digit or a
//	        non-letter ends a word; a plural or a longer word does not match)
//	exact   the whole run equals the entry (no camel splitting)
//	subcs   a sub window compared as written, case preserved
//	phrase  two or three consecutive runs on one line
type Mode string

// The five entry modes.
const (
	ModeSub    Mode = "sub"
	ModeWord   Mode = "word"
	ModeExact  Mode = "exact"
	ModeSubCS  Mode = "subcs"
	ModePhrase Mode = "phrase"
)

const numSets = 5

// Index of each mode in the matcher's tables.
const (
	setSub = iota
	setWord
	setExact
	setSubCS
	setPhrase
)

var setCodes = [numSets]byte{'s', 'w', 'x', 'c', 'p'}

func (m Mode) index() int {
	switch m {
	case ModeSub:
		return setSub
	case ModeWord:
		return setWord
	case ModeExact:
		return setExact
	case ModeSubCS:
		return setSubCS
	case ModePhrase:
		return setPhrase
	}
	return -1
}

// Entry is one denied name. Text is normalized: lower case (case preserved for
// subcs) and, for a phrase, words joined by one space.
type Entry struct {
	Mode Mode
	Text string
}

// Limits of an entry (the generator refuses anything outside them).
const (
	minWindowLen = 4  // sub, subcs, word
	minExactLen  = 3  // exact
	maxEntryLen  = 64 // any entry; a run over 96 characters is never scanned
	// maxRun is the longest run the scanner reads. A longer run is a hash or a
	// base64 blob: it is reported (a fatal in the tree gate), never skipped
	// silently.
	maxRun = 96
)

// The canary is built from two literals so that no file of this tree holds the
// whole string; the generator always appends it and every scan must flag it.
const (
	canaryHead = "zqleak"
	canaryTail = "canary9"
)

// CanaryText is the entry the generator always appends (mode sub).
func CanaryText() string { return canaryHead + canaryTail }

// WithCanary returns a copy of es with the canary appended.
func WithCanary(es []Entry) []Entry {
	out := make([]Entry, 0, len(es)+1)
	out = append(out, es...)
	return append(out, Entry{Mode: ModeSub, Text: CanaryText()})
}

// ParseList reads the plaintext list: one entry per line, `mode<TAB>text`, a
// line whose first non-blank character is `#` is a comment. Entries are
// normalized and validated; an error names the line and never the entry text
// (the text may be a real name and error messages travel).
func ParseList(b []byte) ([]Entry, error) {
	var es []Entry
	var lines []int
	for i, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(raw, "\r")
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("line %d: expected mode<TAB>text", i+1)
		}
		es = append(es, Entry{Mode: Mode(strings.ToLower(strings.TrimSpace(line[:tab]))), Text: line[tab+1:]})
		lines = append(lines, i+1)
	}
	out, err := prepare(es, lines)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ValidateEntries applies the generator's validation to a list (entries are
// normalized first): at least four characters for sub, subcs and word, three
// for exact, two or three words for a phrase, ASCII letters and digits only
// (spaces only between phrase words), no duplicate within a mode.
func ValidateEntries(es []Entry) error {
	_, err := prepare(es, nil)
	return err
}

// prepare normalizes and validates; lines, when given, are the 1-based source
// lines used in messages.
func prepare(es []Entry, lines []int) ([]Entry, error) {
	where := func(i int) string {
		if lines != nil {
			return fmt.Sprintf("line %d", lines[i])
		}
		return fmt.Sprintf("entry %d", i+1)
	}
	if len(es) == 0 {
		return nil, fmt.Errorf("no entries")
	}
	out := make([]Entry, 0, len(es))
	seen := map[string]bool{}
	for i, e := range es {
		if e.Mode.index() < 0 {
			return nil, fmt.Errorf("%s: unknown mode", where(i))
		}
		text := strings.TrimSpace(e.Text)
		if e.Mode == ModePhrase {
			text = strings.Join(strings.Fields(text), " ")
		}
		if e.Mode != ModeSubCS {
			text = strings.ToLower(text)
		}
		if text == "" {
			return nil, fmt.Errorf("%s: empty text", where(i))
		}
		if len(text) > maxEntryLen {
			return nil, fmt.Errorf("%s: longer than %d characters", where(i), maxEntryLen)
		}
		words := []string{text}
		if e.Mode == ModePhrase {
			words = strings.Split(text, " ")
			if len(words) < 2 || len(words) > 3 {
				return nil, fmt.Errorf("%s: a phrase is two or three words", where(i))
			}
		}
		for _, w := range words {
			if !isAlnumString(w) {
				return nil, fmt.Errorf("%s: only ASCII letters and digits are allowed (spaces only between phrase words)", where(i))
			}
		}
		switch e.Mode {
		case ModeSub, ModeSubCS, ModeWord:
			if len(text) < minWindowLen {
				return nil, fmt.Errorf("%s: a %s entry needs at least %d characters", where(i), e.Mode, minWindowLen)
			}
		case ModeExact:
			if len(text) < minExactLen {
				return nil, fmt.Errorf("%s: an exact entry needs at least %d characters", where(i), minExactLen)
			}
		}
		key := string(e.Mode) + "\x00" + text
		if seen[key] {
			return nil, fmt.Errorf("%s: duplicate entry in mode %s", where(i), e.Mode)
		}
		seen[key] = true
		out = append(out, Entry{Mode: e.Mode, Text: text})
	}
	return out, nil
}

func isAlnumString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !alnum[s[i]] {
			return false
		}
	}
	return true
}

// Finding is one hit. Path is the tracked path; a name finding (Name) is a hit
// in the path itself. Line is 1-based; Col is the 1-based byte offset in the
// folded line. A plaintext matcher fills Text with the entry text; a digest
// matcher fills ID with the first four bytes of the entry digest (hex) and
// leaves Text empty, so its output never carries matched text. Shape findings
// carry the rule name in Text.
type Finding struct {
	Path string
	Line int
	Col  int
	Mode string
	ID   string
	Text string
	Name bool
}

// Label is the entry part of a finding line: "mode:text" for a plaintext
// matcher (the oracle's form), "id=<8 hex> mode" for a digest matcher, and
// "shape:<rule>" for a shape rule.
func (f Finding) Label() string {
	switch {
	case f.Mode == "shape":
		return "shape:" + f.Text
	case f.Text != "":
		return f.Mode + ":" + f.Text
	default:
		return "id=" + f.ID + " " + f.Mode
	}
}

// DisplayPath is Path, with " (name)" appended for a name finding.
func (f Finding) DisplayPath() string {
	if f.Name {
		return f.Path + " (name)"
	}
	return f.Path
}

// String is the output form: path:line:col label.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d %s", f.DisplayPath(), f.Line, f.Col, f.Label())
}
