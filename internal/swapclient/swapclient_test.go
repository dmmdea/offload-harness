package swapclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestBaseURLNormalization locks the ONE endpoint rule. The three readers this
// package replaced each had their own: one appended "/v1/models" blindly (so a
// /v1-suffixed endpoint would have fetched /v1/v1/models), one appended "/v1"
// when absent, one cut at the first "/v1" anywhere in the string.
func TestBaseURLNormalization(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:11436":     "http://127.0.0.1:11436",
		"http://127.0.0.1:11436/":    "http://127.0.0.1:11436",
		"http://127.0.0.1:11436/v1":  "http://127.0.0.1:11436",
		"http://127.0.0.1:11436/v1/": "http://127.0.0.1:11436",
		"  http://host:9000/v1  ":    "http://host:9000",
		"":                           "",
	}
	for in, want := range cases {
		if got := BaseURL(in); got != want {
			t.Errorf("BaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// rosterServer serves llama-swap's REAL /v1/models shape and records the path.
func rosterServer(t *testing.T, gotPath *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gemma-4-e4b","meta":{"llamaswap":{"aliases":["offload-e4b"]}}},
			{"id":"gemma-4-26b","meta":{"llamaswap":{"aliases":["gemma4-26b-a4b","local-general"]}}},
			{"id":"plain-seat"}
		]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRosterServesMatchesIdsAndAliases is the regression for the bug this package
// exists to end: llama-swap publishes canonical ids in data[].id and the names the
// harness binds (offload-e4b, gemma4-26b-a4b) ONLY under meta.llamaswap.aliases.
func TestRosterServesMatchesIdsAndAliases(t *testing.T) {
	var path string
	srv := rosterServer(t, &path)
	r, err := FetchRoster(context.Background(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("FetchRoster: %v", err)
	}
	if path != "/v1/models" {
		t.Fatalf("fetched %q, want /v1/models", path)
	}
	for _, name := range []string{"gemma-4-e4b", "offload-e4b", "gemma4-26b-a4b", "local-general", "plain-seat", "OFFLOAD-E4B"} {
		if !r.Serves(name) {
			t.Errorf("Serves(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "not-served-anywhere", "gemma-4"} {
		if r.Serves(name) {
			t.Errorf("Serves(%q) = true, want false", name)
		}
	}
	if got, want := r.IDs(), 3; len(got) != want {
		t.Errorf("IDs() = %v, want %d canonical ids", got, want)
	}
	if r.IDs()[0] != "gemma-4-e4b" {
		t.Errorf("IDs() must stay in roster order, got %v", r.IDs())
	}
}

// TestFetchRosterTolerantOfATrailingV1: the harness's endpoint may or may not
// carry /v1; both must reach /v1/models exactly once.
func TestFetchRosterTolerantOfATrailingV1(t *testing.T) {
	var path string
	srv := rosterServer(t, &path)
	if _, err := FetchRoster(context.Background(), srv.URL+"/v1", 5*time.Second); err != nil {
		t.Fatalf("FetchRoster with a /v1 base: %v", err)
	}
	if path != "/v1/models" {
		t.Fatalf("a /v1 base fetched %q, want /v1/models", path)
	}
}

// TestFetchRosterErrorsAreErrors: an unreachable or 5xx endpoint must surface as
// an error, never as an empty roster — "serves nothing" and "could not be read"
// drive opposite decisions in doctor and acceptance.
func TestFetchRosterErrorsAreErrors(t *testing.T) {
	t.Run("5xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		if _, err := FetchRoster(context.Background(), srv.URL, 5*time.Second); err == nil {
			t.Fatal("a 500 roster must be an error")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		base := srv.URL
		srv.Close()
		if _, err := FetchRoster(context.Background(), base, 2*time.Second); err == nil {
			t.Fatal("an unreachable roster must be an error")
		}
	})
}
