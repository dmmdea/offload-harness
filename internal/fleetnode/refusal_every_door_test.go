package fleetnode

// Every refusal of a credential writes one line in the node's journal, whichever door it came through: the lanes
// that go through authorizeGated, the doors that check the bearer themselves (chat, kvslot, compose-project,
// media-job), the poll and withdraw of a job, and the durable queue's routes. Each door keeps the status and body it
// answered with. The journal stays bounded: the throttle is keyed on the caller's address and the reason, so the
// doors share it.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// allDoorsCfg is a node on which every door that checks the credential is open and bound: the lanes of imageCfg, the
// vision and text lanes, the media-job and compose-project doors. key "" is a node with no token.
func allDoorsCfg(t *testing.T, key string) config.Config {
	t.Helper()
	c := mediaJobCfg(t)
	p := projectCfg(t)
	c.ImageGenScript = "C:/x/comfy-generate.mjs"
	c.ComposeScript, c.HyperframesDir, c.HyperframesBrowserPath = p.ComposeScript, p.HyperframesDir, p.HyperframesBrowserPath
	c.FleetComposeProjects, c.ComposeCacheDir = true, p.ComposeCacheDir
	c.VisionModel = "gemma4-e4b-vision"
	c.TextTasks = []string{"classify", "extract"}
	c.ForecastModel = "forecast"
	c.FleetAuthToken = key
	return c
}

// refusalServer is a node with every door and the queue holder's routes, listening beyond loopback.
func refusalServer(t *testing.T, key string) *Server {
	t.Helper()
	s, _ := newTestServer(t, allDoorsCfg(t, key), &fakeRunner{}, authOpts(false))
	q, err := s.EnableQueueHost(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return s
}

// refusalDoor is one place a node answers a request for want of a credential.
type refusalDoor struct {
	name, label  string // the label is what the journal calls the door
	method, path string
	body         string
	prep         func(t *testing.T, s *Server) // makes exist what the request names
	noKeyStatus  int                           // what a node with no token beyond loopback answers (0: the door is closed there)
	noKeyError   string
}

// dispatchedJob is a job a bearer holder dispatched, for the doors that name one.
func dispatchedJob(t *testing.T, s *Server) {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"door-job","task_type":"image-gen","payload":{"prompt":"hi"}}`,
		map[string]string{"Authorization": "Bearer " + holderKey})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("setup: dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
}

// forecastBody is a well-formed POST /fleet/forecast body: one short series and a horizon.
func forecastBody(jobID string) string {
	return `{"job_id":"` + jobID + `","request":{"series":[[1,2,3,4]],"prediction_length":2}}`
}

var refusalDoors = []refusalDoor{
	{name: "dispatch", label: "dispatch", method: http.MethodPost, path: "/fleet/dispatch",
		body: `{"job_id":"d1","task_type":"image-gen","payload":{}}`, noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "vision", label: "vision", method: http.MethodPost, path: "/fleet/vision",
		body: visionBody("v1", "vqa", tinyPNG(), nil), noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "text", label: "text", method: http.MethodPost, path: "/fleet/text",
		body: textBody("t1", "classify", "x", classifyParams()), noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "forecast", label: "forecast", method: http.MethodPost, path: "/fleet/forecast",
		body: forecastBody("f1"), noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "stt upload", label: "stt-upload", method: http.MethodPost, path: "/fleet/stt",
		body: `{}`, noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "media file", label: "media-file", method: http.MethodGet, path: "/fleet/media/out.png",
		noKeyStatus: 403, noKeyError: fleetTokenRequired},
	// A name that is not a bare name is answered for the credential too: a caller without it gets the auth verdict and
	// nothing about the name rule.
	{name: "media file, encoded slash", label: "media-file", method: http.MethodGet, path: "/fleet/media/a%2Fb",
		noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "media file, literal slash", label: "media-file", method: http.MethodGet, path: "/fleet/media/a/b",
		noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "media file, dot-dot", label: "media-file", method: http.MethodGet, path: "/fleet/media/..x",
		noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "media file, encoded backslash", label: "media-file", method: http.MethodGet, path: "/fleet/media/a%5Cb",
		noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "media file, empty name", label: "media-file", method: http.MethodGet, path: "/fleet/media/",
		noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "pair relay", label: "pair-relay", method: http.MethodPost, path: pairworkloads.RelayPath,
		body: `{}`, noKeyStatus: 403, noKeyError: fleetTokenRequired},
	{name: "chat", label: "chat", method: http.MethodPost, path: "/fleet/chat",
		body: `{"model":"m"}`, noKeyStatus: 403, noKeyError: chatLaneTokenRequired},
	{name: "kvslot save", label: "kvslot", method: http.MethodPost, path: "/fleet/kvslot/save",
		body: `{}`, noKeyStatus: 403, noKeyError: "kvslot lane requires fleet_auth_token on a non-loopback listener"},
	{name: "kvslot restore", label: "kvslot", method: http.MethodPost, path: "/fleet/kvslot/restore",
		body: `{}`, noKeyStatus: 403, noKeyError: "kvslot lane requires fleet_auth_token on a non-loopback listener"},
	{name: "compose project", label: "compose-project", method: http.MethodPost, path: "/fleet/compose-project", body: `{}`},
	{name: "media job", label: "media-job", method: http.MethodPost, path: "/fleet/media-job", body: `{}`},
	{name: "job poll", label: "poll", method: http.MethodGet, path: "/fleet/jobs/door-job", prep: dispatchedJob},
	{name: "job withdraw", label: "withdraw", method: http.MethodDelete, path: "/fleet/jobs/door-job", prep: dispatchedJob},
	{name: "queue submit", label: "fleet-queue", method: http.MethodPost, path: "/fleet/queue/submit", body: `{}`,
		noKeyStatus: 401, noKeyError: "unauthorized"},
	{name: "queue claim", label: "fleet-queue", method: http.MethodPost, path: "/fleet/queue/claim", body: `{}`,
		noKeyStatus: 401, noKeyError: "unauthorized"},
	{name: "queue ack", label: "fleet-queue", method: http.MethodPost, path: "/fleet/queue/ack", body: `{}`,
		noKeyStatus: 401, noKeyError: "unauthorized"},
	{name: "queue nack", label: "fleet-queue", method: http.MethodPost, path: "/fleet/queue/nack", body: `{}`,
		noKeyStatus: 401, noKeyError: "unauthorized"},
	{name: "queue job read", label: "fleet-queue", method: http.MethodGet, path: "/fleet/queue/jobs/q1",
		noKeyStatus: 401, noKeyError: "unauthorized"},
}

// journalLinesFor counts the refusal lines that name the given door.
func journalLinesFor(logged, label string) int {
	n := 0
	for _, l := range strings.Split(logged, "\n") {
		if strings.Contains(l, "refused a "+`"`+label+`"`+" request") {
			n++
		}
	}
	return n
}

func TestEveryDoorWritesOneJournalLineForARefusedCredential(t *testing.T) {
	for _, door := range refusalDoors {
		for name, c := range map[string]struct {
			hdr map[string]string
			why string
		}{
			"no bearer":    {nil, "no fleet bearer presented"},
			"wrong bearer": {map[string]string{"Authorization": "Bearer " + wrongKey}, "the fleet bearer presented is not this node's"},
		} {
			t.Run(door.name+" with "+name, func(t *testing.T) {
				s := refusalServer(t, holderKey)
				if door.prep != nil {
					door.prep(t, s)
				}
				buf := captureLog(t)
				rec := do(t, s, door.method, door.path, door.body, c.hdr)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
				}
				if msg, _ := decodeMap(t, rec)["error"].(string); msg != "unauthorized" {
					t.Fatalf("error = %q, want exactly \"unauthorized\"", msg)
				}
				if n := refusalLines(buf.String()); n != 1 {
					t.Fatalf("wrote %d refusal line(s), want exactly 1; log = %q", n, buf.String())
				}
				if n := refusalsNoted(s); n != 1 {
					t.Fatalf("the door told the journal of %d refusals for one request, want 1 (the throttle hides a second note, so only the count of what the journal kept out shows it)", n)
				}
				if n := journalLinesFor(buf.String(), door.label); n != 1 {
					t.Fatalf("the line does not name the door %q; log = %q", door.label, buf.String())
				}
				if !strings.Contains(buf.String(), c.why) {
					t.Fatalf("the line does not say why (%q); log = %q", c.why, buf.String())
				}
			})
		}
	}
}

func TestEveryDoorWritesOneJournalLineOnANodeWithNoTokenBeyondLoopback(t *testing.T) {
	for _, door := range refusalDoors {
		if door.noKeyStatus == 0 {
			continue
		}
		t.Run(door.name, func(t *testing.T) {
			s := refusalServer(t, "")
			buf := captureLog(t)
			rec := do(t, s, door.method, door.path, door.body, nil)
			if rec.Code != door.noKeyStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, door.noKeyStatus, rec.Body.String())
			}
			if msg, _ := decodeMap(t, rec)["error"].(string); msg != door.noKeyError {
				t.Fatalf("error = %q, want exactly %q", msg, door.noKeyError)
			}
			if n := refusalLines(buf.String()); n != 1 {
				t.Fatalf("wrote %d refusal line(s), want exactly 1; log = %q", n, buf.String())
			}
			if n := refusalsNoted(s); n != 1 {
				t.Fatalf("the door told the journal of %d refusals for one request, want 1", n)
			}
			if n := journalLinesFor(buf.String(), door.label); n != 1 {
				t.Fatalf("the line does not name the door %q; log = %q", door.label, buf.String())
			}
		})
	}
}

// A re-dispatch of a job the node already holds is checked against the job's own rule. Every door that reaches it has
// passed the bearer already, so only a direct call gets there without it; the refusal is journaled all the same.
func TestARefusedReDispatchOfAKnownJobWritesOneJournalLine(t *testing.T) {
	s := refusalServer(t, holderKey)
	buf := captureLog(t)
	rec := httptest.NewRecorder()
	s.answerKnownJob(rec, httptest.NewRequest(http.MethodPost, "/fleet/dispatch", nil), &JobView{ID: "known", Gated: true})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if n := refusalLines(buf.String()); n != 1 {
		t.Fatalf("wrote %d refusal line(s), want exactly 1; log = %q", n, buf.String())
	}
	if n := refusalsNoted(s); n != 1 {
		t.Fatalf("the door told the journal of %d refusals for one request, want 1", n)
	}
}

// The doors share one throttle, keyed on (caller address, reason): a caller refused at every door writes one line
// until the window moves on, a different reason or a different caller writes its own, and the door a caller varies
// buys it nothing.
func TestTheDoorsShareOneThrottleKeyedOnCallerAndReason(t *testing.T) {
	s := refusalServer(t, holderKey)
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.refusals.now = func() time.Time { return clock }
	dispatchedJob(t, s)
	buf := captureLog(t)
	round := func(addr string, hdr map[string]string) {
		for _, door := range refusalDoors {
			req := httptest.NewRequest(door.method, door.path, strings.NewReader(door.body))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = addr
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			s.Handler().ServeHTTP(httptest.NewRecorder(), req)
		}
	}
	for i := 0; i < 5; i++ {
		round("192.0.2.10:4000", nil)
	}
	if n := refusalLines(buf.String()); n != 1 {
		t.Fatalf("one caller refused %d times at %d doors wrote %d lines, want 1; log = %q", 5, len(refusalDoors), n, buf.String())
	}
	round("192.0.2.10:4001", map[string]string{"Authorization": "Bearer " + wrongKey})
	if n := refusalLines(buf.String()); n != 2 {
		t.Fatalf("a second reason from the same caller wrote %d lines in all, want 2", n)
	}
	round("192.0.2.11:4000", nil)
	if n := refusalLines(buf.String()); n != 3 {
		t.Fatalf("a second caller wrote %d lines in all, want 3", n)
	}
	clock = clock.Add(refusalLogEvery + time.Second)
	round("192.0.2.10:4002", nil)
	if n := refusalLines(buf.String()); n != 4 {
		t.Fatalf("the first refusal after the interval wrote %d lines in all, want 4", n)
	}
}
