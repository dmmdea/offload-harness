package piishape

import (
	"strings"
	"testing"
)

// The class table finds a cookie or a credential header by the shape of the text around it, and every shape it
// does not know (a YAML list item, a backtick string, a HAR name/value pair, a bytes literal with some other prefix) leaves
// the value in the text with nothing to compare it to. The check below does not read shapes: a text that still holds
// one of the credential header keywords after redaction, with a long value run a few characters behind it, is not
// safe to keep, whatever stands between them.

// runValue is a value run of n characters, built at run time.
func runValue(seed, n int) string { return valueFixture(seed, n, alnum) }

// TestKeywordRunFiresAtAnyBoundary: the keyword is found inside a name (an underscore in front of it), after a bytes
// prefix, a backtick, a quote or a bracket, in any case, and in each spelling the headers use.
func TestKeywordRunFiresAtAnyBoundary(t *testing.T) {
	v := runValue(9101, 22)
	for name, text := range map[string]string{
		"plain header line":         ckName + ": sid=" + v,
		"set header line":           sckName + ": sid=" + v + "; Path=/",
		"underscore ahead":          "HTTP_" + strings.ToUpper(ckName) + "=sessionid=" + v,
		"lower case":                strings.ToLower(ckName) + "=" + v,
		"mixed case":                "cOoKiE: " + v,
		"bytes prefix, single":      "[(b'" + strings.ToLower(ckName) + "', b'sid=" + v + "')]",
		"bytes prefix, double":      `{b"` + ckName + `": b"sid=` + v + `"}`,
		"backtick value":            `req.Header.Set("` + ckName + `", ` + "`sid=" + v + "`)",
		"backtick name":             "`" + ckName + "`: " + v,
		"single quotes":             "'" + ckName + "': '" + v + "'",
		"brackets":                  "[" + ckName + "]=" + v,
		"parens":                    "(" + ckName + "): " + v,
		"authorization":             "Authorization: Digest " + v,
		"authorization, any case":   "AUTHORIZATION=" + v,
		"api key, hyphen":           "x-api-" + "key: " + v,
		"api key, underscore":       "API_" + "KEY=" + v,
		"api key, joined":           "api" + "key=" + v,
		"auth token, hyphen":        "X-Auth-" + "Token: " + v,
		"auth token, underscore":    "auth_" + "token=" + v,
		"auth token, joined":        "auth" + "token=" + v,
		"security token":            "X-Amz-Security-" + "Token: " + v,
		"security token, joined":    "security" + "token=" + v,
		"yaml list item":            sckName + ":\n      - '" + v + "'\n",
		"yaml list item, no indent": "Authorization:\n- " + v + "\n",
		"har name and value":        `{"name":"` + ckName + `","value":"sid=` + v + `"}`,
		"xml attributes":            `<header name="` + ckName + `" value="sid=` + v + `"/>`,
		"entity-escaped quotes":     "<" + ckName + ">sid=&quot;" + v + "&quot;</" + ckName + ">",
		"json escapes":              `{\"` + ckName + `\":\"` + v + `\"}`,
		"brackets inside the value": ckName + ":[sid=ab]" + v + "]",
		"list, long element":        "map[" + ckName + ":[" + v + "]]",
	} {
		if !KeywordRun(text) {
			t.Errorf("%s: the check does not fire on %q", name, text)
		}
	}
}

// TestKeywordRunNeedsALongRunNearTheKeyword: 12 characters with no white space, a quote, a comma, a semicolon or a
// backslash, starting at most KeywordRunReach characters behind the keyword.
func TestKeywordRunNeedsALongRunNearTheKeyword(t *testing.T) {
	v12, v11 := runValue(9111, 12), runValue(9112, 11)
	if !KeywordRun(ckName + ": " + v12) {
		t.Errorf("a run of 12 does not fire")
	}
	for name, text := range map[string]string{
		"a run of 11":                    ckName + ": " + v11,
		"two runs of 6 with a space":     ckName + ": " + v12[:6] + " " + v12[6:],
		"a run cut by a comma":           ckName + ": " + v12[:6] + "," + v12[6:],
		"a run cut by a semicolon":       ckName + ": " + v12[:6] + ";" + v12[6:],
		"a run cut by a quote":           ckName + ": " + v12[:6] + `"` + v12[6:],
		"a run cut by an apostrophe":     ckName + ": " + v12[:6] + "'" + v12[6:],
		"a run cut by a backslash":       ckName + ": " + v12[:6] + `\` + v12[6:],
		"the keyword alone":              ckName,
		"a short cookie line":            ckName + ": theme=dark; lang=en",
		"the long run stands in front":   v12 + " " + ckName,
		"no keyword":                     "token_count: " + v12,
		"a header that names no keyword": "Host: " + v12,
	} {
		if KeywordRun(text) {
			t.Errorf("%s: the check fires on %q", name, text)
		}
	}
}

// TestKeywordRunReachIsExact: the run may start KeywordRunReach characters after the keyword and no later.
func TestKeywordRunReachIsExact(t *testing.T) {
	v := runValue(9121, 20)
	for _, kw := range []string{ckName, "Authorization", "X-Api-" + "Key"} {
		at := kw + strings.Repeat(" ", KeywordRunReach) + v
		past := kw + strings.Repeat(" ", KeywordRunReach+1) + v
		if !KeywordRun(at) {
			t.Errorf("%s: a run that starts %d characters behind the keyword does not fire", kw, KeywordRunReach)
		}
		if KeywordRun(past) {
			t.Errorf("%s: a run that starts %d characters behind the keyword fires", kw, KeywordRunReach+1)
		}
	}
}

// TestKeywordRunStopsAtARedactionMarker: the placeholders a redactor writes carry the keyword in their names and are
// themselves a long run, and neither counts. A marker also ends the reach of a keyword in front of it: it stands in the
// value's place, and what follows it belongs to the next field (a JSON object of headers, the line after a header).
func TestKeywordRunStopsAtARedactionMarker(t *testing.T) {
	v := runValue(9131, 22)
	for name, text := range map[string]string{
		"cookie marker":            "map[[redacted-cookie-header-1]]",
		"marker after a header":    ckName + ": [redacted-cookie-header-1]",
		"api key marker":           "X-Api-" + "Key: [redacted-api-key-assignment-1]",
		"basic marker":             "Authorization: [redacted-basic-auth-1]",
		"bearer marker, next line": "Authorization: [redacted-bearer-token-2]\nAccept: application/json",
		"json key kept":            `{"` + ckName + `":"[redacted-cookie-header-1]"}`,
		"json, next field":         `{"Authorization":"[redacted-bearer-token-1]","Accept":"application/json"}`,
		"json, next field, Args":   `{\"` + ckName + `\":\"[redacted-cookie-header-1]\",\"Accept\":\"application/json\"}`,
		"marker, short stuff":      ckName + ": [redacted-cookie-header-1] theme=dark",
		"marker, then a value":     ckName + ": [redacted-cookie-header-1] " + v,
		"marker alone, then value": "[redacted-cookie-header-1] " + v,
	} {
		if KeywordRun(text) {
			t.Errorf("%s: the check fires on %q", name, text)
		}
	}
	// A value in front of the marker is in reach.
	if !KeywordRun(ckName + ": " + v + " [redacted-cookie-header-1]") {
		t.Errorf("a value in front of a marker does not fire")
	}
}

// TestKeywordRunCrossesALineBreakOnlyWhereNothingStandsBehindTheKeyword: a YAML list and a pretty-printed JSON document put the
// value on the next line, with only punctuation between; a header line with a value of its own is followed by the next
// header, which is not its value.
func TestKeywordRunCrossesALineBreakOnlyWhereNothingStandsBehindTheKeyword(t *testing.T) {
	v := runValue(9141, 22)
	for name, text := range map[string]string{
		"yaml list":           sckName + ":\n  - " + v + "\n",
		"yaml, crlf":          sckName + ":\r\n  - " + v,
		"pretty json":         "{\n  \"Authorization\": [\n    \"" + v + "\"\n  ]\n}",
		"har, pretty":         "{\n  \"name\": \"" + ckName + "\",\n  \"value\": \"sid=" + v + "\"\n}",
		"blank lines":         ckName + ":\n\n   \n" + v,
		"escaped breaks":      `{\"cmd\":\"` + sckName + `:\n  - ` + v + `\"}`,
		"yaml name and value": "- name: " + ckName + "\n  value: sid=" + v,
		"keyword ends a line": "set the " + ckName + "\n" + v,
	} {
		if !KeywordRun(text) {
			t.Errorf("%s: the check does not fire on %q", name, text)
		}
	}
	for name, text := range map[string]string{
		"short cookie, next header":    ckName + ": theme=dark\nHost: api.example.invalid/v1/users",
		"short cookie, crlf":           ckName + ": theme=dark\r\nContent-Type: application/json",
		"short cookie, escaped breaks": `{\"cmd\":\"` + ckName + `: theme=dark\nContent-Type: application/json\"}`,
		"escaped, deeper":              `\\\"` + ckName + `: theme=dark\\nContent-Type: application/json`,
		"authorization, next header":   "Authorization: Digest\nContent-Type: application/json",
	} {
		if KeywordRun(text) {
			t.Errorf("%s: the check fires on %q", name, text)
		}
	}
}
