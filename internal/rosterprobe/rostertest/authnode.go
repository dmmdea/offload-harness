package rostertest

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Seen is one request an AuthNode received: what the client asked for and the Authorization header it
// carried (empty when it sent none).
type Seen struct {
	Method string
	Path   string
	Auth   string
}

// AuthNode is a fleet node on loopback that records the method, path and Authorization header of every
// request, and answers each with whatever the reply function says. It is how a client test proves that
// EVERY request the client builds carries the fleet bearer: the recorder keeps them all, in order, and
// the test asserts over the whole list, not over the one request it expected.
type AuthNode struct {
	// URL is the base of the node ("http://127.0.0.1:<port>").
	URL  string
	mu   sync.Mutex
	seen []Seen
}

// NewAuthNode starts the node; reply gives the status and body of each answer (nil: 200 with "{}").
// It closes when the test ends.
func NewAuthNode(t testing.TB, reply func(r *http.Request) (status int, body string)) *AuthNode {
	t.Helper()
	n := &AuthNode{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.seen = append(n.seen, Seen{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")})
		n.mu.Unlock()
		status, body := http.StatusOK, "{}"
		if reply != nil {
			status, body = reply(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	n.URL = srv.URL
	return n
}

// Seen returns every request received so far, oldest first.
func (n *AuthNode) Seen() []Seen {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Seen(nil), n.seen...)
}

// Reset forgets what was recorded.
func (n *AuthNode) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seen = nil
}

// WantBearer fails the test unless the node received at least one request and every request carried
// exactly "Authorization: Bearer <token>".
func (n *AuthNode) WantBearer(t testing.TB, token string) {
	t.Helper()
	seen := n.Seen()
	if len(seen) == 0 {
		t.Fatalf("the client sent no request at all, so nothing was checked")
	}
	for _, s := range seen {
		if want := "Bearer " + token; s.Auth != want {
			t.Errorf("%s %s carried Authorization %q, want %q", s.Method, s.Path, s.Auth, want)
		}
	}
}
