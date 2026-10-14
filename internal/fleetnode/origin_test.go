package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// Register D-101 (ADR 0085): a node stamps the asker's session and run class on its own ledger row, from two
// headers, by the ledger's own cleaning rule, and honours the class only for a request it can authenticate.

func originReq(session, class string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", nil)
	if session != "" {
		r.Header.Set(core.OriginHeader, session)
	}
	if class != "" {
		r.Header.Set(core.RunClassHeader, class)
	}
	return r
}

func TestOriginOfIsSanitizedLikeTheLedgerRule(t *testing.T) {
	cases := []struct {
		name, session, class string
		wantSession          string
		wantClass            string
	}{
		{"a session is kept", "sess-abc", "", "sess-abc", ""},
		{"padding is trimmed", "  sess-abc  ", "", "sess-abc", ""},
		{"interior spaces are kept", "two words", "", "two words", ""},
		{"96 bytes is kept", strings.Repeat("s", 96), "", strings.Repeat("s", 96), ""},
		{"97 bytes is refused, not truncated", strings.Repeat("s", 97), "", "", ""},
		{"a control sequence is refused", "\x1b[31m", "", "", ""},
		{"non-ASCII is refused", "sess-café", "", "", ""},
		{"a class is kept", "sess-abc", "bake", "sess-abc", "bake"},
		{"a class is normalised", "sess-abc", "Bake ", "sess-abc", "bake"},
		{"a class with a space is refused", "sess-abc", "x y", "sess-abc", ""},
		{"production is no class", "sess-abc", "production", "sess-abc", ""},
		{"an over-long class is refused", "sess-abc", strings.Repeat("c", 33), "sess-abc", ""},
		{"nothing sent, nothing read", "", "", "", ""},
	}
	for _, c := range cases {
		session, class := originOf(originReq(c.session, c.class), true)
		if session != c.wantSession || class != c.wantClass {
			t.Errorf("%s: originOf = (%q, %q), want (%q, %q)", c.name, session, class, c.wantSession, c.wantClass)
		}
	}
}

// A request the node cannot authenticate (a media dispatch to a tokenless node, which the node leaves open on
// purpose) can name a session, but it cannot hide the node's rows: its class is dropped, and so is a session label
// that carries a class of its own (a "bake:" prefix or the go-test label), because the ledger reads a class off
// such a label when the row names none.
func TestOriginOfDropsTheClassOfARequestItCannotAuthenticate(t *testing.T) {
	cases := []struct {
		name, session, class string
		wantSession          string
	}{
		{"a plain session is kept", "sess-abc", "bake", "sess-abc"},
		{"a class-prefixed session is dropped", "bake:run-7", "", ""},
		{"a measurement-prefixed session is dropped", "measurement:x", "", ""},
		{"the go-test label is dropped", "go-test", "", ""},
		{"another prefix is an ordinary session", "tenant:acme", "", "tenant:acme"},
	}
	for _, c := range cases {
		session, class := originOf(originReq(c.session, c.class), false)
		if session != c.wantSession || class != "" {
			t.Errorf("%s: untrusted originOf = (%q, %q), want (%q, %q)", c.name, session, class, c.wantSession, "")
		}
	}
	// The same labels from a request the node can authenticate are kept: a bake that dispatches to its own fleet.
	if session, class := originOf(originReq("bake:run-7", "bake"), true); session != "bake:run-7" || class != "bake" {
		t.Errorf("trusted originOf = (%q, %q), want (bake:run-7, bake)", session, class)
	}
}

func dispatchAndWait(t *testing.T, s *Server, id string, headers map[string]string) {
	t.Helper()
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"`+id+`","task_type":"image-gen","payload":{"prompt":"hi"}}`, headers); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch %s = %d (body %s)", id, rec.Code, rec.Body.String())
	}
	pollJob(t, s, id, JobDone)
}

func requestFor(t *testing.T, fr *fakeRunner, id string) core.Request {
	t.Helper()
	for _, r := range fr.requests() {
		if r.FleetJobID == id {
			return r
		}
	}
	t.Fatalf("the runner never saw job %s (saw %d requests)", id, len(fr.requests()))
	return core.Request{}
}

// The asker's session and class reach the request the node's runner builds, so the pipeline writes them on the
// node's row. A bearer-authenticated media dispatch is a request the node can authenticate.
func TestPushedJobStampsTheAskersOriginOnTheNodesRequest(t *testing.T) {
	fr := &fakeRunner{}
	s, _ := newTestServer(t, tokenCfg("s3cret"), fr, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer s3cret"}
	dispatchAndWait(t, s, "org-1", map[string]string{"Authorization": "Bearer s3cret", core.OriginHeader: "sess-abc", core.RunClassHeader: "bake"})
	dispatchAndWait(t, s, "org-2", auth) // the control: no headers, both empty
	dispatchAndWait(t, s, "org-3", map[string]string{"Authorization": "Bearer s3cret", core.OriginHeader: strings.Repeat("s", 97)})

	if r := requestFor(t, fr, "org-1"); r.OriginSession != "sess-abc" || r.RunClass != "bake" {
		t.Fatalf("request = session %q class %q, want sess-abc and bake", r.OriginSession, r.RunClass)
	}
	if r := requestFor(t, fr, "org-2"); r.OriginSession != "" || r.RunClass != "" {
		t.Fatalf("no headers: session %q class %q, want both empty", r.OriginSession, r.RunClass)
	}
	if r := requestFor(t, fr, "org-3"); r.OriginSession != "" {
		t.Fatalf("a 97-byte session was kept as %q, want it refused", r.OriginSession)
	}
}

// The trust rule, end to end. A media dispatch to a node with no token (open by design: deployed media clients are
// tokenless) keeps the session and drops the class; with a token configured but no bearer sent it is the same; with the
// bearer the class is kept. Without this a peer that can reach the port could label the node's rows "bake" and hide
// them from every production view.
func TestTokenlessMediaDispatchIgnoresAWireRunClass(t *testing.T) {
	hdr := map[string]string{core.OriginHeader: "sess-abc", core.RunClassHeader: "bake"}

	fr := &fakeRunner{}
	s, _ := newTestServer(t, imageCfg(), fr, authOpts(false))
	dispatchAndWait(t, s, "tokenless-1", hdr)
	if r := requestFor(t, fr, "tokenless-1"); r.RunClass != "" || r.OriginSession != "sess-abc" {
		t.Fatalf("tokenless node: class %q session %q, want no class and the session kept", r.RunClass, r.OriginSession)
	}

	fr = &fakeRunner{}
	s, _ = newTestServer(t, tokenCfg("s3cret"), fr, authOpts(false))
	dispatchAndWait(t, s, "nobearer-1", hdr)
	if r := requestFor(t, fr, "nobearer-1"); r.RunClass != "" {
		t.Fatalf("token configured, no bearer sent: class %q, want none", r.RunClass)
	}
	wrong := map[string]string{"Authorization": "Bearer nope", core.OriginHeader: "sess-abc", core.RunClassHeader: "bake"}
	dispatchAndWait(t, s, "wrongbearer-1", wrong)
	if r := requestFor(t, fr, "wrongbearer-1"); r.RunClass != "" {
		t.Fatalf("token configured, wrong bearer sent: class %q, want none", r.RunClass)
	}
	right := map[string]string{"Authorization": "Bearer s3cret", core.OriginHeader: "sess-abc", core.RunClassHeader: "bake"}
	dispatchAndWait(t, s, "bearer-1", right)
	if r := requestFor(t, fr, "bearer-1"); r.RunClass != "bake" {
		t.Fatalf("right bearer: class %q, want bake", r.RunClass)
	}

	// A class-prefixed session from the unauthenticated caller does not smuggle the class in through the label.
	fr = &fakeRunner{}
	s, _ = newTestServer(t, imageCfg(), fr, authOpts(false))
	dispatchAndWait(t, s, "prefix-1", map[string]string{core.OriginHeader: "bake:run-7"})
	if r := requestFor(t, fr, "prefix-1"); r.OriginSession != "" || r.RunClass != "" {
		t.Fatalf("unauthenticated bake: label: session %q class %q, want neither", r.OriginSession, r.RunClass)
	}
}

// Every lane joins admit, so the text lane stamps the same two fields. The text lane is token-gated: on a loopback
// node with no token the request passed the node's own gate (the same trust boundary as the local MCP surface), so
// its class is honoured.
func TestTextLaneDispatchStampsTheAskersOrigin(t *testing.T) {
	fr := &fakeRunner{}
	s, _ := newTestServer(t, textCfg(""), fr, authOpts(true))
	hdr := map[string]string{core.OriginHeader: "sess-abc", core.RunClassHeader: "bake"}
	if rec := do(t, s, http.MethodPost, "/fleet/text", textBody("txt-org-1", "classify", "an invoice is overdue", classifyParams()), hdr); rec.Code != http.StatusAccepted {
		t.Fatalf("text dispatch = %d (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "txt-org-1", JobDone)
	if r := requestFor(t, fr, "txt-org-1"); r.OriginSession != "sess-abc" || r.RunClass != "bake" {
		t.Fatalf("text request = session %q class %q, want sess-abc and bake", r.OriginSession, r.RunClass)
	}
}

// A pulled job carries the submitter's origin on the queued record; the claim loop cleans it again (the record came
// over the network) and stamps it where the push door does. A holder is reached with the node's bearer, so the class
// it hands over is one the node can authenticate.
func TestPulledJobStampsTheAskersOriginOnTheNodesRequest(t *testing.T) {
	for _, tc := range []struct {
		id, origin, class, wantOrigin, wantClass string
	}{
		{"pulled-org-1", "sess-abc", "bake", "sess-abc", "bake"},
		{"pulled-org-2", "", "", "", ""},
		{"pulled-org-3", "sess\x00abc", "production", "", ""}, // a hostile record is cleaned again
		{"pulled-org-4", strings.Repeat("s", 97), "Measurement", "", "measurement"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			fr := &fakeRunner{}
			cfg := imageCfg()
			cfg.Home = t.TempDir()
			cfg.FleetAgentEnabled = true
			cfg.AgentModel = "agent-seat"
			cfg.FleetAuthToken = "tok"
			s, jobs := newTestServer(t, cfg, fr, authOpts(true))
			served := false
			holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fleet/queue/claim" {
					if served {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					served = true
					_ = json.NewEncoder(w).Encode(fleetqueue.Job{
						ID: tc.id, TaskType: "agent", Origin: tc.origin, RunClass: tc.class,
						Payload: json.RawMessage(`{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `}`),
					})
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer holder.Close()
			client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
			if id, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok || id != tc.id {
				t.Fatalf("claimOne = %q, %v", id, ok)
			}
			waitJobState(t, jobs, tc.id, JobDone)
			r := requestFor(t, fr, tc.id)
			if r.OriginSession != tc.wantOrigin || r.RunClass != tc.wantClass {
				t.Fatalf("pulled request = session %q class %q, want %q and %q", r.OriginSession, r.RunClass, tc.wantOrigin, tc.wantClass)
			}
		})
	}
}
