package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rosterJSON mirrors the shape llama-swap actually serves on /v1/models: the
// CANONICAL model id in data[].id, with the harness-bound ALIASES (the names
// tier seeds put in agent_model) only inside meta.llamaswap.aliases. The alias
// entry is the load-bearing part: an id-only matcher declares a correctly
// served seat missing and fails the run loud for no reason.
const rosterJSON = `{"object":"list","data":[
	{"id":"canonical-26b","meta":{"llamaswap":{"aliases":["gemma4-26b-a4b","m26"]}}},
	{"id":"offload-e4b"}
]}`

// rosterServer serves the fixed roster and records the request path so the
// URL-building cases can assert exactly what was fetched.
func rosterServer(gotPath *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(rosterJSON))
	}))
}

func TestPlannerUnservedRosterMatching(t *testing.T) {
	ctx := context.Background()

	t.Run("canonical id hit", func(t *testing.T) {
		var path string
		srv := rosterServer(&path)
		defer srv.Close()
		missing, checked := plannerUnserved(ctx, srv.URL, "canonical-26b")
		if missing || !checked {
			t.Fatalf("canonical id must be served: missing=%v checked=%v", missing, checked)
		}
	})

	t.Run("alias-only hit", func(t *testing.T) {
		// gemma4-26b-a4b appears ONLY in meta.llamaswap.aliases, never as an id —
		// the exact shape a tier-seeded agent_model resolves against.
		var path string
		srv := rosterServer(&path)
		defer srv.Close()
		missing, checked := plannerUnserved(ctx, srv.URL, "gemma4-26b-a4b")
		if missing || !checked {
			t.Fatalf("alias-only model must count as served: missing=%v checked=%v", missing, checked)
		}
	})

	t.Run("genuinely absent", func(t *testing.T) {
		var path string
		srv := rosterServer(&path)
		defer srv.Close()
		missing, checked := plannerUnserved(ctx, srv.URL, "not-served-anywhere")
		if !missing || !checked {
			t.Fatalf("absent model must be missing on a checked roster: missing=%v checked=%v", missing, checked)
		}
	})

	t.Run("non-200 roster means unchecked", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		missing, checked := plannerUnserved(ctx, srv.URL, "canonical-26b")
		if missing || checked {
			t.Fatalf("a non-200 roster must not fail loud: missing=%v checked=%v", missing, checked)
		}
	})

	t.Run("unreachable server means unchecked", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		base := srv.URL
		srv.Close() // nothing listens here anymore
		missing, checked := plannerUnserved(ctx, base, "canonical-26b")
		if missing || checked {
			t.Fatalf("an unreachable roster must not fail loud: missing=%v checked=%v", missing, checked)
		}
	})

	t.Run("endpoint with trailing /v1 builds /v1/models", func(t *testing.T) {
		var path string
		srv := rosterServer(&path)
		defer srv.Close()
		if _, checked := plannerUnserved(ctx, srv.URL+"/v1", "offload-e4b"); !checked {
			t.Fatal("roster with a /v1 base must be checked")
		}
		if path != "/v1/models" {
			t.Fatalf("base ending in /v1 must fetch /v1/models, fetched %q", path)
		}
	})

	t.Run("endpoint without /v1 builds /v1/models", func(t *testing.T) {
		var path string
		srv := rosterServer(&path)
		defer srv.Close()
		if _, checked := plannerUnserved(ctx, srv.URL, "offload-e4b"); !checked {
			t.Fatal("roster with a bare base must be checked")
		}
		if path != "/v1/models" {
			t.Fatalf("bare base must fetch /v1/models, fetched %q", path)
		}
	})
}
