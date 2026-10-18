package compeval

import (
	"reflect"
	"strings"
	"testing"
)

// The guards here were found by deleting them one at a time (a table of single-line mutants of the cookie class
// and of the harvest scanner): the suite stayed green without them. Each test is green against the code as it stands
// and red against the mutant that removes the guard it names.

// TestCookieBareEscapeAtStopsAtTheEndOfTheSpan: a backslash run that reaches the end of the span has no character
// behind it to escape. Without the bound the scanner reads one past the end of the span and the redactor panics on a
// list cut in a backslash run.
func TestCookieBareEscapeAtStopsAtTheEndOfTheSpan(t *testing.T) {
	for _, c := range []struct {
		span string
		at   int
	}{
		{`ab\\`, 2},
		{`ab\\\`, 2},
		{`ab\`, 2},
		{`\\`, 0},
	} {
		func() {
			defer func() {
				if x := recover(); x != nil {
					t.Errorf("cookieBareEscapeAt(%q, %d) panicked: %v", c.span, c.at, x)
				}
			}()
			if got := cookieBareEscapeAt(c.span, c.at); got != 0 {
				t.Errorf("cookieBareEscapeAt(%q, %d) = %d, want 0", c.span, c.at, got)
			}
		}()
	}
}

// TestRedactorSurvivesATextCutInABackslashRun: the text is cut right after a run of backslashes, in a list string, in a
// bare value and in a line, and the redactor neither panics nor lets the value through.
func TestRedactorSurvivesATextCutInABackslashRun(t *testing.T) {
	v := tailFake(7001, 26, tailAlnum)
	bs := `\\`
	for name, text := range map[string]string{
		"set-cookie list, two backslashes":   spanSetCk + `:["a=` + v + bs,
		"set-cookie list, three backslashes": spanSetCk + `:["a=` + v + bs + `\`,
		"cookie list, two backslashes":       spanCk + `:["a=b; sid=` + v + bs,
		"line, two backslashes":              spanCk + ": a=" + v + bs,
		"line, one backslash":                spanCk + ": a=" + v + `\`,
	} {
		func() {
			defer func() {
				if x := recover(); x != nil {
					t.Errorf("%s: the redactor panicked: %v", name, x)
				}
			}()
			reviewRedact(t, name, text, v)
		}()
	}
	if got := cookieValues(`sid=` + v + bs); len(got) != 1 {
		t.Errorf("a bare value cut in a backslash run: got %q, want the one value", got)
	}
}

// TestCookieValuesReadsTheCookieBehindAQuotedValue: the walk resumes at the closing quote of a quoted value, so an equals
// sign right behind it is read as the next cookie. Resuming one character later would skip it.
func TestCookieValuesReadsTheCookieBehindAQuotedValue(t *testing.T) {
	a, b := tailFake(7011, 18, tailAlnum), tailFake(7012, 30, tailAlnum)
	got := cookieValues(`m="` + a + `"=` + b)
	if want := []string{a, b}; !reflect.DeepEqual(got, want) {
		t.Errorf("values %q, want %q", got, want)
	}
	if got := cookieValues(`m="` + a + `"; x="` + strings.Repeat("z", 13) + `"`); len(got) != 2 {
		t.Errorf("two quoted values: got %q", got)
	}
}
