// Agent-lane bearer-auth tests (multi-node delegation plan, Task 3; auth
// scope v1 = reshape delta 10: the AGENT lane ONLY). Two invariants carry the
// whole feature:
//
//  1. The agent lane is never reachable un-authed beyond loopback — token
//     wrong/missing ⇒ 401 before ANY validation or job-state disclosure, and
//     a tokenless non-loopback listener refuses agent dispatch outright (403).
//  2. Every media path (dispatch, job poll, media file, health) is
//     BYTE-IDENTICAL with or without a token configured — the deployed 0.62.1
//     media clients are tokenless and must never start 401ing.
//
// task_type "agent" is not registered in tasks.go yet (Task 4 adds it), so an
// AUTHORIZED agent dispatch 400s at BuildRequest ("unsupported task_type").
// The tests lean on that deliberately: a 400 proves auth passed AND that the
// auth decision precedes task validation (401/403 rows never reach the 400).
package fleetnode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// tokenCfg is imageCfg (image-gen + run-graph bound) plus the agent-lane token.
func tokenCfg(token string) config.Config {
	c := imageCfg()
	c.FleetAuthToken = token
	return c
}

// authOpts builds full Options with an explicit loopback stance (newTestServer's
// default Options leave LoopbackListener false = non-loopback = fail closed).
func authOpts(loopback bool) *Options {
	return &Options{
		NodeID:           "testnode",
		Snapshot:         goodSnapshot,
		Footprints:       func() []FootprintEntry { return nil },
		GpuVendor:        "nvidia",
		GpuArch:          "ampere",
		LoopbackListener: loopback,
	}
}

// TestAgentDispatchAuthMatrix drives every (token, listener, header) cell of
// the dispatch guard. 401/403 error strings are asserted EXACTLY — they are
// contract text the delegator client will key on, and a contains-match could
// hide drift.
func TestAgentDispatchAuthMatrix(t *testing.T) {
	const agentBody = `{"job_id":"agd-1","task_type":"agent","payload":{}}`
	cases := []struct {
		name       string
		token      string // cfg.FleetAuthToken ("" = unset)
		loopback   bool
		header     string // Authorization value ("" = absent)
		wantStatus int
		wantErr    string // exact match for 401/403; substring for the 400 rows
		exact      bool
	}{
		{"token set, no header", "s3cret", true, "", http.StatusUnauthorized, "unauthorized", true},
		{"token set, wrong token", "s3cret", true, "Bearer wrong", http.StatusUnauthorized, "unauthorized", true},
		{"token set, wrong scheme", "s3cret", true, "Basic s3cret", http.StatusUnauthorized, "unauthorized", true},
		{"token set, token as prefix", "s3cret", true, "Bearer s3cretplus", http.StatusUnauthorized, "unauthorized", true},
		// Right token ⇒ auth passes; the request then dies in BuildRequest
		// because "agent" is not a registered task yet (Task 4) — the 400
		// PROVES the auth gate sits before task validation.
		{"token set, right token, loopback", "s3cret", true, "Bearer s3cret", http.StatusBadRequest, "unsupported task_type", false},
		{"token set, right token, non-loopback", "s3cret", false, "Bearer s3cret", http.StatusBadRequest, "unsupported task_type", false},
		// No token + loopback: the agent lane is open locally (same trust
		// boundary as the local MCP surface).
		{"no token, loopback", "", true, "", http.StatusBadRequest, "unsupported task_type", false},
		// No token + non-loopback: loud refusal, header or not — there is no
		// credential that COULD authorize this listener.
		{"no token, non-loopback", "", false, "", http.StatusForbidden, "agent lane requires fleet_auth_token on a non-loopback listener", true},
		{"no token, non-loopback, header presented", "", false, "Bearer anything", http.StatusForbidden, "agent lane requires fleet_auth_token on a non-loopback listener", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t, tokenCfg(tc.token), &fakeRunner{}, authOpts(tc.loopback))
			var hdr map[string]string
			if tc.header != "" {
				hdr = map[string]string{"Authorization": tc.header}
			}
			rec := do(t, s, http.MethodPost, "/fleet/dispatch", agentBody, hdr)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			m := decodeMap(t, rec)
			msg, _ := m["error"].(string)
			if tc.exact {
				if msg != tc.wantErr {
					t.Fatalf("error = %q, want exactly %q", msg, tc.wantErr)
				}
			} else {
				wantErrorShape(t, rec, tc.wantStatus, tc.wantErr)
			}
		})
	}
}

// TestAgentAuthPrecedesEnvelopeValidation: the auth verdict outranks even the
// job_id-required 400 — an unauthorized agent caller learns NOTHING about the
// envelope's validity, only that it is unauthorized.
func TestAgentAuthPrecedesEnvelopeValidation(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(true))
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"task_type":"agent","payload":{}}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (auth must precede job_id validation; body %s)", rec.Code, rec.Body.String())
	}
}

// TestAgentAuthPrecedesKnownJobReack: re-dispatching a job id this node
// already owns AS task_type "agent" without the token must 401, never re-ack
// — the auth check sits after decode but BEFORE the known-job lookup, so an
// unauthorized caller cannot use the agent lane to probe job existence/state.
func TestAgentAuthPrecedesKnownJobReack(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(true))
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"reack-1","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("media dispatch status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "reack-1", JobDone)

	rec = do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"reack-1","task_type":"agent","payload":{}}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent re-dispatch of known job = %d, want 401 before the re-ack path (body %s)", rec.Code, rec.Body.String())
	}
}

// TestMediaLaneByteIdenticalWithTokenConfigured is the deployed-client pin
// (reshape delta 10): the SAME media dispatch + poll, against a server with a
// token configured vs one without, must produce byte-for-byte identical
// responses — auth v1 touches the agent lane only, and the Aorus 0.62.1 media
// client (which sends no Authorization header) must never 401.
func TestMediaLaneByteIdenticalWithTokenConfigured(t *testing.T) {
	const dispatchBody = `{"job_id":"media-1","task_type":"image-gen","payload":{"prompt":"hi"}}`
	run := func(cfg config.Config, hdr map[string]string) (ack, done []byte) {
		s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
		rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody, hdr)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		ack = append([]byte(nil), rec.Body.Bytes()...)
		pollJob(t, s, "media-1", JobDone)
		rec = do(t, s, http.MethodGet, "/fleet/jobs/media-1", "", hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		done = append([]byte(nil), rec.Body.Bytes()...)
		return ack, done
	}

	ackPlain, donePlain := run(imageCfg(), nil)
	ackToken, doneToken := run(tokenCfg("s3cret"), nil)
	if !bytes.Equal(ackPlain, ackToken) {
		t.Errorf("dispatch ack diverged with token configured:\n  no token: %s\n  token:    %s", ackPlain, ackToken)
	}
	if !bytes.Equal(donePlain, doneToken) {
		t.Errorf("job poll diverged with token configured:\n  no token: %s\n  token:    %s", donePlain, doneToken)
	}

	// A media client sending a BOGUS Authorization header (proxies do) must
	// also be untouched: the media lane ignores the header entirely.
	ackBogus, doneBogus := run(tokenCfg("s3cret"), map[string]string{"Authorization": "Bearer wrong"})
	if !bytes.Equal(ackPlain, ackBogus) || !bytes.Equal(donePlain, doneBogus) {
		t.Errorf("media lane inspected the Authorization header (must ignore it):\n  ack %s\n  done %s", ackBogus, doneBogus)
	}
}

// TestHealthAndMediaFilesNeverRequireAuth: /fleet/health (capability
// advertisement — the dispatcher must keep routing) and /fleet/media/{f}
// (the deployed media client's artifact fetch) stay open with a token set.
func TestHealthAndMediaFilesNeverRequireAuth(t *testing.T) {
	mediaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mediaDir, "render-x.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := tokenCfg("s3cret")
	cfg.MediaDir = mediaDir
	s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))

	if rec := do(t, s, http.MethodGet, "/fleet/health", "", nil); rec.Code != http.StatusOK {
		t.Errorf("health without auth = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, http.MethodGet, "/fleet/media/render-x.png", "", nil); rec.Code != http.StatusOK {
		t.Errorf("media file without auth = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAgentJobPollAuth: jobs CREATED by an agent dispatch are the only ones
// whose /fleet/jobs/{id} poll is token-gated. The job is seeded straight into
// the store via AcceptAgent because the dispatch path cannot create one until
// Task 4 registers the task — this exercises the exact marker mechanism the
// server will use then.
func TestAgentJobPollAuth(t *testing.T) {
	s, jobs := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(true))
	jobs.AcceptAgent("agd-poll", func(ctx context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"output":"done"}`), nil
	})

	deadline := time.Now().Add(5 * time.Second)
	auth := map[string]string{"Authorization": "Bearer s3cret"}
	for {
		rec := do(t, s, http.MethodGet, "/fleet/jobs/agd-poll", "", auth)
		if rec.Code == http.StatusOK && decodeMap(t, rec)["state"] == string(JobDone) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent job never reached done with the right token")
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec := do(t, s, http.MethodGet, "/fleet/jobs/agd-poll", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent job poll without token = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if msg, _ := decodeMap(t, rec)["error"].(string); msg != "unauthorized" {
		t.Fatalf("error = %q, want exactly \"unauthorized\"", msg)
	}
	rec = do(t, s, http.MethodGet, "/fleet/jobs/agd-poll", "", map[string]string{"Authorization": "Bearer wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent job poll with wrong token = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}

	// An unknown id stays 404 whether or not auth is presented — the auth gate
	// keys on the job RECORD's marker, so there is nothing to gate for an id
	// the store does not hold (evicted agent jobs age back to plain 404s).
	rec = do(t, s, http.MethodGet, "/fleet/jobs/never-seen", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job poll = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAgentJobPollOpenWithoutToken: with no token configured (the loopback-
// only posture) an agent job polls without credentials — matching dispatch,
// where the tokenless loopback agent lane is open.
func TestAgentJobPollOpenWithoutToken(t *testing.T) {
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, authOpts(true))
	jobs.AcceptAgent("agd-open", func(ctx context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"output":"done"}`), nil
	})
	pollJob(t, s, "agd-open", JobDone)
}

// TestJobsAgentMarker pins the store mechanism: AcceptAgent stamps the record,
// Accept does not, and the marker survives into every JobView copy — it lives
// ON the job (set atomically at creation, evicted with it), so there is no
// window where a poller can observe an agent job unmarked.
func TestJobsAgentMarker(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	t.Cleanup(func() { j.DrainAndStop(2 * time.Second) })
	j.AcceptAgent("a", func(ctx context.Context) (json.RawMessage, error) { return nil, nil })
	j.Accept("m", func(ctx context.Context) (json.RawMessage, error) { return nil, nil })
	if v, ok := j.Get("a"); !ok || !v.Agent {
		t.Errorf("AcceptAgent job: Agent = %v (ok=%v), want true", v != nil && v.Agent, ok)
	}
	if v, ok := j.Get("m"); !ok || v.Agent {
		t.Errorf("Accept job: Agent marker set, want false (media polls must stay open)")
	}
}

// agentLaneCfg is a node with the agent lane genuinely ON: enabled, a seat, a
// token, and a test-owned BaseDir for the materialized job dir. The matrix
// above runs with the lane OFF, so every authorized row there dies at
// "unsupported task_type" — which proves the auth ORDER but exercises none of
// the lane it is guarding. With the lane on, an authorized row must reach 202.
func agentLaneCfg(t *testing.T, token string) config.Config {
	t.Helper()
	return config.Config{
		Home:              t.TempDir(),
		FleetAgentEnabled: true,
		AgentModel:        "agent-seat",
		FleetAuthToken:    token,
	}
}

const agentLaneSchema = `{"properties":{"answer":{"type":"string"}}}`

// agentLaneBody is a VALID agent dispatch envelope — one that reaches 202 when
// authorized, so an auth row's outcome is unambiguous.
func agentLaneBody(jobID string) string {
	return `{"job_id":"` + jobID + `","task_type":"agent","payload":` +
		`{"schema_version":1,"goal":"summarize","output_schema":` + agentLaneSchema + `}}`
}

// TestAgentLaneBearerHeaderShapes drives the credential parser's edges with the
// lane ON. bearerOK matches the SCHEME case-insensitively (RFC 7235 says the
// scheme is case-insensitive) and the TOKEN byte-exactly — a refactor to a
// plain HasPrefix("Bearer ") would break every RFC-compliant client that sends
// "bearer", and today it would break with zero test failures. The negative rows
// pin the other half: no separator, an empty token, and any whitespace the
// header picked up are all NOT the token, however close they look.
func TestAgentLaneBearerHeaderShapes(t *testing.T) {
	const token = "s3cret"
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"canonical scheme", "Bearer " + token, http.StatusAccepted},
		{"lowercase scheme (RFC 7235: scheme is case-insensitive)", "bearer " + token, http.StatusAccepted},
		{"uppercase scheme", "BEARER " + token, http.StatusAccepted},
		{"mixed-case scheme", "BeArEr " + token, http.StatusAccepted},
		{"empty token after the scheme", "Bearer ", http.StatusUnauthorized},
		{"bare scheme, no separator", "Bearer", http.StatusUnauthorized},
		{"double space before the token", "Bearer  " + token, http.StatusUnauthorized},
		{"trailing space inside the token", "Bearer " + token + " ", http.StatusUnauthorized},
		{"leading tab instead of a space", "Bearer\t" + token, http.StatusUnauthorized},
		{"header absent", "", http.StatusUnauthorized},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t, agentLaneCfg(t, token), &fakeRunner{}, authOpts(true))
			var hdr map[string]string
			if tc.header != "" {
				hdr = map[string]string{"Authorization": tc.header}
			}
			rec := do(t, s, http.MethodPost, "/fleet/dispatch", agentLaneBody(fmt.Sprintf("agd-hdr-%d", i)), hdr)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusUnauthorized {
				if msg, _ := decodeMap(t, rec)["error"].(string); msg != "unauthorized" {
					t.Fatalf("error = %q, want exactly \"unauthorized\"", msg)
				}
			}
		})
	}
}
