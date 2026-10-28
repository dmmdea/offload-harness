package fleetnode

// Every lane of a node requires the fleet bearer (the fleet's one credential), not only the agent, vision,
// text and door lanes. Before, a media task type (image-gen, video-gen, animate, audio-gen, run-graph, the
// legacy stt lane) was admitted with no check at all, so any peer that could reach a listener beyond
// loopback could dispatch a run-graph whose manifest names node packs for the node to clone and install.
//
// The rule, in three cells of (token configured, listener):
//
//	token set, any listener   the bearer is required: 401 without it, with it the task is admitted
//	no token,  loopback       open, as the local MCP surface is (the loopback trust boundary)
//	no token,  beyond it      refused with 403 for every task type (a misconfiguration, never an open port)

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// asHolder is the request header of a caller that holds the node's token (none when the node has none).
func asHolder(cfg config.Config) map[string]string {
	if cfg.FleetAuthToken == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + cfg.FleetAuthToken}
}

// everyTaskType is a media task of each kind, the legacy lanes and a name no node serves: the bearer
// check precedes task validation, so an unserved task type is refused for the credential first.
var everyTaskType = []string{"image-gen", "video-gen", "animate", "audio-gen", "run-graph", "stt", "accel", "compose-video", "a-task-type-from-a-later-release"}

// A graph that decodes, and a manifest that asks the node to clone a pack from an address of the
// caller's choosing: the request the gate exists to keep out.
const (
	plainGraph      = `{"1":{"class_type":"KSampler"}}`
	hostilePackBody = `{"job_id":"rg-hostile","task_type":"run-graph","payload":{"graph":` + plainGraph +
		`,"manifest":{"node_packs":[{"name":"payload","repo":"https://example.invalid/payload"}]}}}`
)

func TestEveryTaskTypeRidesTheBearerRule(t *testing.T) {
	for _, token := range []string{"", "s3cret"} {
		for _, tt := range everyTaskType {
			if !tokenGated(config.Config{FleetAuthToken: token}, tt) {
				t.Errorf("tokenGated(token %q, %q) = false: that task type is admitted without the bearer", token, tt)
			}
		}
	}
}

func TestATokenedNodeRefusesEveryTaskTypeWithoutTheBearer(t *testing.T) {
	for _, loopback := range []bool{false, true} {
		for _, tt := range everyTaskType {
			name := tt + " beyond loopback"
			if loopback {
				name = tt + " on loopback"
			}
			t.Run(name, func(t *testing.T) {
				s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(loopback))
				body := `{"job_id":"j-` + tt + `","task_type":"` + tt + `","payload":{}}`
				for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer wrong"}} {
					rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, hdr)
					if rec.Code != http.StatusUnauthorized {
						t.Fatalf("header %v: status = %d, want 401 (body %s)", hdr, rec.Code, rec.Body.String())
					}
					if msg, _ := decodeMap(t, rec)["error"].(string); msg != "unauthorized" {
						t.Fatalf("error = %q, want exactly \"unauthorized\"", msg)
					}
				}
			})
		}
	}
}

func TestATokenlessRunGraphBeyondLoopbackIsRefusedLoggedAndNeverBuilt(t *testing.T) {
	runner := &fakeRunner{}
	s, jobs := newTestServer(t, tokenCfg("s3cret"), runner, authOpts(false))
	buf := captureLog(t)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", hostilePackBody, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if _, ok := jobs.Get("rg-hostile"); ok {
		t.Fatal("a refused dispatch left a job behind")
	}
	if n := len(runner.requests()); n != 0 {
		t.Fatalf("the runner ran %d request(s) for a refused dispatch", n)
	}
	// The task type is in the body, which a refused caller's request never gets as far as: the line names the door.
	line := buf.String()
	if !strings.Contains(line, "refused") || !strings.Contains(line, `"`+dispatchRefusalLabel+`"`) {
		t.Fatalf("the node did not log the refusal naming the dispatch door; log = %q", line)
	}
	if strings.Contains(line, "s3cret") || strings.Contains(line, "example.invalid") {
		t.Fatalf("the refusal log carries the token or the payload: %q", line)
	}
}

func TestATokenlessNodeBeyondLoopbackRefusesEveryTaskTypeWith403(t *testing.T) {
	for _, tt := range everyTaskType {
		t.Run(tt, func(t *testing.T) {
			s, _ := newTestServer(t, tokenCfg(""), &fakeRunner{}, authOpts(false))
			body := `{"job_id":"j-` + tt + `","task_type":"` + tt + `","payload":{}}`
			for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer anything"}} {
				rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, hdr)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("header %v: status = %d, want 403 (body %s)", hdr, rec.Code, rec.Body.String())
				}
				if msg, _ := decodeMap(t, rec)["error"].(string); msg != fleetTokenRequired {
					t.Fatalf("error = %q, want exactly %q", msg, fleetTokenRequired)
				}
			}
		})
	}
	if strings.Contains(fleetTokenRequired, "agent lane") {
		t.Errorf("the refusal still says it is about the agent lane only: %q", fleetTokenRequired)
	}
}

func TestALoopbackNodeWithNoTokenStillTakesARunGraph(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg(""), &fakeRunner{}, authOpts(true))
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"rg-local","task_type":"run-graph","payload":{"graph":`+plainGraph+`}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if want := `{"job_id":"rg-local","status":"accepted"}` + "\n"; rec.Body.String() != want {
		t.Fatalf("ack = %q, want the bytes the media lane always answered with, %q", rec.Body.String(), want)
	}
}

func TestATokenedNodeBeyondLoopbackTakesARunGraphWithTheBearer(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(false))
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"rg-remote","task_type":"run-graph","payload":{"graph":`+plainGraph+`}}`,
		map[string]string{"Authorization": "Bearer s3cret"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJobWith(t, s, "rg-remote", JobDone, map[string]string{"Authorization": "Bearer s3cret"})
}

// TestMediaClientThatSendsTheTokenGetsTheSameBytes: a media client that presents the node's token gets, for
// the dispatch ack and the job poll, exactly the bytes a tokenless client got from a loopback node with no
// token. The node's answer to a credentialed caller did not change; only the uncredentialed caller's did.
func TestMediaClientThatSendsTheTokenGetsTheSameBytes(t *testing.T) {
	const dispatchBody = `{"job_id":"media-1","task_type":"image-gen","payload":{"prompt":"hi"}}`
	run := func(cfg config.Config, loopback bool, hdr map[string]string) (ack, done []byte) {
		s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(loopback))
		rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody, hdr)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		ack = append([]byte(nil), rec.Body.Bytes()...)
		pollJobWith(t, s, "media-1", JobDone, hdr)
		rec = do(t, s, http.MethodGet, "/fleet/jobs/media-1", "", hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		return ack, append([]byte(nil), rec.Body.Bytes()...)
	}
	bearer := map[string]string{"Authorization": "Bearer s3cret"}
	ackPlain, donePlain := run(imageCfg(), true, nil)
	for _, loopback := range []bool{true, false} {
		ackTok, doneTok := run(tokenCfg("s3cret"), loopback, bearer)
		if !bytes.Equal(ackPlain, ackTok) {
			t.Errorf("dispatch ack (loopback %v) diverged:\n  tokenless loopback: %s\n  with the bearer:    %s", loopback, ackPlain, ackTok)
		}
		if !bytes.Equal(donePlain, doneTok) {
			t.Errorf("job poll (loopback %v) diverged:\n  tokenless loopback: %s\n  with the bearer:    %s", loopback, donePlain, doneTok)
		}
	}
}

// A media job is polled with the bearer on a node that has a token, like every other gated job: without it
// the poll is 401 and says nothing about the job.
func TestAMediaJobIsPolledWithTheBearerOnATokenedNode(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(true))
	hdr := map[string]string{"Authorization": "Bearer s3cret"}
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"m-1","task_type":"image-gen","payload":{"prompt":"hi"}}`, hdr); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	pollJobWith(t, s, "m-1", JobDone, hdr)
	rec := do(t, s, http.MethodGet, "/fleet/jobs/m-1", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a poll with no bearer = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "image_path") {
		t.Fatalf("a refused poll disclosed the job's output: %s", rec.Body.String())
	}
}

// pollJobWith is pollJob for a node whose jobs are polled with a credential.
func pollJobWith(t *testing.T, s *Server, id string, want JobState, hdr map[string]string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := do(t, s, http.MethodGet, "/fleet/jobs/"+id, "", hdr)
		if rec.Code == http.StatusOK {
			m := decodeMap(t, rec)
			if m["state"] == string(want) {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached state %s (last %d %s)", id, want, rec.Code, rec.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
