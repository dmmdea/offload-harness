package netguard

import (
	"strings"
	"testing"
)

// secretMarks are the substrings no message about a refused value may carry, however the
// credential was spelled: the plain password and token, their %-escaped spellings, and the fragment.
var secretMarks = []string{"s3cret", "tok%33", "p%77", "frag-"}

// credentialedBases are values a roster entry or endpoint can be pasted as, each carrying a
// credential somewhere a URL can hold one, and each REFUSED by the tailnet guard (by name, scheme,
// address, shape or because it does not parse). The guard quotes what it refuses; none of these
// may come back out of it with the credential.
func credentialedBases() []string {
	return []string{
		"http://user:pw-s3cret@node-x." + strangerZone + ":18811/?token=tok-s3cret",       // refused name, userinfo + query
		"http://node-x." + strangerZone + ":18811/?token=tok%33-s3cret#frag-s3cret",       // query with an escape, fragment
		"http://user:pw-s3cret@example.com:18811",                                         // public name
		"http://example.com:18811/?token=s%33cret",                                        // public name, escaped query
		"https://us%65r:p%77-s3cret@example.com/path?k=v",                                 // escaped userinfo
		"http://user:pw-s3cret@192.0.2.1:18811/?token=s3cret",                             // IP literal off the tailnet
		"ftp://user:pw-s3cret@node:21/?token=s3cret",                                      // scheme refusal
		"user:pw-s3cret@node-a:18811",                                                     // no "//": reads as scheme "user"
		"s3cret-tok:pw@node-a:18811",                                                      // the "scheme" is the secret
		"tok-s3cret@node-a:18811",                                                         // does not parse
		"http://user:pw-s3cret@node-a:notaport/?token=s3cret",                             // does not parse (bad port)
		"http://user:pw-s3cret@[::1/?token=s3cret",                                        // does not parse (bad host)
		"http://user:pw-s3cret@:18811/?token=s3cret",                                      // no host
		"http://node-x." + strangerZone + ":18811/?token=s3cret%zz-frag-s3cret",           // bad escape in the query
		"  http://user:pw-s3cret@example.com:18811/?token=tok%33-s3cret  ",                // surrounding whitespace
		"HTTP://USER:PW-s3cret@EXAMPLE.COM:18811/?TOKEN=s3cret",                           // upper case
		"http://node-x." + strangerZone + ":18811/p?a=1&token=s3cret&b=%33#frag-s3cret/x", // path, many params
	}
}

// The guard names what it refuses, and a roster entry is a base URL that may have been pasted with a
// token in its query or user:password in front of its host. Its message (printed by doctor, by the
// lanes' "probed ..." lines and by the config loader, all of which get shared) carries only the
// redacted form, whichever check refused the value.
func TestTailnetURLInNeverQuotesACredential(t *testing.T) {
	// The hostname refusal words itself three ways (no zone, one zone, several), and each quotes the value.
	for _, zones := range [][]string{nil, {ownZone}, {ownZone, sharerZone}} {
		for _, raw := range credentialedBases() {
			err := TailnetURLIn(zones, raw)
			if err == nil {
				t.Errorf("precondition: %q must be refused (zones %q)", raw, zones)
				continue
			}
			for _, mark := range secretMarks {
				if strings.Contains(err.Error(), mark) {
					t.Errorf("the refusal of %q (zones %q) carries %q:\n%s", raw, zones, mark, err)
				}
			}
			// An escape error quotes three bytes of the value; the message says so in words instead.
			if strings.Contains(err.Error(), "%zz") {
				t.Errorf("the refusal of %q (zones %q) quotes the bytes of a bad escape:\n%s", raw, zones, err)
			}
		}
	}
}

// Redaction must not make the refusal useless: it still names the host that was refused and says why.
func TestTailnetURLInKeepsTheHostAndTheCauseWhenItRedacts(t *testing.T) {
	err := TailnetURLIn([]string{ownZone}, "http://user:pw-s3cret@node-x."+strangerZone+":18811/?token=tok-s3cret")
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"node-x." + strangerZone, ":18811", "not allowed", "under " + ownZone} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the redacted refusal must still say %q:\n%s", want, err)
		}
	}
	// A value with nothing to hide is quoted exactly as before.
	plain := "http://node-x." + strangerZone + ":18811"
	if err := TailnetURLIn([]string{ownZone}, plain); err == nil || !strings.Contains(err.Error(), `"`+plain+`"`) {
		t.Errorf("an ordinary refused value must be quoted unchanged, got %v", err)
	}
}

// RedactBase on every spelling the guard test uses: no mark survives, and a value with nothing secret is
// byte-identical (so no existing message changes).
func TestRedactBaseDropsEverySpellingOfACredential(t *testing.T) {
	for _, raw := range credentialedBases() {
		got := RedactBase(raw)
		for _, mark := range secretMarks {
			if strings.Contains(got, mark) {
				t.Errorf("RedactBase(%q) = %q still carries %q", raw, got, mark)
			}
		}
	}
	for _, plain := range []string{"http://node-a:18811", "https://node-a.example:443/v1", "${NODE_A_HOST}:18811", "node-a:18811", ""} {
		if got := RedactBase(plain); got != plain {
			t.Errorf("RedactBase(%q) = %q, want it unchanged", plain, got)
		}
	}
	// Specific shapes: what survives is the part an operator needs to find the line.
	for in, want := range map[string]string{
		"user:pw@node-a:18811":                    "<redacted>@node-a:18811",
		"tok@node-a:18811":                        "<redacted>@node-a:18811",
		"user:pw@node-a:18811?x=1":                "<redacted>@node-a:18811<redacted>",
		"http://user:pw@node-a:18811/v1?x=1#f":    "http://node-a:18811/v1",
		"http://user:pw@node-a:bad/?token=abc":    "http://<redacted>@node-a:bad/<redacted>",
		"http://node-a:18811/path?token=s%33cret": "http://node-a:18811/path",
	} {
		if got := RedactBase(in); got != want {
			t.Errorf("RedactBase(%q) = %q, want %q", in, got, want)
		}
	}
}
