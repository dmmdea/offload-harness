package fleetnode

// A refusal's journal line names who asked, which door and why, and never what was presented: the value in the
// Authorization header of a refused request is a credential that somebody holds, and a journal is read by more people
// than the key is. The tests that look for the configured key in the journal send no Authorization header at all, so they
// cannot see the presented value. These send a distinctive wrong bearer, and two malformed headers, to every door that
// refuses a credential, and look for the presented value and for the configured key in the journal and in the answer.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// presentedValue is the secret-looking text a refused caller sends. It is built at run time and is unlike the node's key,
// so seeing it in the journal can only mean the journal copied the header.
func presentedValue() string {
	return strings.Join([]string{"zq7x2", "m4k9", "presented", "value"}, "-")
}

// presentedHeaders are the Authorization headers a refused caller can send: a bearer that is not the node's, a scheme the
// node does not use, and a bearer scheme with no separator.
func presentedHeaders() map[string]string {
	v := presentedValue()
	return map[string]string{
		"wrong bearer":                         "Bearer " + v,
		"malformed, another scheme":            "Basic " + v,
		"malformed, no space after the scheme": "Bearer" + v,
	}
}

func assertCredentialNotCopied(t *testing.T, logged, answer string) {
	t.Helper()
	for what, text := range map[string]string{"journal": logged, "answer": answer} {
		if strings.Contains(text, presentedValue()) {
			t.Fatalf("the %s carries the credential the caller presented: %q", what, text)
		}
		if strings.Contains(text, "zq7x2") || strings.Contains(text, "m4k9") {
			t.Fatalf("the %s carries part of the credential the caller presented: %q", what, text)
		}
		if strings.Contains(text, holderKey) {
			t.Fatalf("the %s carries the node's own key: %q", what, text)
		}
	}
}

func TestNoDoorWritesThePresentedCredentialInItsJournalLine(t *testing.T) {
	for _, door := range refusalDoors {
		for name, header := range presentedHeaders() {
			t.Run(door.name+" with "+name, func(t *testing.T) {
				s := refusalServer(t, holderKey)
				if door.prep != nil {
					door.prep(t, s)
				}
				buf := captureLog(t)
				rec := do(t, s, door.method, door.path, door.body, map[string]string{"Authorization": header})
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
				}
				if n := refusalLines(buf.String()); n == 0 {
					t.Fatalf("the door refused and wrote no line, so there is nothing to check; log = %q", buf.String())
				}
				assertCredentialNotCopied(t, buf.String(), rec.Body.String())
			})
		}
	}
}

// The same on a node that has no token and listens beyond loopback: the refusal is for want of a key on the node, and the
// caller's header is still not copied.
func TestNoDoorWritesThePresentedCredentialOnANodeWithNoTokenBeyondLoopback(t *testing.T) {
	for _, door := range refusalDoors {
		if door.noKeyStatus == 0 {
			continue
		}
		for name, header := range presentedHeaders() {
			t.Run(door.name+" with "+name, func(t *testing.T) {
				s := refusalServer(t, "")
				buf := captureLog(t)
				rec := do(t, s, door.method, door.path, door.body, map[string]string{"Authorization": header})
				if rec.Code != door.noKeyStatus {
					t.Fatalf("status = %d, want %d (body %s)", rec.Code, door.noKeyStatus, rec.Body.String())
				}
				if n := refusalLines(buf.String()); n == 0 {
					t.Fatalf("the door refused and wrote no line, so there is nothing to check; log = %q", buf.String())
				}
				assertCredentialNotCopied(t, buf.String(), rec.Body.String())
			})
		}
	}
}

// The re-dispatch of a job the node already holds is the one door the table cannot reach over HTTP (every route that gets
// there has passed the bearer), so it is called directly with the header set.
func TestARefusedReDispatchOfAKnownJobDoesNotWriteThePresentedCredential(t *testing.T) {
	for name, header := range presentedHeaders() {
		t.Run(name, func(t *testing.T) {
			s := refusalServer(t, holderKey)
			buf := captureLog(t)
			req := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", nil)
			req.Header.Set("Authorization", header)
			rec := httptest.NewRecorder()
			s.answerKnownJob(rec, req, &JobView{ID: "known", Gated: true})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
			if n := refusalLines(buf.String()); n != 1 {
				t.Fatalf("wrote %d refusal line(s), want exactly 1; log = %q", n, buf.String())
			}
			assertCredentialNotCopied(t, buf.String(), rec.Body.String())
		})
	}
}
