package leakgate

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestParseList(t *testing.T) {
	src := "# comment\r\n" +
		"\r\n" +
		"sub\tZorblax\r\n" +
		"word\t  Quuxel  \n" +
		"subcs\tFroodToken\n" +
		"exact\t33K\n" +
		"phrase\tPlugh    Xyzzy\n" +
		"phrase\tk Frotz  Plover\n" +
		"# trailing comment\n"
	es, err := ParseList([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{ModeSub, "zorblax"},
		{ModeWord, "quuxel"},
		{ModeSubCS, "FroodToken"},
		{ModeExact, "33k"},
		{ModePhrase, "plugh xyzzy"},
		{ModePhrase, "k frotz plover"},
	}
	if !reflect.DeepEqual(es, want) {
		t.Fatalf("entries = %+v", es)
	}
}

func TestParseListRefusesMalformedLines(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{"no tab", "sub zorblax\n", "line 1"},
		{"unknown mode", "fuzzy\tzorblax\n", "line 1"},
		{"short sub", "sub\tabc\n", "line 1"},
		{"empty text", "sub\t\n", "line 1"},
		{"second line is bad", "sub\tzorblax\nword\tab\n", "line 2"},
		{"duplicate", "sub\tzorblax\nsub\tZORBLAX\n", "line 2"},
		{"punctuation", "sub\tzor-blax\n", "line 1"},
		{"empty list", "# nothing\n\n", "no entries"},
	} {
		_, err := ParseList([]byte(c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a message naming %q", c.name, err, c.want)
			continue
		}
		// the message names the line, never the offending text (it may be a real name).
		if strings.Contains(err.Error(), "zorblax") && c.name != "second line is bad" {
			t.Errorf("%s: the message repeats the entry text: %v", c.name, err)
		}
	}
}

func TestWithCanary(t *testing.T) {
	es := synthEntries(t)
	n := len(es)
	out := WithCanary(es)
	if len(out) != n+1 || len(es) != n {
		t.Fatalf("WithCanary: %d -> %d (input now %d)", n, len(out), len(es))
	}
	last := out[len(out)-1]
	if last.Mode != ModeSub || last.Text != CanaryText() {
		t.Errorf("canary entry = %+v", last)
	}
	if c := CanaryText(); len(c) != 13 || strings.ToLower(c) != c {
		t.Errorf("canary %q: want one lower-case run of 13 characters", c)
	}
}

func TestPatternsMapping(t *testing.T) {
	es := WithCanary(synthEntries(t))
	got := Patterns(es)
	want := []string{
		"[Zz][Oo][Rr][Bb][Ll][Aa][Xx]",
		"[Gg][Rr][Ii][Bb][Bb][Ll][Ee]",
		"[Pp][Ll][Uu][Gg][Hh][Xx][Yy][Zz][Zz][Yy]",
		"[Gg][Rr][Uu][Ee][Ll][Aa][Mm][Pp]",
		"[Kk][Ff][Rr][Oo][Tt][Zz][Pp][Ll][Oo][Vv][Ee][Rr]",
		`[/\\]froodtoken([/\\.]|$)`,
		`-d[= ]+froodtoken`,
		"(^|[^A-Za-z])[Qq][Uu][Uu][Xx][Ee][Ll]([^A-Za-z]|$)",
		"(^|[^A-Za-z])[Ff][Rr][Oo][Bb][Nn][Ii][Cc]([^A-Za-z]|$)",
		"(^|[^A-Za-z0-9])8128([^A-Za-z0-9]|$)",
		"(^|[^A-Za-z0-9])33[Kk]([^A-Za-z0-9]|$)",
		"[Pp][Ll][Uu][Gg][Hh][^A-Za-z0-9]+[Xx][Yy][Zz][Zz][Yy]",
		"[Gg][Rr][Uu][Ee][^A-Za-z0-9]+[Ll][Aa][Mm][Pp]",
		"[Jj]:[^A-Za-z0-9]*[Ff][Rr][Oo][Tt][Zz]",
		"[Kk]:[^A-Za-z0-9]*[Ff][Rr][Oo][Tt][Zz][^A-Za-z0-9]+[Pp][Ll][Oo][Vv][Ee][Rr]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patterns\n got %q\nwant %q", got, want)
	}
	for _, p := range got {
		if strings.Contains(strings.ToLower(p), CanaryText()) {
			t.Errorf("the canary was emitted: %q", p)
		}
	}
}

// TestPatternsActuallyMatch compiles each pattern and checks it flags what its
// entry flags and passes what it passes (the hook greps with ERE; RE2 reads
// the same constructs).
func TestPatternsActuallyMatch(t *testing.T) {
	cases := []struct {
		entry Entry
		hit   []string
		miss  []string
	}{
		{Entry{ModeSub, "zorblax"}, []string{"ZorblaxDevices", "x_zorblax", "ZORBLAX"}, []string{"zorbla", "zorb lax"}},
		{Entry{ModeWord, "quuxel"}, []string{"a Quuxel box", "QUUXEL", "quuxel-2", "x:quuxel"}, []string{"quuxels", "aquuxel", "quuxeled"}},
		{Entry{ModeExact, "8128"}, []string{"the 8128 box", "x-8128-y"}, []string{"38128", "81281", "8128x", "a8128"}},
		{Entry{ModeExact, "33k"}, []string{"laptop 33K", "33k"}, []string{"33kb", "x33k"}},
		{Entry{ModePhrase, "plugh xyzzy"}, []string{"Plugh Xyzzy", "plugh/xyzzy", "plugh-xyzzy"}, []string{"plughxyzzy", "plugh", "xyzzy plugh"}},
		{Entry{ModePhrase, "j frotz"}, []string{"J:\\Frotz", "j: frotz", "J:/frotz"}, []string{"j frotz", "%j frotz"}},
		{Entry{ModePhrase, "k frotz plover"}, []string{"K:/Frotz Plover", "k:frotz/plover"}, []string{"k frotz plover"}},
		{Entry{ModeSubCS, "froodtoken"}, []string{"/opt/froodtoken/serve", "x\\froodtoken\\y", "/froodtoken.vhdx", "wsl -d froodtoken", "wsl -d=froodtoken"}, []string{"FroodToken product", "the froodtoken distro"}},
	}
	for _, c := range cases {
		ps := Patterns([]Entry{c.entry})
		for _, h := range c.hit {
			if !anyMatch(t, ps, h) {
				t.Errorf("%s %q: patterns %q do not match %q", c.entry.Mode, c.entry.Text, ps, h)
			}
		}
		for _, m := range c.miss {
			if anyMatch(t, ps, m) {
				t.Errorf("%s %q: patterns %q wrongly match %q", c.entry.Mode, c.entry.Text, ps, m)
			}
		}
	}
}

func anyMatch(t *testing.T, patterns []string, s string) bool {
	t.Helper()
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			t.Fatalf("pattern %q does not compile: %v", p, err)
		}
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func TestPatternBlockAndReplace(t *testing.T) {
	es := synthEntries(t)
	block := PatternBlock(es)
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "# BEGIN leak-gate") || !strings.HasPrefix(lines[len(lines)-1], "# END leak-gate") {
		t.Fatalf("block is not bracketed by BEGIN / END markers:\n%s", block)
	}
	if len(lines) != len(Patterns(es))+2 {
		t.Errorf("block has %d lines, want %d", len(lines), len(Patterns(es))+2)
	}
	// appended to a file without a block (the file's own final newline kept)
	out := ReplaceBlock("keep this\nand this\n", block)
	if !strings.HasPrefix(out, "keep this\nand this\n") || !strings.Contains(out, block) {
		t.Errorf("append: %q", out)
	}
	// replaced in place by a re-run, whatever surrounds it
	existing := "before\n" + PatternBlock([]Entry{{ModeSub, "oldname"}}) + "after\n"
	out = ReplaceBlock(existing, block)
	if strings.Contains(out, "oldname") || !strings.HasPrefix(out, "before\n") || !strings.HasSuffix(out, "after\n") || strings.Count(out, "# BEGIN leak-gate") != 1 {
		t.Errorf("replace: %q", out)
	}
	if again := ReplaceBlock(out, block); again != out {
		t.Errorf("a re-run is not idempotent:\n%q\n%q", out, again)
	}
	// CRLF files keep their line ending for the new block
	crlf := ReplaceBlock("a\r\nb\r\n", block)
	if strings.Contains(strings.ReplaceAll(crlf, "\r\n", ""), "\n") {
		t.Errorf("a CRLF file got LF-only lines: %q", crlf)
	}
	// an empty file
	if out := ReplaceBlock("", block); out != block {
		t.Errorf("empty file: %q", out)
	}
	// a file with no final newline
	if out := ReplaceBlock("a", block); !strings.HasPrefix(out, "a\n# BEGIN") {
		t.Errorf("no final newline: %q", out)
	}
}
