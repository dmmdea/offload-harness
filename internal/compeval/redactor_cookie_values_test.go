package compeval

import (
	"reflect"
	"testing"
)

// TestCookieValuesReadsEveryValueOfASpan: the values the value-tail gate remembers, as they stand in the text: a bare
// value to the first space, quote, comma, semicolon, ampersand or backslash (a backslash-u escape is part of
// it); a quoted value between its quotes at the level it was opened, whatever stands inside, to the end of the
// line when the quote is never closed. Short values and the Set-Cookie attributes are not values.
func TestCookieValuesReadsEveryValueOfASpan(t *testing.T) {
	a, b := tailFake(931, 16, tailAlnum), tailFake(932, 16, tailAlnum)
	v := tailFake(933, 32, tailAlnum)
	bsu := "\x5c" + "u"
	for _, c := range []struct {
		name string
		span string
		want []string
	}{
		{"bare", "Cookie: sid=" + v, []string{v}},
		{"bare ends at a semicolon", "Cookie: sid=" + v + "; theme=dark", []string{v}},
		{"bare ends at a space", "Cookie: sid=" + v + " theme=dark", []string{v}},
		{"bare ends at an ampersand", "Cookie: sid=" + a + "&" + b, []string{a}},
		{"bare ends at a comma", "Cookie: sid=" + a + "," + b, []string{a}},
		{"bare ends at a backslash", "Cookie: sid=" + a + `\n` + b, []string{a}},
		{"bare takes a unicode escape", "Cookie: sid=" + a + bsu + "0026" + b, []string{a + bsu + "0026" + b}},
		{"bare keeps an equals sign", "Cookie: sid=" + a + "=" + b, []string{a + "=" + b}},
		{"short values are not remembered", "Cookie: a=short; b=" + v, []string{v}},
		{"attributes are not remembered", spanSetCk + ": sid=" + v + "; Domain=subdomain.example.invalid; Path=/some/long/path/here; Expires=Wed", []string{v}},
		{"quoted with a space", `Cookie: m="` + a + ` ` + b + `"; x=y`, []string{a + ` ` + b}},
		{"quoted with every character", `Cookie: m="` + a + `'&,;` + "\t" + `$` + "`" + b + `"`, []string{a + `'&,;` + "\t" + `$` + "`" + b}},
		{"quoted, escaped quote inside", `Cookie: m="` + a + `\"` + b + `"; x=y`, []string{a + `\"` + b}},
		{"quoted, escaped backslash before the closing quote", `Cookie: m="` + a + b + `\\"; x=` + v, []string{a + b + `\\`, v}},
		{"quoted at level 1", `Cookie: m=\"` + a + ` ` + b + `\"; x=` + v, []string{a + ` ` + b, v}},
		{"quoted at level 1, escaped quote inside", `Cookie: m=\"` + a + `\\\"` + b + `\"; x=` + v, []string{a + `\\\"` + b, v}},
		{"quoted at level 1, escaped backslash before the closing quote", `Cookie: m=\"` + a + b + `\\\\\"; x=` + v, []string{a + b + `\\\\`, v}},
		{"quoted at level 3", `Cookie: m=\\\"` + a + `\\$x ` + b + `\\\"; x=` + v, []string{a + `\\$x ` + b, v}},
		{"quoted, never closed", `Cookie: m="` + a + ` ` + b, []string{a + ` ` + b}},
		{"quoted, a line break inside an open quote is part of the value", "Cookie: m=\"" + a + "\n" + b + `"`, []string{a + "\n" + b}},
		{"quoted, escaped line break inside an open quote", `Cookie: m=\"` + a + `\n` + b + `\"; x=` + v, []string{a + `\n` + b, v}},
		{"quoted, a fold inside an open quote", "Cookie: m=\"" + a + "\r\n " + b + `"; x=` + v, []string{a + "\r\n " + b, v}},
		{"bare takes a shell escape", "Cookie: sid=" + a + `\$` + b, []string{a + `\$` + b}},
		{"bare takes a backslash and a space", "Cookie: sid=" + a + `\ ` + b, []string{a + `\ ` + b}},
		{"bare ends at an escaped line break", "Cookie: sid=" + a + `\n` + b, []string{a}},
		{"quoted at level 7", `Cookie: m=\\\\\\\"` + a + ` ` + b + `\\\\\\\"; x=` + v, []string{a + ` ` + b, v}},
		{"quoted at level 15", `Cookie: m=\\\\\\\\\\\\\\\"` + a + ` ` + b + `\\\\\\\\\\\\\\\"; x=` + v, []string{a + ` ` + b, v}},
		{"two values, the second quoted", `Cookie: sid=` + v + `; m="` + a + ` ` + b + `"`, []string{v, a + ` ` + b}},
		{"empty value", `Cookie: a=; b=` + v, []string{v}},
		{"list", spanSetCk + `":["theme=dark","session=` + v + `; Path=/","other=` + a + b, []string{v, a + b}},
	} {
		got := cookieValues(c.span)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: values %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCookieQuoteCloses: a quote closes a value opened with b backslashes when the run of backslashes before it is b
// plus a multiple of 2b+2 (the escaped backslashes of the value); a quote inside the value carries 2b+1.
func TestCookieQuoteCloses(t *testing.T) {
	for _, c := range []struct {
		k, b int
		want bool
	}{
		{0, 0, true}, {1, 0, false}, {2, 0, true}, {3, 0, false}, {4, 0, true},
		{1, 1, true}, {3, 1, false}, {5, 1, true}, {7, 1, false}, {0, 1, false},
		{3, 3, true}, {7, 3, false}, {11, 3, true}, {2, 3, false},
		{7, 7, true}, {15, 7, false}, {23, 7, true}, {6, 7, false},
		{15, 15, true}, {31, 15, false}, {47, 15, true},
	} {
		if got := cookieQuoteCloses(c.k, c.b); got != c.want {
			t.Errorf("cookieQuoteCloses(%d, %d) = %v, want %v", c.k, c.b, got, c.want)
		}
	}
}

// TestCookieNameStopsAtEveryDelimiter: the name in front of an equals sign is read back to the first white space,
// quote, comma, semicolon, ampersand, backslash, colon, bracket, brace, paren or angle bracket, so a Set-Cookie
// attribute after any of them is known by its name and is not remembered as a value.
func TestCookieNameStopsAtEveryDelimiter(t *testing.T) {
	v := tailFake(941, 32, tailAlnum)
	for _, c := range []string{" ", "\t", "\r", "\n", "'", `"`, ",", ";", "&", `\`, ":", "[", "]", "{", "}", "(", ")", "<", ">"} {
		span := spanSetCk + ": sid=" + v + "; x" + c + "Path=/some/long/path/here"
		got := cookieValues(span)
		if len(got) != 1 || got[0] != v {
			t.Errorf("delimiter %q: values %q, want only the session value", c, got)
		}
	}
}
