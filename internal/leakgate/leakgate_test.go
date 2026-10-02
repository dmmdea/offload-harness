package leakgate

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	bs = "\\"
	bt = "`"
)

// gpu keeps "GPU-" apart from the head so the test source never spells a
// real-shaped card id (the keyless shape rule scans this file too).
func gpu(head string) string { return "GPU-" + head }

// distro keeps the UNC prefix apart from the distro name for the same reason.
func unc(sep, name, rest string) string {
	return sep + sep + "wsl.localhost" + sep + name + rest
}

type matrixCase struct {
	label string
	text  string
	want  bool
}

// matrixCases is the 108-case behaviour matrix (107 rows plus the line-number
// invariance check below), with the synthetic names of synthList mapped onto the
// classes of the real list. A row is true when the scanner or a shape rule must
// flag the text.
func matrixCases() []matrixCase {
	bsl := bs
	return []matrixCase{
		// ---- S1: 8+ hex digits, or a 4-7 digit head only in a truncated-UUID shape (31)
		{"S1 full uuid head", "id " + gpu("9b1c42e7") + "-5d3a-4f08-91ce-77aa00bb11cc here", true},
		{"S1 8-hex short pin", "pin " + gpu("9b1c42e7") + " in config", true},
		{"S1 upper short pin", "pin " + gpu("9B1C42E7") + " in config", true},
		{"S1 12-hex head", "pin " + gpu("9b1c42e75d3a") + " in config", true},
		{"S1 8-decimal-digit head", "pin " + gpu("12345678") + " in config", true},
		{"S1 4-hex three-dot ellipsis", "pin " + gpu("9b1c") + "... in comment", true},
		{"S1 4-hex unicode ellipsis", "pin " + gpu("9b1c") + "\u2026 in comment", true},
		{"S1 4-hex head plus two uuid groups", "pin " + gpu("9b1c") + "-5d3a-4f08 sample", true},
		{"S1 placeholder head plus group", "GPU-8888bbbb-9999 sample", false},
		{"S1 placeholder full", "GPU-1111aaaa-2222-3333-4444-555566667777", false},
		{"S1 placeholder zeros", "GPU-00000000-0000-0000-0000-000000000000", false},
		{"S1 placeholder aaaa", "GPU-aaaa example", false},
		{"S1 placeholder aaaa fixture", "\"0, GPU-aaaa, NVIDIA GeForce\"", false},
		{"S1 model number A100", "a GPU-A100 class card", false},
		{"S1 model number 4090", "a GPU-4090 build", false},
		{"S1 model number dual 5070", "a dual-GPU-5070 box", false},
		{"S1 model number 2080 Ti", "GPU-2080 Ti", false},
		{"S1 model number H100 (H is not hex)", "GPU-H100 node", false},
		{"S1 model number A100-80GB", "GPU-A100-80GB", false},
		{"S1 one trailing group is not a uuid shape", "GPU-4090-2024 refresh", false},
		{"S1 four digits without ellipsis", "GPU-1234 is a made-up id", false},
		{"S1 word head with ellipsis and no digit", "the GPU-cafe\u2026 lounge", false},
		{"S1 prose accelerated", "a GPU-accelerated build", false},
		{"S1 prose effective", "GPU-effective on 16 GB", false},
		{"S1 prose added", "a GPU-added column", false},
		{"S1 prose cafe", "the GPU-cafe lounge", false},
		{"S1 prose deadlock", "a GPU-deadlock detector", false},
		{"S1 prose bound", "GPU-bound loads", false},
		{"S1 prose accelerated2", "GPU-accelerated2", false},
		{"S1 prose faced", "GPU-faced with", false},
		{"S1 prose env", "its own GPU-env injection", false},
		// ---- S2: a WSL UNC path naming a real distro; public distro names pass (9)
		{"S2 real distro UNC", unc(bsl, "scratchbox", bsl+"opt"+bsl+"x"), true},
		{"S2 non-public distro with a public prefix", unc(bsl, "Ubuntu-"+"Lab", bsl+"opt"+bsl+"x"), true},
		{"S2 placeholder", "//wsl.localhost/<distro>/opt/x", false},
		{"S2 literal distro word", "//wsl.localhost/distro/opt/x", false},
		{"S2 public Ubuntu", unc(bsl, "Ubuntu", bsl+"home"), false},
		{"S2 public Ubuntu-24.04 on the dollar form", bsl + bsl + "wsl$" + bsl + "Ubuntu-24.04" + bsl + "home", false},
		{"S2 public Debian", "//wsl.localhost/Debian/x", false},
		{"S2 public kali-linux", "//wsl.localhost/kali-linux/x", false},
		{"S2 public docker-desktop-data", "//wsl.localhost/docker-desktop-data/x", false},
		// ---- case rule (11)
		{"subcs lower distro", "wsl.exe -d froodtoken -u root", true},
		{"subcs distribution flag", "wsl --distribution froodtoken", true},
		{"subcs UNC", unc(bsl, "froodtoken", bsl+"x"), true},
		{"subcs mnt path", "/mnt/wsl/froodtoken", true},
		{"subcs the distro", "the froodtoken distro", true},
		{"subcs vhdx", "froodtoken.vhdx", true},
		{"subcs opt path", "/opt/froodtoken/serve.sh", true},
		{"subcs product case passes", "FroodToken (Acme engine)", false},
		{"subcs hyphenated product passes", "llama.cpp-vs-FroodToken", false},
		{"subcs org slash product passes", "Acme-org/FroodToken#240", false},
		{"subcs file name", "0027-froodtoken-is-the-big-opt-in-engine.md", true},
		// ---- exact, word, sub (16)
		{"exact beside a sub name", "Gribble 8128", true},
		{"exact bare", "the 8128 box", true},
		{"exact digit prefix passes", "listen on :38128", false},
		{"exact inside a hex blob passes", "sha 9f8128ab", false},
		{"word bare", "a Quuxel box", true},
		{"word plural and suffix pass", "Quuxeled Quuxele", false},
		{"exact letter-bearing", "laptop 33K", true},
		{"exact letter suffix passes", "font-size:33kx", false},
		{"sub camel-glued", "zorblaxDevices", true},
		{"sub underscore-joined", "zorblax_optane", true},
		{"word beside a first name", "Dana Quuxel", true},
		{"word extended by letters passes", "quuxelhanchen", false},
		{"word extended by one letter passes", "github.com/quuxela/offload-harness", false},
		{"word between path separators", "/srv/quuxel/x", true},
		{"unlisted brand passes", "Acme Tools Auto Reviews", false},
		{"plain GPU word passes", "GPU 0 of 3", false},
		// ---- phrase and drive rules (5)
		{"drive phrase", "J:" + bs + "Frotz" + bs + "x", true},
		{"percent-d style text passes", "printf('%j frotz')", false},
		{"three-word drive phrase", "K:/Frotz Plover/x", true},
		{"glued phrase", "PlughXyzzy", true},
		{"sub inside a mail domain", "someone@gribblemail.com", true},
		// ---- fold: diacritics, JSON escape, zero width, full width (6)
		{"fold accent utf8", "Quux\u00e9l", true},
		{"fold json escape", "Quux" + bs + "u00e9l", true},
		{"fold zero width", "Quu\u200bxel", true},
		{"fold accent in a sub name", "Zorbl\u00e1x", true},
		{"fold full width", "\uff31\uff55\uff55\uff58\uff45\uff4c", true},
		{"fold json escaped ASCII letter", bs + "u007aorblax", true},
		// ---- escape glue: literal backslash and backtick escapes must not glue onto the next word (18)
		{"glue backslash n + word", bs + "nfrobnic", true},
		{"glue backslash n + word again", bs + "nquuxel", true},
		{"glue backslash t + word", bs + "tfrobnic", true},
		{"glue backslash r + word", bs + "rquuxel", true},
		{"glue backslash n + exact digits", bs + "n8128", true},
		{"glue backslash n + exact letter-bearing", bs + "n33K", true},
		{"glue backslash r + word (second)", bs + "rfrobnic", true},
		{"glue regex word boundary", bs + "bquuxel" + bs + "b", true},
		{"glue hex escape", bs + "x0aquuxel", true},
		{"glue backtick n", bt + "nquuxel", true},
		{"glue decoded control", "a" + bs + "u000a" + "quuxel", true},
		{"glue still flags a sub name", bs + "nzorblax", true},
		{"glue negative: newline word", "x" + bs + "nnewline y", false},
		{"glue negative: right boundary", bs + "nquuxele", false},
		{"glue negative: exact extended", bs + "n8128x", false},
		{"glue negative: placeholder user path", "C:" + bs + "Users" + bs + "<user>" + bs + "x", false},
		{"glue negative: printf newline", "printf(\"%d" + bs + "n\")", false},
		{"glue negative: tabular", bs + "tabular", false},
		// ---- percent-encoding and glued phrase forms (11)
		{"percent two-word phrase", "Plugh%20Xyzzy", true},
		{"percent drive phrase (pass B only)", "K:/Frotz%20Plover", true},
		{"percent lower phrase", "grue%20lamp", true},
		{"percent slash inside a phrase", "plugh%2Fxyzzy", true},
		{"percent then sub name", "%2Fzorblax", true},
		{"glued phrase camel", "GrueLamp", true},
		{"glued drive phrase", "KFrotzPlover", true},
		{"glued phrase lower", "the plughxyzzy box", true},
		{"glued phrase upper-camel", "PlughXyzzy and more", true},
		{"percent negative: 50%off", "50%off", false},
		{"percent negative: x %j frotz", "x %j frotz", false},
	}
}

// TestMatrixHasTheSpecifiedSize pins the matrix at 107 rows plus the
// line-number invariance check: the specification of the scanner is 108 cases.
func TestMatrixHasTheSpecifiedSize(t *testing.T) {
	if n := len(matrixCases()); n != 107 {
		t.Fatalf("matrix rows = %d, want 107 (+1 line-number invariance = 108)", n)
	}
}

// TestScannerBehaviourMatrix runs the matrix through the plaintext matcher and
// through the digest matcher (synthetic key): both must agree with the row.
func TestScannerBehaviourMatrix(t *testing.T) {
	plain := plainMatcher(t)
	digest, _ := digestMatcher(t, "ab", nil, nil)
	bad := 0
	for _, c := range matrixCases() {
		gotP, labelsP := flagged(plain, c.text)
		gotD, labelsD := flagged(digest, c.text)
		if gotP != c.want {
			bad++
			t.Errorf("plain  %-58s flagged=%v want %v %v", c.label, gotP, c.want, labelsP)
		}
		if gotD != c.want {
			bad++
			t.Errorf("digest %-58s flagged=%v want %v %v", c.label, gotD, c.want, labelsD)
		}
	}
	if bad > 0 {
		t.Fatalf("%d matrix mismatches", bad)
	}
}

// TestLineNumbersAreInvariantAcrossADecodedNewline: a decoded unicode escape for
// a line feed is a separator and never a newline, so a finding after it stays
// on its own line.
func TestLineNumbersAreInvariantAcrossADecodedNewline(t *testing.T) {
	m := plainMatcher(t)
	src := "l1\nl2 " + bs + "u000a quuxel\nl4"
	fs, _ := m.ScanText("x.md", []byte(src))
	if len(fs) == 0 {
		t.Fatal("no finding for the planted name")
	}
	for _, f := range fs {
		if f.Line != 2 {
			t.Errorf("finding on line %d, want 2 (a decoded line feed must not add a line)", f.Line)
		}
	}
}

// TestFindingsCarryTheOraclesColumnsAndLabels pins the observable output the
// parity check relies on: 1-based byte columns in the folded stream, entry
// labels as mode:text, every overlapping occurrence reported.
func TestFindingsCarryTheOraclesColumnsAndLabels(t *testing.T) {
	m := plainMatcher(t)
	fs, _ := m.ScanText("x.md", []byte("ab zorblaxzorblax\n  Plugh Xyzzy 8128"))
	var got []string
	for _, f := range fs {
		got = append(got, f.String())
	}
	sort.Strings(got)
	want := []string{
		"x.md:1:4 sub:zorblax",
		"x.md:1:11 sub:zorblax",
		"x.md:2:3 phrase:plugh xyzzy",
		"x.md:2:15 exact:8128",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings\n got %q\nwant %q", got, want)
	}
}

// TestOverlappingOccurrencesAllCount: the window modes report every occurrence,
// including overlapping ones, and a three-word phrase reports beside its
// two-word prefix.
func TestOverlappingOccurrencesAllCount(t *testing.T) {
	es := []Entry{{ModeSub, "abab"}, {ModePhrase, "k frotz"}, {ModePhrase, "k frotz plover"}}
	m, err := NewPlainMatcher(es)
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := m.ScanText("x", []byte("abababab\nK:/Frotz Plover"))
	count := map[string]int{}
	for _, f := range fs {
		count[f.Label()]++
	}
	if count["sub:abab"] != 3 {
		t.Errorf("overlapping windows = %d, want 3", count["sub:abab"])
	}
	if count["phrase:k frotz"] != 1 || count["phrase:k frotz plover"] != 1 {
		t.Errorf("phrase counts = %v, want both the 2-word and the 3-word match", count)
	}
}

// TestDriveLetterRule: a one-letter first word counts only before a colon or
// after /mnt/ (so a format verb or a stray letter never flags).
func TestDriveLetterRule(t *testing.T) {
	m := plainMatcher(t)
	cases := []struct {
		text string
		want bool
	}{
		{"J:/Frotz", true},
		{"J:   Frotz", true},
		{"/mnt/j/frotz", true},
		{"/mnt/J/Frotz/x", true},
		{"j frotz", false},
		{"%j frotz", false},
		{"/mnt2/j/frotz", false},
		{"/mn/j/frotz", false},
		{"jj:/frotz", false},
	}
	for _, c := range cases {
		got, labels := flagged(m, c.text)
		if got != c.want {
			t.Errorf("%q flagged=%v want %v %v", c.text, got, c.want, labels)
		}
	}
}

// TestPhraseDoesNotCrossALine: a line break is the one separator a phrase does
// not cross, which is also how a test splits a phrase at a word boundary.
func TestPhraseDoesNotCrossALine(t *testing.T) {
	m := plainMatcher(t)
	if got, _ := flagged(m, "x := \"plugh\" +\n\t\"xyzzy\""); got {
		t.Error("a phrase split across two lines was flagged")
	}
	if got, _ := flagged(m, "x := \"plugh\" + \"xyzzy\""); !got {
		t.Error("a phrase split at a word boundary on one line was not flagged")
	}
	if got, _ := flagged(m, "x := \"plu\" + \"gh xyzzy\""); got {
		t.Error("a phrase split inside a word was flagged")
	}
}

// TestSplitInsideAWordNeverSpellsTheEntry is the rule-7 contract: assembling a
// name from parts split inside the word leaves no run equal to the entry.
func TestSplitInsideAWordNeverSpellsTheEntry(t *testing.T) {
	m := plainMatcher(t)
	cases := []struct {
		text string
		want bool
	}{
		{`x := "zorb" + "lax"`, false},
		{`x := "zorblax"`, true},
		{`x := "81" + "28"`, false},
		{`x := "quu" + "xel"`, false},
		{`x := "quuxel"`, true},
		{`x := "froodto" + "ken"`, false},
	}
	for _, c := range cases {
		if got, labels := flagged(m, c.text); got != c.want {
			t.Errorf("%s flagged=%v want %v %v", c.text, got, c.want, labels)
		}
	}
}

// TestCanaryIsFlaggedAndABlindMatcherFails: the canary is appended by the
// generator and must be flagged by every matcher; a matcher built without it
// is blind and SelfTest says so.
func TestCanaryIsFlaggedAndABlindMatcherFails(t *testing.T) {
	canary := CanaryText()
	if canary == "" || strings.Contains(canary, " ") {
		t.Fatalf("canary %q must be one non-empty run", canary)
	}
	plain := plainMatcher(t)
	if err := plain.SelfTest(); err != nil {
		t.Errorf("plain matcher SelfTest: %v", err)
	}
	if got, _ := flagged(plain, "x "+canary+" y"); !got {
		t.Error("plain matcher does not flag the canary")
	}
	digest, df := digestMatcher(t, "ab", nil, nil)
	if err := digest.SelfTest(); err != nil {
		t.Errorf("digest matcher SelfTest: %v", err)
	}
	if got, _ := flagged(digest, "x "+canary+" y"); !got {
		t.Error("digest matcher does not flag the canary")
	}
	if n := len(df.Entries); n != len(synthEntries(t))+1 {
		t.Errorf("digest entries = %d, want the list plus exactly one canary (%d)", n, len(synthEntries(t))+1)
	}
	blind, err := NewPlainMatcher(synthEntries(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := blind.SelfTest(); err == nil {
		t.Error("a matcher without the canary passed SelfTest")
	}
	if got, _ := flagged(blind, "x "+canary+" y"); got {
		t.Error("a matcher built without the canary flagged it")
	}
}

// TestWordModeBoundaries pins the letter-boundary rules of word mode.
func TestWordModeBoundaries(t *testing.T) {
	m := plainMatcher(t)
	cases := []struct {
		text string
		want bool
	}{
		{"quuxel", true},
		{"Quuxel2", true},
		{"2quuxel", true},
		{"quuxel-box", true},
		{"my-quuxel", true},
		{"my_quuxel_box", true},
		{"myQuuxel", true},
		{"QuuxelBox", true},
		{"QUUXELBOX", false},
		{"quuxels", false},
		{"aquuxel", false},
		{"quuxeled", false},
		{"QUUXEL", true},
		{"QUUXEL box", true},
	}
	for _, c := range cases {
		if got, labels := flagged(m, c.text); got != c.want {
			t.Errorf("%q flagged=%v want %v %v", c.text, got, c.want, labels)
		}
	}
}

// TestExactModeWholeRunOnly: exact compares the whole run, lowercased, with no
// camel splitting.
func TestExactModeWholeRunOnly(t *testing.T) {
	m := plainMatcher(t)
	for _, c := range []struct {
		text string
		want bool
	}{
		{"8128", true}, {"x 8128 y", true}, {"x-8128-y", true}, {"x_8128", true},
		{"38128", false}, {"81281", false}, {"8128x", false}, {"ab8128", false},
		{"33k", true}, {"33K", true}, {"33kb", false}, {"a33k", false},
		{"Box33K", false},
	} {
		if got, labels := flagged(m, c.text); got != c.want {
			t.Errorf("%q flagged=%v want %v %v", c.text, got, c.want, labels)
		}
	}
}

// TestLongRunsAreReportedNotSkipped: a run over 96 characters is never
// scanned silently; the scanner reports its line.
func TestLongRunsAreReportedNotSkipped(t *testing.T) {
	m := plainMatcher(t)
	long := strings.Repeat("a", 97)
	_, runs := m.ScanText("x", []byte("ok\n"+long+"\nok"))
	if !reflect.DeepEqual(runs, []int{2}) {
		t.Fatalf("long-run lines = %v, want [2]", runs)
	}
	_, runs = m.ScanText("x", []byte(strings.Repeat("b", 96)))
	if len(runs) != 0 {
		t.Fatalf("a 96-character run is within the limit, got %v", runs)
	}
}

// TestNameScanKeysAndFolds: a name is scanned as one line, directories and file
// names alike, and the finding is marked as a name finding.
func TestNameScanKeysAndFolds(t *testing.T) {
	m := plainMatcher(t)
	fs := m.ScanName("docs/zorblax-notes/plain.md")
	if len(fs) != 1 || !fs[0].Name || fs[0].Line != 1 || fs[0].Label() != "sub:zorblax" {
		t.Fatalf("name findings = %+v", fs)
	}
	if got := m.ScanName("docs/Quux%C3%A9l.md"); len(got) != 0 {
		// %C3%A9 is not a decoded character for the name scan: the percent
		// forms blank in place, leaving "Quux" and "l" as separate runs.
		t.Errorf("percent-coded name flagged: %+v", got)
	}
	if got := m.ScanName("docs/Quux\u00e9l.md"); len(got) != 1 {
		t.Errorf("accented name findings = %+v, want 1", got)
	}
}

// TestFindingStringNeverCarriesMatchedText: digest-mode findings print an id and
// a mode, never the matched text (CI logs of a public repository are public).
func TestFindingStringNeverCarriesMatchedText(t *testing.T) {
	m, _ := digestMatcher(t, "ab", nil, nil)
	fs, _ := m.ScanText("a.md", []byte("here is zorblax and Quuxel"))
	if len(fs) != 2 {
		t.Fatalf("findings = %+v", fs)
	}
	for _, f := range fs {
		s := f.String()
		if strings.Contains(strings.ToLower(s), "zorblax") || strings.Contains(strings.ToLower(s), "quuxel") {
			t.Errorf("finding output carries matched text: %q", s)
		}
		if f.Text != "" {
			t.Errorf("digest finding carries plaintext %q", f.Text)
		}
		if len(f.ID) != 8 || !strings.Contains(s, "id="+f.ID) {
			t.Errorf("digest finding id %q not printed in %q", f.ID, s)
		}
	}
}
