package piishape

import (
	"strings"
	"testing"
)

// headerFormSamples are a Basic credential in the places Go, JavaScript, Java, Ruby and PHP code and a
// printed http.Header put an Authorization header: a list, a call with the name and the value as two
// arguments, a subscript, a hash. Built from pieces at run time so no literal in the tree is
// credential-shaped. The credential is the last thing in each text, before its closers.
func headerFormSamples(b64 string) []struct{ name, text string } {
	basic := "Ba" + "sic " + b64
	return []struct{ name, text string }{
		{"go header dump", "Header:map[Authorization:[" + basic + "]]"},
		{"go request dump", "&{GET /x HTTP/1.1 1 1 map[Authorization:[" + basic + "] User-Agent:[go]] <nil>}"},
		{"bracketed value", "Authorization: [" + basic + "]"},
		{"bracketed equals", "Authorization=[" + basic + "]"},
		{"js setHeader", "req.setHeader('Authorization', '" + basic + "')"},
		{"xhr", `xhr.setRequestHeader("Authorization", "` + basic + `")`},
		{"go Header.Set", `req.Header.Set("Authorization", "` + basic + `")`},
		{"java property", `con.setRequestProperty("Authorization", "` + basic + `")`},
		{"subscript", `headers["Authorization"] = "` + basic + `"`},
		{"hash rocket", `'Authorization' => '` + basic + `'`},
		{"go literal", `http.Header{"Authorization": []string{"` + basic + `"}}`},
		{"php array", "[Authorization] => " + basic},
	}
}

// TestBasicHeaderInCodeAndDumpsIsRecognised: the class took only a colon or an equals sign straight after
// the header name, so a bracket, a comma or a closing quote and paren in between hid the credential.
// The credential is matched whole, and the closers behind it are not part of the match.
func TestBasicHeaderInCodeAndDumpsIsRecognised(t *testing.T) {
	b64 := valueFixture(401, 22, alnum) + "=="
	for _, s := range headerFormSamples(b64) {
		var got string
		for _, cl := range Classes {
			if cl.Name == "basic-auth" {
				got = cl.Re.FindString(s.text)
			}
		}
		if got == "" {
			t.Errorf("%s: basic-auth does not recognise %q", s.name, s.text)
			continue
		}
		if !strings.HasSuffix(got, b64) {
			t.Errorf("%s: matched %q, want it to end with the value %q", s.name, got, b64)
		}
	}
}

// TestProseAboutTheBasicSchemeStaysQuiet: the words near each other, with no separator between the
// header name and the scheme, are not a header.
func TestProseAboutTheBasicSchemeStaysQuiet(t *testing.T) {
	for _, text := range []string{
		"the Authorization header uses Basic authentication with a username and password",
		"Authorization Basic authentication is described in the manual",
		"Authorization: Basic",
		"req.Header.Set(\"Authorization\", token)",
		"[Authorization] => Bearer",
	} {
		for _, c := range Matching(text) {
			if c == "basic-auth" {
				t.Errorf("basic-auth matches prose %q", text)
			}
		}
	}
}
