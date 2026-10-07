package sttremote

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// This lane sends the fleet bearer to the roster node it picked. A node (or a proxy in front of it) that
// answers 3xx must not make the client replay the request, Authorization header and body included, at a
// Location it chose: the 3xx comes back to the lane as the node's answer and the target is never contacted.
func TestHTTPClientNeverFollowsARedirectWithTheBearer(t *testing.T) {
	trap := rostertest.NewRedirectTrap(t)
	req, err := http.NewRequest(http.MethodPost, trap.URL+"/fleet/dispatch", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer the-fleet-bearer")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("a redirect is an answer, not a transport error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the node's own 307 returned unfollowed", resp.StatusCode)
	}
	if trap.Hits() != 0 {
		t.Fatalf("the redirect target was contacted %d time(s) with Authorization %q: the bearer left the node it was sent to", trap.Hits(), trap.Auths())
	}
}
