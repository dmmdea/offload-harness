package piishape

import (
	"strings"
	"testing"
	"time"
)

// scanTime is the time of one scan of s for every match of the cookie-header class, the way a redactor makes them: the
// shortest of three trials, each the mean of as many scans as fit in 30 ms (the clock of some machines is too coarse for
// one scan of a short text).
func scanTime(s string) time.Duration {
	re := cookieClass().Re
	best := time.Duration(1<<63 - 1)
	for trial := 0; trial < 3; trial++ {
		n, t0 := 0, time.Now()
		for n < 1000 && time.Since(t0) < 30*time.Millisecond {
			re.FindAllStringIndex(s, -1)
			n++
		}
		if d := time.Since(t0) / time.Duration(n); d < best {
			best = d
		}
	}
	return best
}

func repeatTo(unit string, n int) string { return strings.Repeat(unit, n/len(unit)+1)[:n] }

// TestCookieClassScansLinearly: a text that repeats a short header over and over, every one of them opening a list that
// never closes, was scanned to its end once per match, so the time grew with the square of the length (4 times the text,
// 16 times the time). The units are the ones found by reviews: a header, a bracket, an apostrophe, an escaped equals
// sign and a quote; and a longer unit of quotes, backslashes and brackets. Four times the text must cost well under
// 16 times the time; linear is 4.
func TestCookieClassScansLinearly(t *testing.T) {
	bs := `\`
	for name, unit := range map[string]string{
		"apostrophe and escaped equals sign": "t-" + ckName + `:['` + bs + `="`,
		"escaped equals sign and a value":    ckName + `:[` + bs + `="0123456789ab"`,
		"equals signs and escaped quotes":    ckName + "==" + bs + `"=` + bs + `"]t-` + ckName + ":",
		"a slice literal behind them":        ckName + "==" + bs + `"=` + bs + `"][]string{e:`,
		"a slice literal, two headers":       bs + `"=` + bs + `"[]string{->` + ckName + ": " + sckName + ":" + bs + `"` + bs + bs,
		"quotes, backslashes and an escape":  sckName + ":=" + strings.Repeat(bs, 3) + `"=` + strings.Repeat(bs, 3) + `"]<` + bs + "u0026|",
		"quotes, backslashes and brackets": strings.Join([]string{
			ckName + ": ", `" => [`, `"`, `=` + strings.Repeat(bs, 15) + `"`, `: `, `a=`, `]`, strings.Repeat(bs, 4), `u0026`, `|`,
			sckName + `":[`, strings.Repeat(bs, 3) + `"`, `'`, bs, `=` + strings.Repeat(bs, 3) + `"`, `]`, `[]string{`}, ""),
	} {
		small, large := scanTime(repeatTo(unit, 4<<10)), scanTime(repeatTo(unit, 16<<10))
		if ratio := float64(large) / float64(small+time.Microsecond); ratio > 9 {
			t.Errorf("%s: 4 KiB took %v and 16 KiB %v, a factor of %.1f for 4 times the text (linear is 4, quadratic 16)", name, small, large, ratio)
		}
	}
}

// TestCookieClassScansHostileTextInTimeOfBenignText: 1 MiB-scale text of single quotes, equals signs and seven-backslash
// quotes behind a header opener is not much slower to scan than ordinary source text.
func TestCookieClassScansHostileTextInTimeOfBenignText(t *testing.T) {
	hostile := ckName + ":[" + repeatTo("'a="+strings.Repeat(`\`, 7)+`"a`, 256<<10)
	benign := repeatTo("func f(x int) string { return fmt.Sprintf(\"%d: %s\", x, name) }\n", 256<<10)
	h, b := scanTime(hostile), scanTime(benign)
	if ratio := float64(h) / float64(b+time.Microsecond); ratio > 9 {
		t.Errorf("256 KiB of hostile text took %v and the same of source text %v, a factor of %.1f", h, b, ratio)
	}
}

// TestCookieQuotedValueThatStartsWithACloserIsNoLongValue: a quote after an equals sign opens a value only when a value can
// start behind it, which is the rule that tells the quote that opens a value from the one that closes the string the header
// sits in. The long value that starts a match follows the same rule: a quote followed by a closer or a separator is the
// closing quote of what came before, and the 12 characters behind it are not a cookie value. Counted from that quote they made a
// match where the span's own walk saw no pair, so a text of short pairs and such quotes held a thread alive that found no
// long value while another route found one, and every match waited for it.
func TestCookieQuotedValueThatStartsWithACloserIsNoLongValue(t *testing.T) {
	v := valueFixture(881, 24, alnum)
	re := cookieClass().Re
	for _, first := range []string{"]", ")", "}", ";", ",", "|", "&", "<", ">", ":", " "} {
		if m := re.FindString(ckName + `: m="` + first + v + `"`); m != "" {
			t.Errorf("a quote followed by %q: matched %q", first, m)
		}
	}
	if m := re.FindString(ckName + `: m="x` + v + `"`); m == "" {
		t.Errorf("a quote followed by a word character: no match")
	}
}
