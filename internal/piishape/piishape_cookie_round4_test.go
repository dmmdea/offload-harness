package piishape

import (
	"strings"
	"testing"
)

// The fourth review's findings, at the level of the class: the span each header shape takes, and the classes that name a
// credential header printed as a list and a Basic header behind a line break.

func spanOf(t *testing.T, name, text, want string) {
	t.Helper()
	if got := cookieClass().Re.FindString(text); got != want {
		t.Errorf("%s: matched %q in %q, want %q", name, got, text, want)
	}
}

// TestCookieListTakesBracketsInsideValuesAndNames: a closing bracket is a closer only when what stands behind it cannot be
// the rest of a cookie (white space, another closer, a quote, a comma, a semicolon, an angle bracket, the end of the text).
func TestCookieListTakesBracketsInsideValuesAndNames(t *testing.T) {
	a, b := valueFixture(9201, 22, alnum), valueFixture(9202, 32, alnum)
	for name, c := range map[string]struct{ text, want string }{
		"tail behind a bracket":      {"map[" + ckName + ":[sid=" + a + "]" + b + "; theme=dark]]", ckName + ":[sid=" + a + "]" + b + "; theme=dark"},
		"short head, bracket":        {"map[" + ckName + ":[sid=ab]" + b + "]]", ckName + ":[sid=ab]" + b},
		"subscript name":             {"map[" + ckName + ":[user[id]=" + a + "]]", ckName + ":[user[id]=" + a},
		"subscript name, two":        {"map[" + sckName + ":[a=" + a + " user[id]=" + b + "]]", sckName + ":[a=" + a + " user[id]=" + b},
		"value that starts with one": {"map[" + ckName + ":[lang=en sid=]" + a + " theme=dark]]", ckName + ":[lang=en sid=]" + a + " theme=dark"},
		"two subscripts":             {"map[" + ckName + ":[a[0][1]=" + a + "]]", ckName + ":[a[0][1]=" + a},
		"closer then a header":       {"map[" + ckName + ":[sid=" + a + "] Host:[x]]", ckName + ":[sid=" + a},
		"closer then end":            {"map[" + ckName + ":[sid=" + a + "]", ckName + ":[sid=" + a},
		"closer then a quote":        {`{"cmd":"map[` + ckName + ":[sid=" + a + `]"}`, ckName + ":[sid=" + a},
	} {
		spanOf(t, name, c.text, c.want)
	}
}

// TestCookieListTailOfAnApostropheStopsAtACloser: an apostrophe that opens no string of a Python list (it stands in the middle
// of a word) took the rest of the line, which in the Args of a tool call is the closing quote of the string and the brace.
func TestCookieListTailOfAnApostropheStopsAtACloser(t *testing.T) {
	a := valueFixture(9211, 22, alnum)
	spanOf(t, "apostrophe, closers behind", `{"cmd":"map[`+ckName+":[sid="+a+`'x]]"}`, ckName+":[sid="+a+"'x")
	spanOf(t, "apostrophe, text behind", "map["+ckName+":[sid="+a+"'x]] and more", ckName+":[sid="+a+"'x")
	spanOf(t, "apostrophe, a quote behind", `{"cmd":"map[`+ckName+":[sid="+a+`'x"}`, ckName+":[sid="+a+"'x")
	// A Python list cut in its last string is still taken to the end of the line.
	spanOf(t, "cut python list", "{'"+sckName+"': ['sid="+a+"', 'b=cdefghijklmnop; Path=/", sckName+"': ['sid="+a+"', 'b=cdefghijklmnop; Path=/")
}

// TestCookieGoLiteralOfAnyList: json.Unmarshal into an interface gives map[string]interface {} and []interface {}, and the
// Go-syntax print writes a list of those as []interface {}{...}; source writes []interface{}{...} and []any{...}.
func TestCookieGoLiteralOfAnyList(t *testing.T) {
	a := valueFixture(9221, 22, alnum)
	for name, c := range map[string]struct{ text, want string }{
		"printed":       {`map[string]interface {}{"` + ckName + `":[]interface {}{"sid=` + a + `"}}`, ckName + `":[]interface {}{"sid=` + a + `"`},
		"printed, set":  {`map[string]interface {}{"` + sckName + `":[]interface {}{"sid=` + a + `; Path=/"}}`, sckName + `":[]interface {}{"sid=` + a + `; Path=/"`},
		"source":        {`map[string]interface{}{"` + ckName + `":[]interface{}{"sid=` + a + `"}}`, ckName + `":[]interface{}{"sid=` + a + `"`},
		"any":           {`map[string]any{"` + ckName + `": []any{"sid=` + a + `"}}`, ckName + `": []any{"sid=` + a + `"`},
		"typed strings": {`map[string][]string{"` + ckName + `":[]string{"sid=` + a + `"}}`, ckName + `":[]string{"sid=` + a + `"`},
	} {
		spanOf(t, name, c.text, c.want)
	}
}

// TestCookieOpensBehindABytesPrefix: a b, r, u or f in front of the quote of a string is a prefix, not a word that ends the
// gap, in the comma form (a tuple) and behind a colon or an equals sign (a dict).
func TestCookieOpensBehindABytesPrefix(t *testing.T) {
	a, b := valueFixture(9231, 22, alnum), valueFixture(9232, 24, alnum)
	for name, c := range map[string]struct{ text, want string }{
		"tuple":          {"[(b'cookie', b'sid=" + a + "')]", "cookie', b'sid=" + a},
		"dict":           {"{b'" + ckName + "': b'sid=" + a + "'}", ckName + "': b'sid=" + a},
		"double quotes":  {`[(b"cookie", b"sid=` + a + `")]`, `cookie", b"sid=` + a},
		"dict, double":   {`{b"` + ckName + `": b"sid=` + a + `"}`, ckName + `": b"sid=` + a},
		"two cookies":    {"(b'cookie', b'sessionid=" + a + "; csrftoken=" + b + "')", "cookie', b'sessionid=" + a + "; csrftoken=" + b},
		"set-cookie":     {"[(b'" + sckName + "', b'sid=" + a + "; Path=/')]", sckName + "', b'sid=" + a + "; Path=/"},
		"raw string":     {"headers.append((r'" + ckName + "', u'sid=" + a + "'))", ckName + "', u'sid=" + a},
		"escaped quotes": {`(b\"cookie\", b\"sid=` + a + `\")`, `cookie\", b\"sid=` + a},
	} {
		spanOf(t, name, c.text, c.want)
	}
	// A word that ends in b is not a prefix.
	spanOf(t, "word before the quote", "cookie, web'sid="+a+"'", "")
}

// TestCookieNameTakesAnUnderscoreInFront: HTTP_COOKIE, http_cookie and HTTP_SET_COOKIE are the CGI, WSGI and web server
// spellings of the header.
func TestCookieNameTakesAnUnderscoreInFront(t *testing.T) {
	a, b := valueFixture(9241, 22, alnum), valueFixture(9242, 20, alnum)
	up, low := strings.ToUpper(ckName), strings.ToLower(ckName)
	for name, c := range map[string]struct{ text, want string }{
		"environ":      {"HTTP_" + up + "=sessionid=" + a + "; csrftoken=" + b, up + "=sessionid=" + a + "; csrftoken=" + b},
		"lower":        {"http_" + low + `="sessionid=` + a + `"`, low + `="sessionid=` + a},
		"dict":         {"{'HTTP_" + up + "': 'sessionid=" + a + "'}", up + "': 'sessionid=" + a},
		"set variant":  {"HTTP_SET_" + up + "=sid=" + a + "; Path=/", up + "=sid=" + a + "; Path=/"},
		"prefix a, no": {"mycookie_name_is_" + a, ""},
	} {
		spanOf(t, name, c.text, c.want)
	}
}

// TestCookieValueMayStartWithAnAmpersandOrABracket: a leading ampersand and a leading closing bracket are part of the value
// and count towards its 12 characters, the line and the list alike.
func TestCookieValueMayStartWithAnAmpersandOrABracket(t *testing.T) {
	a := valueFixture(9251, 22, alnum)
	for name, c := range map[string]struct{ text, want string }{
		"ampersand":         {ckName + ": lang=en; sid=&" + a + "; theme=dark", ckName + ": lang=en; sid=&" + a + "; theme=dark"},
		"ampersand, only":   {ckName + ": refresh_" + kwToken + "=&" + a, ckName + ": refresh_" + kwToken + "=&" + a},
		"bracket":           {sckName + ": sid=]" + a + "; Path=/", sckName + ": sid=]" + a + "; Path=/"},
		"list, bracket":     {"map[" + ckName + ":[sid=]" + a + "]]", ckName + ":[sid=]" + a},
		"line, inner":       {ckName + ": sid=ab]" + a, ckName + ": sid=ab]" + a},
		"ampersand, short":  {ckName + ": sid=&abcdef", ""},
		"ampersand, no run": {ckName + ": a=b&c=d", ""},
	} {
		spanOf(t, name, c.text, c.want)
	}
}

// TestCookieBareCountTakesACapitalEscape: a capital N, R or U after a backslash is no escape of a line break or of a
// character code (a Windows path, \Name); the class is case-insensitive, and the pattern that counts one as a single
// character has to stay case-sensitive inside it, or a value with one in its first 12 characters is not counted at all.
func TestCookieBareCountTakesACapitalEscape(t *testing.T) {
	a := valueFixture(9261, 24, alnum)
	for _, esc := range []string{`\N`, `\R`, `\U`, `\Q`} {
		text := ckName + ": sid=ab" + esc + "xyzQ" + a
		spanOf(t, esc+" in the count", text, text)
		spanOf(t, esc+" in the count, Args", `{"cmd":"`+strings.ReplaceAll(text, `\`, `\\`)+`"}`, strings.ReplaceAll(text, `\`, `\\`))
	}
}

// TestAssignmentTakesACredentialHeaderPrintedAsAList: X-Api-Key, Api-Key, X-Auth-Token and X-Amz-Security-Token print as
// name:[value] and as a list in JSON and Python, and the value cannot start at the opening bracket of the plain form.
func TestAssignmentTakesACredentialHeaderPrintedAsAList(t *testing.T) {
	a := valueFixture(9271, 22, alnum)
	x := "X-Api-" + "Key"
	for name, text := range map[string]string{
		"go print":          "map[" + x + ":[" + a + "]]",
		"go print, others":  "map[Content-Type:[a] X-Auth-" + "Token:[" + a + "]]",
		"security token":    "map[X-Amz-Security-" + "Token:[" + a + "] Host:[x.example]]",
		"api key, no x":     "map[Api-" + "Key:[" + a + "]]",
		"json":              `{"` + x + `":["` + a + `"]}`,
		"python":            `{'` + x + `': ['` + a + `']}`,
		"flow list, spaced": x + ": [ " + a + " ]",
	} {
		got := Matching(text)
		found := false
		for _, c := range got {
			found = found || c == "api-key-assignment"
		}
		if !found {
			t.Errorf("%s: classes %v on %q, want api-key-assignment", name, got, text)
		}
	}
	// The plain form keeps ending where it did, and a list of names stays quiet.
	for name, text := range map[string]string{
		"header names":  "map[Vary:[" + x + ", X-Auth-" + "Token]]",
		"short value":   "map[" + x + ":[abc]]",
		"placeholder":   "map[" + x + ":[[redacted-bearer-token-1]]]",
		"slice literal": x + `: []string{"abc"}`,
	} {
		for _, c := range Matching(text) {
			if c == "api-key-assignment" {
				t.Errorf("%s: api-key-assignment matches %q", name, text)
			}
		}
	}
}

// TestAssignmentValueMayStartWithAnAmpersand: a leading ampersand is part of an unquoted value of the assignment class.
func TestAssignmentValueMayStartWithAnAmpersand(t *testing.T) {
	a := valueFixture(9281, 22, alnum)
	kw := "DB_" + strings.ToUpper(kwPass)
	for name, text := range map[string]string{
		"env line":     kw + "=&" + a,
		"in args":      `{"cmd":"export API_` + `KEY=&` + a + `"}`,
		"with a space": kw + "= &" + a,
	} {
		found := false
		for _, c := range Matching(text) {
			found = found || c == "api-key-assignment"
		}
		if !found {
			t.Errorf("%s: api-key-assignment does not match %q", name, text)
		}
	}
	if v := AssignmentValue(kw + "=&" + a); v != "&"+a {
		t.Errorf("AssignmentValue = %q, want the value with its ampersand", v)
	}
}

// TestBasicAuthTakesALineBreakAndAListPosition: a line break and indentation, a YAML dash, and a value behind another
// element of the same list or a placeholder left by an earlier class.
func TestBasicAuthTakesALineBreakAndAListPosition(t *testing.T) {
	b := valueFixture(9291, 36, alnum)
	for name, text := range map[string]string{
		"indented json":         "{\n  \"Authorization\": [\n    \"Basic " + b + "\"\n  ]\n}",
		"tab indented":          "{\n\t\"Authorization\": [\n\t\t\"Basic " + b + "\"\n\t]\n}",
		"yaml list":             "Authorization:\n  - Basic " + b + "\n",
		"yaml list, flush":      "Authorization:\n- Basic " + b + "\n",
		"break after colon":     "{\"Authorization\":\n \"Basic " + b + "\"}",
		"behind a placeholder":  `{"Authorization":["[redacted-bearer-token-1]","Basic ` + b + `"]}`,
		"go print, placeholder": "map[Authorization:[[redacted-bearer-token-1] Basic " + b + "]]",
	} {
		found := false
		for _, c := range Matching(text) {
			found = found || c == "basic-auth"
		}
		if !found {
			t.Errorf("%s: basic-auth does not match %q", name, text)
		}
	}
	// Prose about the header stays quiet.
	for _, text := range []string{
		"the Authorization header takes the scheme and the value, for example Basic and a base64 string",
		"Authorization: Basic",
	} {
		for _, c := range Matching(text) {
			if c == "basic-auth" {
				t.Errorf("basic-auth matches %q", text)
			}
		}
	}
}
