package delegate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestTheDelegatorNeverReplaysTheFleetBearerAtARedirectTarget (ADR 0074 decision 9): every request the delegator
// sends a roster node carries fleet_auth_token, so neither the health read nor the dispatch/poll/withdraw client may
// follow a 3xx. A node, or a proxy in front of one, would otherwise have the client replay the request and its
// Authorization header at a Location it chose. Both clients hand the 3xx back to their caller, which reads it as the
// failure or refusal any other non-2xx status is.
func TestTheDelegatorNeverReplaysTheFleetBearerAtARedirectTarget(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"node_id":"elsewhere"}`)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	for _, tc := range []struct {
		name   string
		client *http.Client
		method string
	}{
		{"the health client", healthClient, http.MethodGet},
		{"the dispatch, poll and withdraw client", fleetClient, http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			req, err := http.NewRequestWithContext(context.Background(), tc.method, redirector.URL+"/fleet/jobs", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer secret-token")
			resp, err := tc.client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v (want the 3xx handed back, not an error)", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusTemporaryRedirect {
				t.Errorf("status = %d, want %d handed back to the caller", resp.StatusCode, http.StatusTemporaryRedirect)
			}
			if got := hits.Load() - before; got != 0 {
				t.Errorf("the redirect target received %d request(s) carrying the bearer, want 0", got)
			}
		})
	}

	if _, err := FetchNodeView(context.Background(), redirector.URL, "secret-token"); err == nil {
		t.Error("FetchNodeView through a redirect succeeded, want the 3xx read as a failed health read")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the redirect target received %d request(s) in total, want 0", got)
	}
}
