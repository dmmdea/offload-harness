package fleetnode

// A refusal's journal line holds three things and nothing else: the door (a fixed word, clipped), the caller's address
// (the host of the connection, clipped) and one of three constant reasons. Everything else a request carries is the
// caller's own text, and a line that copied any of it would let a caller fill the journal, in bytes, at the throttle's
// rate. The tests that look for the credential in the journal cannot see that, so these compare the WHOLE line against
// the exact shape, on a request whose User-Agent, X-Forwarded-For, Referer, Host and path are all chosen by the caller.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

const refusalLinePrefix = "fleetnode: refused a "

// refusalLineRE is the whole line a refusal writes for the given door label and peer host.
func refusalLineRE(label, host string) *regexp.Regexp {
	reasons := []string{refusedNoBearer, refusedWrongBearer, refusedNoToken}
	for i, r := range reasons {
		reasons[i] = regexp.QuoteMeta(r)
	}
	return regexp.MustCompile("^" + regexp.QuoteMeta(refusalLinePrefix) + `"` + regexp.QuoteMeta(label) + `" request from "` +
		regexp.QuoteMeta(host) + `": (` + strings.Join(reasons, "|") + ")$")
}

// callerChosen is text a caller controls on every request, none of which may reach the journal.
var callerChosen = map[string]string{
	"User-Agent":      "agent-text-" + strings.Repeat("u", 300),
	"X-Forwarded-For": "203.0.113.99, 198.51.100.98",
	"Referer":         "http://referer-text.example.test/" + strings.Repeat("r", 300),
	"Accept-Language": "xx-language-text",
	"X-Request-Text":  "request-text-" + strings.Repeat("q", 300),
}

// refusalLinesOf sends one request carrying callerChosen text, with the given peer and path, and
// returns the journal lines that begin a refusal.
func refusalLinesOf(t *testing.T, s *Server, door refusalDoor, path, remote string, hdr map[string]string) []string {
	t.Helper()
	if door.prep != nil {
		door.prep(t, s)
	}
	buf := captureLog(t)
	req := httptest.NewRequest(door.method, path, strings.NewReader(door.body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	req.Host = "host-text." + strings.Repeat("h", 100) + ".example.test"
	for k, v := range callerChosen {
		req.Header.Set(k, v)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 401 or 403 (body %s)", rec.Code, rec.Body.String())
	}
	var lines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, refusalLinePrefix) {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestEveryRefusalLineIsTheDoorThePeerAndOneConstantReasonAndNothingElse(t *testing.T) {
	const peer = "192.0.2.77"
	type cell struct {
		name string
		key  string
		hdr  map[string]string
	}
	cells := []cell{
		{"no bearer", holderKey, nil},
		{"wrong bearer", holderKey, map[string]string{"Authorization": "Bearer " + wrongKey}},
		{"no token configured", "", nil},
	}
	for _, door := range refusalDoors {
		for _, c := range cells {
			if c.key == "" && door.noKeyStatus == 0 {
				continue // the door is closed on a node with no token: it answers 403 without a credential refusal
			}
			t.Run(door.name+" with "+c.name, func(t *testing.T) {
				s := refusalServer(t, c.key)
				lines := refusalLinesOf(t, s, door, door.path, peer+":4321", c.hdr)
				if len(lines) != 1 {
					t.Fatalf("wrote %d refusal line(s), want exactly 1: %q", len(lines), lines)
				}
				if !refusalLineRE(door.label, peer).MatchString(lines[0]) {
					t.Fatalf("the line is not exactly the door, the peer and one of the three reasons:\n got  %q\n want %s", lines[0], refusalLineRE(door.label, peer))
				}
			})
		}
	}
}

// The one door that routes a path the caller chooses is the artifact read, so a very long name is sent there; and a peer
// address long enough to need clipping is written clipped. The door label is the fixed word either way.
func TestARefusalLineClipsALongPeerAndNeverCopiesALongPath(t *testing.T) {
	media := refusalDoor{name: "media file", label: "media-file", method: http.MethodGet}
	long := "/fleet/media/" + strings.Repeat("n", 4000)
	s := refusalServer(t, holderKey)
	lines := refusalLinesOf(t, s, media, long, "192.0.2.77:4321", nil)
	if len(lines) != 1 || !refusalLineRE("media-file", "192.0.2.77").MatchString(lines[0]) {
		t.Fatalf("a long path: lines = %q, want exactly one in the exact shape", lines)
	}
	if len(lines[0]) > 200 {
		t.Fatalf("the line for a 4000-byte path is %d bytes long: %q", len(lines[0]), lines[0])
	}

	longPeer := strings.Repeat("p", 200)
	s = refusalServer(t, holderKey)
	lines = refusalLinesOf(t, s, media, "/fleet/media/out.png", longPeer, nil)
	want := strings.Repeat("p", 64) + "..."
	if len(lines) != 1 || !refusalLineRE("media-file", want).MatchString(lines[0]) {
		t.Fatalf("a long peer address: lines = %q, want the host clipped to %q", lines, want)
	}
}
