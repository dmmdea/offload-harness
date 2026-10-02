package leakgate

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// TestFoldTableIsPinned: the Latin fold table has exactly 244 entries (generated
// once from the canonical decompositions) and a spot list of known members and
// known non-members; a regenerated table that drifts fails here.
func TestFoldTableIsPinned(t *testing.T) {
	n := 0
	for _, b := range foldLatin {
		if b != 0 {
			n++
		}
	}
	if n != 244 {
		t.Fatalf("fold table has %d entries, want 244", n)
	}
	in := map[rune]byte{
		0x00C0: 'A', 0x00C9: 'E', 0x00D1: 'N', 0x00DC: 'U', 0x00E0: 'a', 0x00E9: 'e', 0x00ED: 'i',
		0x00F1: 'n', 0x00FC: 'u', 0x00FF: 'y', 0x0100: 'A', 0x0107: 'c', 0x010C: 'C', 0x0160: 'S',
		0x017E: 'z', 0x01CD: 'A', 0x01F4: 'G', 0x0232: 'Y', 0x0233: 'y',
	}
	for r, want := range in {
		if foldLatin[r] != want {
			t.Errorf("U+%04X folds to %q, want %q", r, foldLatin[r], want)
		}
	}
	out := []rune{0x00C6 /* AE */, 0x00D0 /* eth */, 0x00D7 /* times */, 0x00D8 /* O stroke */, 0x00DE /* thorn */, 0x00DF /* sharp s */, 0x00F7, 0x0131, 0x0141 /* L stroke */, 0x0153 /* oe */, 0x017F}
	for _, r := range out {
		if foldLatin[r] != 0 {
			t.Errorf("U+%04X is not decomposable to one ASCII letter, yet the table maps it to %q", r, foldLatin[r])
		}
	}
	for r := rune(0); r < 0xC0; r++ {
		if foldLatin[r] != 0 {
			t.Errorf("the table reaches below U+00C0: U+%04X", r)
		}
	}
}

func TestFoldClasses(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ascii untouched", "Plain text 123", "Plain text 123"},
		{"accented letters", "Quux" + string(rune(0x00E9)) + "l " + string(rune(0x00C0)) + string(rune(0x00F1)), "Quuxel An"},
		{"combining marks are dropped", "e" + string(rune(0x0301)) + "a" + string(rune(0x0300)), "ea"},
		{"zero-width characters rejoin a name", "Quu" + string(rune(0x200B)) + "x" + string(rune(0x200C)) + "e" + string(rune(0x200D)) + "l" + string(rune(0x2060)) + string(rune(0xFEFF)) + string(rune(0x00AD)) + "!", "Quuxel!"},
		{"full-width ASCII", string(rune(0xFF21)) + string(rune(0xFF42)) + string(rune(0xFF10)), "Ab0"},
		{"ligatures", "o" + string(rune(0xFB00)) + "i" + string(rune(0xFB01)) + "c" + string(rune(0xFB02)) + string(rune(0xFB03)) + string(rune(0xFB04)) + string(rune(0xFB05)) + string(rune(0xFB06)), "o" + "ff" + "i" + "fi" + "c" + "fl" + "ffi" + "ffl" + "st" + "st"},
		{"letters with no single-letter decomposition become separators", "a" + string(rune(0x00DF)) + "b" + string(rune(0x00D8)) + "c" + string(rune(0x00E6)) + "d", "a b c d"},
		{"non-Latin scripts become separators", "a" + string(rune(0x0416)) + "b" + string(rune(0x4E2D)) + "c", "a b c"},
		{"mathematical bold letters are not folded", "a\U0001d42ab", "a b"},
		{"invalid bytes become separators", "a\xffb\xc0c", "a b c"},
		{"an escaped letter decodes", "\\u0041\\u00e9", "Ae"},
		{"an escaped control becomes a space, never a newline", "a\\u000ab\\u0009c\\u007fd", "a b c d"},
		{"an escaped zero-width character disappears", "a\\u200bb", "ab"},
		{"an escaped surrogate pair is one separator", "a\\ud83d\\ude00b", "a b"},
		{"a lone escaped surrogate is a separator", "a\\ud800b", "a b"},
		{"a short escape is left alone", "a\\u00zzb\\u12", "a\\u00zzb\\u12"},
		{"upper-case hex digits", "\\u00C9", "E"},
		{"a newline stays", "a\nb", "a\nb"},
	}
	for _, c := range cases {
		if got := string(Fold([]byte(c.in))); got != c.want {
			t.Errorf("%s: Fold(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestNeedsFold(t *testing.T) {
	if NeedsFold([]byte("plain ascii \\n \\x41 %20")) {
		t.Error("plain ASCII with other escapes needs no fold")
	}
	if !NeedsFold([]byte("caf"+string(rune(0x00E9)))) || !NeedsFold([]byte("a\\u0041")) || !NeedsFold([]byte{0xff}) {
		t.Error("non-ASCII bytes and unicode escapes need the fold")
	}
}

// TestFoldNeverChangesTheLineStructure: whatever the input, the fold adds and
// removes no newline, so line numbers stay exact.
func TestFoldNeverChangesTheLineStructure(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	pieces := []string{"a", "Z", "\n", " ", "\\u000a", "\\u000d", "\\u2028", string(rune(0x2028)), "\u0085", string(rune(0x00E9)), string(rune(0x200B)), string(rune(0xD7FF)), "\U0001f600",
		"\xff", "\\ud83d\\ude00", "\\ud800", "\\u0000", "\\", "u", "0", "\r\n", "\\n", "%0a"}
	for i := 0; i < 2000; i++ {
		var b strings.Builder
		for j := r.Intn(30); j >= 0; j-- {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		in := []byte(b.String())
		out := Fold(in)
		if bytes.Count(in, []byte("\n")) != bytes.Count(out, []byte("\n")) {
			t.Fatalf("newline count changed: %q -> %q", in, out)
		}
		for _, c := range out {
			if c >= 0x80 {
				t.Fatalf("a non-ASCII byte survived the fold: %q -> %q", in, out)
			}
		}
	}
}

func TestBlankEscapes(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		changed bool
	}{
		{"x\\nfrobnic", "x  frobnic", true},
		{"\\tq\\rq\\bq\\fq\\vq\\aq\\eq\\0q", "  q  q  q  q  q  q  q  q", true},
		{"`nfrobnic", " " + " " + "frobnic", true},
		{"\\x0afrobnic", "    frobnic", true},
		{"\\X0afrobnic", "\\X0afrobnic", false},
		{"Plugh%20Xyzzy", "Plugh   Xyzzy", true},
		{"%2Fzorblax", "   zorblax", true},
		{"50%off", "50%off", false},
		{"%d dev", "%d dev", false},
		{"%zz", "%zz", false},
		{"\\\\nfrobnic", "\\  frobnic", true},
		{"\\n\\n", "    ", true},
		{"no escapes here", "no escapes here", false},
		{"\\g\\y\\1", "\\g\\y\\1", false},
	}
	for _, c := range cases {
		out, changed := BlankEscapes([]byte(c.in))
		if string(out) != c.want || changed != c.changed {
			t.Errorf("BlankEscapes(%q) = %q, %v; want %q, %v", c.in, out, changed, c.want, c.changed)
		}
		if len(out) != len(c.in) {
			t.Errorf("BlankEscapes(%q) changed the length", c.in)
		}
	}
}

// TestPassBOnlyAddsFindings: a path that pass A finds is still found when pass B
// blanks the escape beside it, and the union never reports one (line, column,
// entry) twice.
func TestPassBOnlyAddsFindings(t *testing.T) {
	m := plainMatcher(t)
	fs, _ := m.ScanText("x", []byte("C:"+bs+"zorblax"+bs+"x\n"+bs+"nzorblax"))
	seen := map[string]int{}
	for _, f := range fs {
		seen[f.String()]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("finding %q reported %d times", k, n)
		}
	}
	if len(fs) != 2 {
		t.Errorf("findings = %v, want one per line", fs)
	}
}
