package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode body: %v (%s)", err, b)
	}
	return m
}

func TestRecallMergesUsersSortsAndDedupes(t *testing.T) {
	var mu sync.Mutex
	var gotKey string
	var queriedUsers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		u := body["filters"].(map[string]any)["user_id"].(string)
		mu.Lock() // Recall now queries namespaces CONCURRENTLY → guard shared test state
		gotKey = r.Header.Get("X-API-Key")
		queriedUsers = append(queriedUsers, u)
		mu.Unlock()
		// score returned as a STRING (as the live server does) to exercise parseScore
		switch u {
		case "local-agent":
			_, _ = io.WriteString(w, `{"results":[{"id":"a","memory":"agent past run","score":"0.9"},{"id":"dup","memory":"shared","score":"0.5"}]}`)
		case "dmmdea":
			_, _ = io.WriteString(w, `{"results":[{"id":"b","memory":"user fact","score":"0.7"},{"id":"dup","memory":"shared","score":"0.5"}]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	}))
	defer srv.Close()

	c := NewMemoryClient(srv.URL, "sek", []string{"local-agent", "dmmdea"}, "local-agent", "agent-1", 5*time.Second)
	got, err := c.Recall(context.Background(), "q", 8)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if gotKey != "sek" {
		t.Errorf("X-API-Key = %q", gotKey)
	}
	if len(queriedUsers) != 2 {
		t.Errorf("queried %v, want both readUsers", queriedUsers)
	}
	// dedup by id ("dup" appears in both) → 3 unique, sorted by score desc
	if len(got) != 3 {
		t.Fatalf("got %d recalled, want 3 (deduped): %+v", len(got), got)
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[0].Score < got[1].Score {
		t.Errorf("not score-sorted/deduped: %+v", got)
	}
	if got[0].Source != "local-agent" {
		t.Errorf("source not tagged: %+v", got[0])
	}
}

func TestPersistIsEvidenceTierUnderAgentNamespace(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"results":[{"id":"new-mem-1"}]}`)
	}))
	defer srv.Close()

	c := NewMemoryClient(srv.URL, "sek", []string{"dmmdea"}, "local-agent", "agent-1", 5*time.Second)
	// caller maliciously/accidentally tries every escape: lowercase tier, CAPITALIZED
	// Tier (case-fold bypass), and a server retrieval-gating key — all must be STRIPPED.
	id, err := c.Persist(context.Background(), "ran objective X, outcome Y", map[string]string{
		"tier": "canonical", "Tier": "canonical", "contradicts_canonical": "victim-id", "run": "r1",
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if id != "new-mem-1" {
		t.Errorf("id = %q", id)
	}
	if body["user_id"] != "local-agent" {
		t.Errorf("write user_id = %v, want local-agent (isolated)", body["user_id"])
	}
	md := body["metadata"].(map[string]any)
	if md["tier"] != "evidence" {
		t.Errorf("tier = %v, want evidence (canonical must never be sent)", md["tier"])
	}
	if _, bad := md["Tier"]; bad {
		t.Error("capitalized 'Tier' was not stripped (case-fold bypass)")
	}
	if _, bad := md["contradicts_canonical"]; bad {
		t.Error("forbidden retrieval-gating key 'contradicts_canonical' was not stripped")
	}
	if md["source"] != "local-agent" {
		t.Errorf("source = %v, want local-agent", md["source"])
	}
	if md["run"] != "r1" {
		t.Errorf("benign caller metadata dropped: %v", md)
	}
	if body["infer"] != false {
		t.Errorf("infer should be false (store verbatim), got %v", body["infer"])
	}
}

func TestRecallDefersNotCrashesOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()
	c := NewMemoryClient(srv.URL, "k", []string{"dmmdea"}, "local-agent", "a", 5*time.Second)
	got, err := c.Recall(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("expected error surfaced on total failure")
	}
	if len(got) != 0 {
		t.Errorf("expected empty recall on failure, got %d", len(got))
	}
}

func TestMem0KeyFromEnv(t *testing.T) {
	t.Setenv("MEM0_API_KEY", "  envkey  ")
	if got := Mem0KeyFromEnvOrFile(); got != "envkey" {
		t.Errorf("key = %q, want envkey (trimmed)", got)
	}
}
