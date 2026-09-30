package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FetchNodeView carries the node's advertised release (harness_version) for
// doctor's fleet-skew rows; a node that publishes none reads as empty.
func TestFetchNodeViewReadsTheHarnessVersion(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"node_id":"node-a","harness_version":"9.8.7"}`, "9.8.7"},
		{`{"node_id":"node-a"}`, ""},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.body))
		}))
		v, err := FetchNodeView(context.Background(), srv.URL, "")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if v.HarnessVersion != tc.want {
			t.Fatalf("body %s: HarnessVersion = %q, want %q", tc.body, v.HarnessVersion, tc.want)
		}
	}
}
