package fleetnode

// /fleet/health and the jobs feed answer a caller that presents a bearer which is not the node's EXACTLY as they answer one
// that presents none: the same status, the same response headers, the same body. A difference of any kind (a challenge
// header, a verdict header, a field left out of the view) would tell the caller its credential was looked at and refused,
// which is what the doors that need the bearer say with a 401, and would give a delegator holding a stale key a node that
// reads as serving nothing. The only thing a presented credential changes on these routes is the journal's one throttled
// line (refusal_public_routes_test.go).

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// presentedCredentials are the Authorization values a caller can present that the node does not hold: a wrong bearer of the
// node's key length and of another, the scheme in another case, a scheme that is not Bearer, a scheme with no value, and a
// value of 64 KiB.
var presentedCredentials = map[string]string{
	"wrong bearer":                "Bearer " + wrongKey,
	"longer wrong bearer":         "Bearer " + wrongKey + "-and-then-some",
	"lower-case scheme":           "bearer " + wrongKey,
	"Basic":                       "Basic c29tZW9uZTplbHNl",
	"bare Bearer and a space":     "Bearer ",
	"bare Bearer with no space":   "Bearer",
	"a 64 KiB bearer":             "Bearer " + strings.Repeat("x", 64<<10),
	"a 64 KiB value, no scheme":   strings.Repeat("y", 64<<10),
	"the right key, other scheme": "Token " + holderKey,
}

// answerOf is what a caller can compare of a response: its status, its headers without the two a server sets per response,
// and its body.
func answerOf(rec *httptest.ResponseRecorder) (int, http.Header, []byte) {
	h := rec.Header().Clone()
	h.Del("Date")
	h.Del("Content-Length")
	return rec.Code, h, rec.Body.Bytes()
}

func clip(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}

func TestAPresentedCredentialThatIsNotTheNodesGetsTheAnswerOfNoCredential(t *testing.T) {
	for _, rt := range publicRoutes {
		for name, value := range presentedCredentials {
			t.Run(rt.name+" with "+name, func(t *testing.T) {
				s := publicRouteServer(t)
				buf := captureLog(t)
				const peer = "192.0.2.50:5000"

				none := getFrom(s, rt.path, peer, nil)
				rt.masked(t, none) // the view to compare with is the one a caller without the bearer gets
				if lines := journalRefusals(buf.String()); len(lines) != 0 {
					t.Fatalf("a request with no credential wrote %d refusal line(s), want none: %q", len(lines), lines)
				}
				wantCode, wantHeader, wantBody := answerOf(none)

				got := getFrom(s, rt.path, peer, map[string]string{"Authorization": value})
				code, header, body := answerOf(got)
				if code != wantCode {
					t.Errorf("status = %d, a request with no credential got %d", code, wantCode)
				}
				if !reflect.DeepEqual(header, wantHeader) {
					t.Errorf("response headers = %v, a request with no credential got %v", header, wantHeader)
				}
				if !bytes.Equal(body, wantBody) {
					t.Errorf("body = %s, a request with no credential got %s", clip(body), clip(wantBody))
				}

				// What it does change is the journal: one throttled line in the usual shape, whatever the value.
				lines := journalRefusals(buf.String())
				if len(lines) != 1 {
					t.Fatalf("a presented credential wrote %d refusal line(s), want 1; log = %.400q", len(lines), buf.String())
				}
				if !refusalLineRE(rt.label, "192.0.2.50").MatchString(lines[0]) || !strings.Contains(lines[0], refusedWrongBearer) {
					t.Errorf("the line is not the usual one for door %q and a wrong bearer: %.400q", rt.label, lines[0])
				}
			})
		}
	}
}
