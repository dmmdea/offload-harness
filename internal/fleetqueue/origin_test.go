package fleetqueue

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Register D-101 (ADR 0087): the submitter's session label and run class ride the queued job to whichever node
// claims it, so a pulled job names the session it worked for exactly like a pushed one.

func submitWith(t *testing.T, h http.Handler, id string, headers map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/fleet/queue/submit", strings.NewReader(`{"job_id":"`+id+`","task_type":"agent","payload":{"goal":"g"},"timeout_sec":60}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit %s = %d (%s)", id, rec.Code, rec.Body.String())
	}
}

func queueMux(q *Queue) http.Handler {
	mux := http.NewServeMux()
	Mount(mux, q, func(*http.Request) bool { return true })
	return mux
}

func claimOnly(t *testing.T, q *Queue) *Job {
	t.Helper()
	job, ok, err := q.Claim("node-b", []string{"agent"})
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	return job
}

// The submit handler keeps the headers on the job. The route is behind the Mount guard (a bearer when a token is
// configured, else loopback), so a request that reaches it is one the node can authenticate: the class is kept.
func TestSubmitCarriesTheOriginAndClassToTheClaimedJob(t *testing.T) {
	q := testQueue(t)
	submitWith(t, queueMux(q), "agd-1", map[string]string{core.OriginHeader: "sess-abc", core.RunClassHeader: "bake", core.AskerHeader: "node-a"})
	job := claimOnly(t, q)
	if job.Origin != "sess-abc" || job.RunClass != "bake" || job.Asker != "node-a" {
		t.Fatalf("claimed job = origin %q class %q asker %q, want sess-abc, bake, node-a", job.Origin, job.RunClass, job.Asker)
	}
}

// The values are cleaned with the ledger's rule: a 97-byte session is refused whole (never cut), "production" is no
// class, a class is normalised, and a submitter that sends neither leaves both empty.
func TestSubmitCleansTheOriginHeaders(t *testing.T) {
	q := testQueue(t)
	h := queueMux(q)
	submitWith(t, h, "agd-long", map[string]string{core.OriginHeader: strings.Repeat("s", 97), core.RunClassHeader: "production"})
	submitWith(t, h, "agd-norm", map[string]string{core.OriginHeader: "  sess-abc  ", core.RunClassHeader: "Bake "})
	submitWith(t, h, "agd-none", nil)
	got := map[string]Job{}
	for i := 0; i < 3; i++ {
		j := claimOnly(t, q)
		got[j.ID] = *j
	}
	if j := got["agd-long"]; j.Origin != "" || j.RunClass != "" {
		t.Errorf("97 bytes and production: origin %q class %q, want both empty", j.Origin, j.RunClass)
	}
	if j := got["agd-norm"]; j.Origin != "sess-abc" || j.RunClass != "bake" {
		t.Errorf("padded and mixed case: origin %q class %q, want sess-abc and bake", j.Origin, j.RunClass)
	}
	if j := got["agd-none"]; j.Origin != "" || j.RunClass != "" {
		t.Errorf("no headers: origin %q class %q, want both empty", j.Origin, j.RunClass)
	}
}

// SubmitAttributed copies the attribution onto the stored job (the HTTP handler is one caller of it).
func TestSubmitAttributedStoresTheOriginAndClass(t *testing.T) {
	q := testQueue(t)
	if _, err := q.SubmitAttributed("agd-2", "agent", json.RawMessage(`{"goal":"g"}`), 60, Attribution{Asker: "node-a", Origin: "sess-abc", RunClass: "measurement"}); err != nil {
		t.Fatal(err)
	}
	job := claimOnly(t, q)
	if job.Origin != "sess-abc" || job.RunClass != "measurement" || job.Asker != "node-a" {
		t.Fatalf("stored job = %+v", job)
	}
}

// Old holders and new ones read each other's records: a job queued before the fields existed decodes with both
// empty, and a job that has neither marshals without the keys, so a reader one release behind sees the bytes it
// always saw.
func TestJobRoundTripsWithAndWithoutTheOriginFields(t *testing.T) {
	var old Job
	if err := json.Unmarshal([]byte(`{"id":"agd-3","task_type":"agent","payload":{},"timeout_sec":60,"state":"queued","nacks":0,"submitted":1,"asker":"node-a"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Origin != "" || old.RunClass != "" || old.Asker != "node-a" {
		t.Fatalf("pre-0087 job decoded as %+v", old)
	}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "origin_session") || strings.Contains(string(b), "run_class") {
		t.Fatalf("a job with neither field must omit both keys: %s", b)
	}
	b, err = json.Marshal(Job{ID: "agd-4", Origin: "sess-abc", RunClass: "bake"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"origin_session":"sess-abc"`) || !strings.Contains(string(b), `"run_class":"bake"`) {
		t.Fatalf("keys: %s", b)
	}
	var back Job
	if err := json.Unmarshal(b, &back); err != nil || back.Origin != "sess-abc" || back.RunClass != "bake" {
		t.Fatalf("round trip = %+v (%v)", back, err)
	}
}
