package fleetqueue

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func TestQueueLifecycle(t *testing.T) {
	q := testQueue(t)
	state, err := q.Submit("agd-1", "agent", json.RawMessage(`{"goal":"g"}`), 60)
	if err != nil || state != StateQueued {
		t.Fatalf("submit: %v state=%s", err, state)
	}
	// Idempotent re-submit reports current state, changes nothing.
	if state, _ = q.Submit("agd-1", "agent", nil, 60); state != StateQueued {
		t.Fatalf("re-submit state = %s", state)
	}
	// A claimant for a DIFFERENT task type gets nothing.
	if _, ok, _ := q.Claim("node-b", []string{"stt"}); ok {
		t.Fatal("wrong task type must not claim")
	}
	job, ok, err := q.Claim("node-a", []string{"agent", "stt"})
	if err != nil || !ok || job.ID != "agd-1" || job.Claimant != "node-a" {
		t.Fatalf("claim: %v ok=%v job=%+v", err, ok, job)
	}
	// Claimed jobs are not re-claimable while leased.
	if _, ok, _ = q.Claim("node-b", []string{"agent"}); ok {
		t.Fatal("leased job must not be re-claimable")
	}
	if err := q.Ack("agd-1", "node-a", json.RawMessage(`{"output":"done"}`), ""); err != nil {
		t.Fatal(err)
	}
	got, err := q.Get("agd-1")
	if err != nil || got.State != StateDone || !strings.Contains(string(got.Result), "done") {
		t.Fatalf("get after ack: %v %+v", err, got)
	}
	// Late duplicate ack (the at-least-once shape) is ignored, not an error.
	if err := q.Ack("agd-1", "node-b", json.RawMessage(`{"output":"late"}`), ""); err != nil {
		t.Fatal(err)
	}
	got, _ = q.Get("agd-1")
	if !strings.Contains(string(got.Result), "done") {
		t.Fatal("first ack must win over the late duplicate")
	}
}

func TestQueueNackBound(t *testing.T) {
	q := testQueue(t)
	_, _ = q.Submit("agd-n", "agent", nil, 60)
	for i := 0; i < maxNacks; i++ {
		if _, ok, _ := q.Claim("node-a", []string{"agent"}); !ok {
			t.Fatalf("claim %d must succeed", i)
		}
		if err := q.Nack("agd-n", "node-a", "build failed"); err != nil {
			t.Fatal(err)
		}
		got, _ := q.Get("agd-n")
		if got.State != StateQueued {
			t.Fatalf("nack %d must requeue, got %s", i, got.State)
		}
	}
	if _, ok, _ := q.Claim("node-a", []string{"agent"}); !ok {
		t.Fatal("final claim must succeed")
	}
	if err := q.Nack("agd-n", "node-a", "build failed again"); err != nil {
		t.Fatal(err)
	}
	got, _ := q.Get("agd-n")
	if got.State != StateFailed || !strings.Contains(got.Error, "abandoned") {
		t.Fatalf("past maxNacks the job must fail loudly, got %+v", got)
	}
}

func TestQueueLeaseExpiry(t *testing.T) {
	q := testQueue(t)
	clock := time.Now()
	q.now = func() time.Time { return clock }
	_, _ = q.Submit("agd-l", "agent", nil, 10)
	if _, ok, _ := q.Claim("node-dead", []string{"agent"}); !ok {
		t.Fatal("first claim must succeed")
	}
	// Within the lease: not claimable.
	if _, ok, _ := q.Claim("node-b", []string{"agent"}); ok {
		t.Fatal("in-lease job must not move")
	}
	// Past timeout+slack the lease expires and ANOTHER node reclaims it.
	clock = clock.Add(10*time.Second + leaseSlack + time.Minute)
	job, ok, _ := q.Claim("node-b", []string{"agent"})
	if !ok || job.Claimant != "node-b" || job.Nacks != 1 {
		t.Fatalf("expired lease must reclaim with history, got ok=%v %+v", ok, job)
	}
	// Two more expiries exhaust the bound → failed, not orbiting.
	clock = clock.Add(10*time.Second + leaseSlack + time.Minute)
	if _, ok, _ = q.Claim("node-c", []string{"agent"}); !ok {
		t.Fatal("second reclaim must succeed (nacks=2 == maxNacks)")
	}
	clock = clock.Add(10*time.Second + leaseSlack + time.Minute)
	if _, ok, _ = q.Claim("node-d", []string{"agent"}); ok {
		t.Fatal("third expiry must abandon, not reclaim")
	}
	got, _ := q.Get("agd-l")
	if got.State != StateFailed {
		t.Fatalf("job must be failed after repeated lease deaths, got %s", got.State)
	}
}

func TestQueueHTTPRoundTripAndAuth(t *testing.T) {
	q := testQueue(t)
	mux := http.NewServeMux()
	Mount(mux, q, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer tok" })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	do := func(method, path, body string, auth bool) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.Header.Set("Authorization", "Bearer tok")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Unauthorized without the bearer.
	if r := do("POST", "/fleet/queue/submit", `{"job_id":"j1","task_type":"agent"}`, false); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth submit = %d, want 401", r.StatusCode)
	}
	if r := do("POST", "/fleet/queue/submit", `{"job_id":"j1","task_type":"agent","payload":{"goal":"g"},"timeout_sec":30}`, true); r.StatusCode != http.StatusAccepted {
		t.Fatalf("submit = %d, want 202", r.StatusCode)
	}
	// Claim returns the job; an empty queue afterwards returns 204.
	r := do("POST", "/fleet/queue/claim", `{"node_id":"n1","task_types":["agent"]}`, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", r.StatusCode)
	}
	var j Job
	_ = json.NewDecoder(r.Body).Decode(&j)
	r.Body.Close()
	if j.ID != "j1" {
		t.Fatalf("claimed %+v", j)
	}
	if r := do("POST", "/fleet/queue/claim", `{"node_id":"n1","task_types":["agent"]}`, true); r.StatusCode != http.StatusNoContent {
		t.Fatalf("empty claim = %d, want 204", r.StatusCode)
	}
	if r := do("POST", "/fleet/queue/ack", `{"job_id":"j1","node_id":"n1","result":{"output":"ok"}}`, true); r.StatusCode != http.StatusNoContent {
		t.Fatalf("ack = %d, want 204", r.StatusCode)
	}
	// Results route mirrors the push wire shape (state done + data).
	r = do("GET", "/fleet/queue/jobs/j1", "", true)
	var wire struct {
		State string          `json:"state"`
		Data  json.RawMessage `json:"data"`
	}
	_ = json.NewDecoder(r.Body).Decode(&wire)
	r.Body.Close()
	if wire.State != "done" || !strings.Contains(string(wire.Data), "ok") {
		t.Fatalf("results wire = %+v", wire)
	}
	// Unknown job → 404 (the push namespace's positive denial).
	if r := do("GET", "/fleet/queue/jobs/ghost", "", true); r.StatusCode != http.StatusNotFound {
		t.Fatalf("ghost job = %d, want 404", r.StatusCode)
	}
}
