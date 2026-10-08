package rosterprobe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The token probe asks one node one question and must never carry the bearer anywhere else. A node (or a
// proxy in front of it) that answers 3xx would otherwise make the client replay the request, headers
// included, at a Location it chose. The redirect is returned to the caller as the answer: it is not
// acceptance (TokenUnknown), and the target is never contacted.
func TestCheckTokenDoesNotFollowARedirectWithTheBearer(t *testing.T) {
	var hits atomic.Int64
	var gotAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"job_id required"}`)) // what would READ as acceptance if it were followed
	}))
	t.Cleanup(target.Close)

	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/fleet/dispatch", code)
		}))
		defer redirector.Close()

		status, body, err := CheckToken(context.Background(), redirector.URL, "the-fleet-bearer")
		if err != nil {
			t.Fatalf("%d: a redirect is an answer, not a transport error: %v", code, err)
		}
		if status != code {
			t.Errorf("status = %d, want the redirect's own %d returned unfollowed", status, code)
		}
		if got := ClassifyToken(status, body); got != TokenUnknown {
			t.Errorf("%d: ClassifyToken = %v, want TokenUnknown: a redirect says nothing about the token", code, got)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the redirect target was contacted %d time(s) (Authorization %q): the bearer left the node it was sent to", n, gotAuth.Load())
	}
}
