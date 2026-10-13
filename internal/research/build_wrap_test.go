package research

// A page the seat cannot read is a page it cannot digest. The agent's read_file cuts every line at 2,000 characters
// (" (line truncated)") and its offset and limit count LINES, so the rest of a line is out of reach: a page that
// arrives as one 98,304-character line (a minified document, a page whose block tags were stripped to nothing) gave the
// seat its first 2,020 characters and no way to the other 96,000. Research runs on such pages reached the forced final
// step in 38 of 117 cases, against 36 of 1,283 for pages with ordinary lines, and the digests that did arrive were
// about the first screen of the page. Build now hands the seat the page in lines it can read, breaking at spaces and
// losing nothing.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// agentLineCap is the per-line cap of the agent's read_file (internal/agent tools.go maxLineChars).
const agentLineCap = 2000

func oneLinePage(words int) string {
	var b strings.Builder
	for i := 0; i < words; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("word")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(string(rune('a' + i%26)))
	}
	return b.String()
}

func contextTextOf(t *testing.T, text string) string {
	t.Helper()
	specs, _ := Build(Request{Goal: "List what the page says about widgets."}, []Fetched{{URL: "https://docs.example/a", FinalURL: "https://docs.example/a", Text: text}})
	if len(specs) != 1 || len(specs[0].Context) != 1 {
		t.Fatalf("specs = %+v, want one contract with one context doc", specs)
	}
	return specs[0].Context[0].Text
}

func TestAPageThatIsOneHugeLineReachesTheSeatInLinesItCanRead(t *testing.T) {
	page := oneLinePage(14000) // about 98 KB, one line
	if utf8.RuneCountInString(page) < 90_000 || strings.Contains(page, "\n") {
		t.Fatalf("setup: page is %d runes with %d newlines", utf8.RuneCountInString(page), strings.Count(page, "\n"))
	}
	got := contextTextOf(t, page)
	for i, line := range strings.Split(got, "\n") {
		if n := utf8.RuneCountInString(line); n > agentLineCap-100 {
			t.Fatalf("line %d is %d runes: the seat's read_file would cut it at %d and the rest would be out of reach", i+1, n, agentLineCap)
		}
	}
	if strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(page), " ") {
		t.Fatal("the page the seat is handed has other words than the page that was fetched: a break must lose nothing and glue nothing")
	}
}

func TestALongWordWithNoSpaceIsCutWithoutLosingACharacter(t *testing.T) {
	blob := strings.Repeat("QUJD", 2500) // 10,000 characters, no space to break at
	orig := "before " + blob + " after"
	got := contextTextOf(t, orig)
	bare := strings.NewReplacer("\n", "", " ", "")
	if bare.Replace(got) != bare.Replace(orig) {
		t.Fatalf("characters were lost or added cutting the blob")
	}
	for i, line := range strings.Split(got, "\n") {
		if n := utf8.RuneCountInString(line); n > agentLineCap-100 {
			t.Fatalf("line %d is %d runes", i+1, n)
		}
	}
}

func TestAPageOfOrdinaryLinesIsHandedOverByteForByte(t *testing.T) {
	page := "The driven path uses shared memory transfers between processes.\nThe engine copies every buffer through pinned staging memory.\n\n" + strings.Repeat("A line of prose that is nowhere near too long. ", 20)
	if got := contextTextOf(t, page); got != page {
		t.Fatalf("a page whose lines all fit was rewritten:\n%q\nwant\n%q", got, page)
	}
}

func TestMultibyteTextIsBrokenOnRuneBoundaries(t *testing.T) {
	page := strings.Repeat("áéíóú ", 1200) // 7,200 runes, 13,200 bytes
	got := contextTextOf(t, page)
	if !utf8.ValidString(got) {
		t.Fatal("a break split a multibyte rune")
	}
	for i, line := range strings.Split(got, "\n") {
		if n := utf8.RuneCountInString(line); n > agentLineCap-100 {
			t.Fatalf("line %d is %d runes", i+1, n)
		}
	}
}

// TestALineThatFitsInRunesIsNotRewrittenBecauseItIsLongInBytes: the seat's cap is in characters.
func TestALineThatFitsInRunesIsNotRewrittenBecauseItIsLongInBytes(t *testing.T) {
	page := strings.Repeat("áéíóú ", 250) // 1,500 runes, 2,750 bytes
	if got := contextTextOf(t, page); got != page {
		t.Fatalf("a line of %d runes was rewritten for being %d bytes", utf8.RuneCountInString(page), len(page))
	}
}
