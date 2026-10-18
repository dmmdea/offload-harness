package piishape

import (
	"strings"
	"testing"
	"time"
)

// scanTime is the shortest of three scans of s for every match of the cookie-header class, the way a redactor makes
// them.
func scanTime(s string) time.Duration {
	re := cookieClass().Re
	best := time.Duration(1<<63 - 1)
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		re.FindAllStringIndex(s, -1)
		if d := time.Since(t0); d < best {
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
