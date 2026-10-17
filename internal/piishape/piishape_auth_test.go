package piishape

import "testing"

// authSamples are credentials that sit in an HTTP header or a registry config, not behind a key name
// the assignment class knows. Built from pieces so no literal in the tree is credential-shaped.
func authSamples() []string {
	b64 := valueFixture(101, 22, alnum) + "=="
	sid := valueFixture(102, 32, alnum)
	return []string{
		"Authorization: Ba" + "sic " + b64,
		`curl -H "Authorization: Ba` + `sic ` + b64 + `" https://host.invalid/x`,
		"Proxy-Authorization: Ba" + "sic " + b64,
		`{"auths":{"r.invalid":{"au` + `th":"` + b64 + `"}}}`,
		`{\"auths\":{\"r.invalid\":{\"au` + `th\":\"` + b64 + `\"}}}`,
		"Coo" + "kie: session=" + sid,
		"Set-Coo" + "kie: sessionid=" + sid + "; HttpOnly",
		"Coo" + "kie: theme=dark; session=" + sid,
	}
}

// TestHeaderAndRegistryCredentialsAreRecognised: a Basic authorization header, a registry config's
// auth value and a cookie header with a session value reach the corpus and the seat otherwise.
func TestHeaderAndRegistryCredentialsAreRecognised(t *testing.T) {
	for _, text := range authSamples() {
		if len(Matching(text)) == 0 {
			t.Errorf("not recognised: %q", text)
		}
	}
}

// TestProseAroundTheHeaderNamesStaysQuiet: the words alone, without a credential value, are not
// matches.
func TestProseAroundTheHeaderNamesStaysQuiet(t *testing.T) {
	for _, text := range []string{
		"Basic authentication is described in the manual of the proxy",
		"Authorization: Basic",
		"send the Authorization header with the request",
		`{"auth":"none"}`,
		`{"auth": "oauth"}`,
		"--auth basic --user someone",
		"Set-Cookie headers are sent by the server",
		"Cookie: theme=dark",
		"cookie=abc",
	} {
		if got := Matching(text); got != nil {
			t.Errorf("Matching(%q) = %v, want nil", text, got)
		}
	}
}
