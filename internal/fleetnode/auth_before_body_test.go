package fleetnode

// The bearer is checked before a request body is read on the three lanes that take one through the shared
// admission path: /fleet/dispatch, /fleet/vision and /fleet/text. A caller without the node's credential is
// answered 401 (or 403 on a node that has none and listens beyond loopback) before the node has buffered a byte
// of its body, decoded it, or looked at its content type, so what such a caller learns about the node is the
// auth verdict and nothing else. A caller that does hold the bearer still gets the same validation answers as
// before, and a loopback node with no token still admits.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"encoding/json"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// holderKey is the key these tests configure on a node; the wrong one is a different string of the same shape.
const (
	holderKey = "holder-key-0001"
	wrongKey  = "holder-key-0002"
)

// countingBody is a request body of filler bytes that counts what the handler reads of it.
type countingBody struct {
	left  int64
	read  int64
	calls int
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.calls++
	if c.left <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > c.left {
		n = c.left
	}
	for i := int64(0); i < n; i++ {
		p[i] = ' '
	}
	c.left -= n
	c.read += n
	return int(n), nil
}

func (c *countingBody) Close() error { return nil }

// bodyLanes are the three routes, each with a body the node admits and the config that lets it.
var bodyLanes = []struct {
	name, path, valid string
	malformed         string // a body the strict decode refuses
	tooBigMessage     string // what a holder is told when the body is over the cap
	badBodyMessage    string
}{
	{"dispatch", "/fleet/dispatch", `{"job_id":"lane-d","task_type":"image-gen","payload":{"prompt":"hi"}}`,
		`{"job_id":"lane-d","task_type":"image-gen","payload":{},"surprise":1}`, "request body too large", "malformed dispatch body"},
	{"vision", "/fleet/vision", visionBody("lane-v", "vqa", tinyPNG(), map[string]string{"question": "what is it?"}),
		`{"job_id":"lane-v","task":"vqa","image":"x","surprise":1}`, "request body too large", "malformed vision body"},
	{"text", "/fleet/text", textBody("lane-t", "classify", "an invoice is overdue", classifyParams()),
		`{"job_id":"lane-t","task":"classify","input":"x","surprise":1}`, "request body too large", "malformed text body"},
}

func bodyLaneCfg(key string) config.Config {
	c := textCfg(key)
	c.VisionModel = "gemma4-e4b-vision"
	return c
}

func bodyLaneServer(t *testing.T, key string, loopback bool) *Server {
	t.Helper()
	r := &visionRunner{res: core.Result{OK: true, Data: json.RawMessage(`{"answer":"a cat"}`)}}
	s, _ := newTestServer(t, bodyLaneCfg(key), r, authOpts(loopback))
	return s
}

// post sends one request whose body is the given reader, declared at the given length, through the routed handler.
func post(s *Server, path string, body io.Reader, length int64, contentType string, hdr map[string]string) *httptest.ResponseRecorder {
	return send(s, http.MethodPost, path, body, length, contentType, hdr)
}

// send is post for any method that carries a body.
func send(s *Server, method, path string, body io.Reader, length int64, contentType string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.ContentLength = length
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestABearerlessPostToABodyLaneReadsNoBodyBytes(t *testing.T) {
	const size = 20 << 20
	for _, tc := range bodyLanes {
		for name, hdr := range map[string]map[string]string{
			"no bearer":    nil,
			"wrong bearer": {"Authorization": "Bearer " + wrongKey},
		} {
			t.Run(tc.name+" with "+name, func(t *testing.T) {
				s := bodyLaneServer(t, holderKey, false)
				cb := &countingBody{left: size}
				rec := post(s, tc.path, cb, size, "application/json", hdr)
				if cb.read != 0 {
					t.Errorf("the node read %d byte(s) of a body it was going to refuse for its credential", cb.read)
				}
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestATokenlessNodeBeyondLoopbackReadsNoBodyBytesEither(t *testing.T) {
	const size = 20 << 20
	for _, tc := range bodyLanes {
		t.Run(tc.name, func(t *testing.T) {
			s := bodyLaneServer(t, "", false)
			cb := &countingBody{left: size}
			rec := post(s, tc.path, cb, size, "application/json", nil)
			if cb.read != 0 {
				t.Errorf("the node read %d byte(s) of a body it was going to refuse for want of a token", cb.read)
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// A caller without the credential is told it is unauthorized, never that its body was malformed, of the wrong
// type or too big.
func TestABearerlessCallerGetsTheAuthVerdictNotAValidationError(t *testing.T) {
	for _, tc := range bodyLanes {
		cases := []struct {
			name        string
			body        string
			contentType string
			size        int
		}{
			{"a malformed body", tc.malformed, "application/json", 0},
			{"a wrong content type", tc.valid, "text/plain", 0},
			{"an oversized body", "", "application/json", 20 << 20},
		}
		for _, c := range cases {
			t.Run(tc.name+" with "+c.name, func(t *testing.T) {
				s := bodyLaneServer(t, holderKey, false)
				var rec *httptest.ResponseRecorder
				if c.size > 0 {
					rec = post(s, tc.path, &countingBody{left: int64(c.size)}, int64(c.size), c.contentType, nil)
				} else {
					rec = post(s, tc.path, strings.NewReader(c.body), int64(len(c.body)), c.contentType, nil)
				}
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
				}
				if msg, _ := decodeMap(t, rec)["error"].(string); msg != "unauthorized" {
					t.Fatalf("error = %q, want exactly \"unauthorized\"", msg)
				}
			})
		}
	}
}

// A holder of the bearer is answered as before: the same 400s, and a valid request admitted.
func TestABearerHolderStillGetsTheValidationAnswers(t *testing.T) {
	hold := map[string]string{"Authorization": "Bearer " + holderKey}
	for _, tc := range bodyLanes {
		t.Run(tc.name, func(t *testing.T) {
			s := bodyLaneServer(t, holderKey, false)
			rec := post(s, tc.path, strings.NewReader(tc.malformed), int64(len(tc.malformed)), "application/json", hold)
			wantErrorShape(t, rec, http.StatusBadRequest, tc.badBodyMessage)
			rec = post(s, tc.path, strings.NewReader(tc.valid), int64(len(tc.valid)), "text/plain", hold)
			wantErrorShape(t, rec, http.StatusBadRequest, "content-type must be application/json")
			rec = post(s, tc.path, &countingBody{left: 20 << 20}, 20<<20, "application/json", hold)
			wantErrorShape(t, rec, http.StatusBadRequest, tc.tooBigMessage)
			rec = post(s, tc.path, strings.NewReader(tc.valid), int64(len(tc.valid)), "application/json", hold)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("a valid request from a holder: status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// A loopback node with no token is open, as before: the body is read and a valid request admitted.
func TestALoopbackNodeWithNoTokenStillReadsAndAdmitsTheBodyLanes(t *testing.T) {
	for _, tc := range bodyLanes {
		t.Run(tc.name, func(t *testing.T) {
			s := bodyLaneServer(t, "", true)
			rec := post(s, tc.path, strings.NewReader(tc.malformed), int64(len(tc.malformed)), "application/json", nil)
			wantErrorShape(t, rec, http.StatusBadRequest, tc.badBodyMessage)
			rec = post(s, tc.path, strings.NewReader(tc.valid), int64(len(tc.valid)), "application/json", nil)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// admit keeps its own check as a backstop for a caller that forgets to make the first one: a request that reaches
// it without the bearer is refused, and no job exists afterwards.
func TestAdmitRefusesABearerlessRequestEvenWhenItsCallerDidNotCheck(t *testing.T) {
	s, jobs := newTestServer(t, tokenCfg(holderKey), &fakeRunner{}, authOpts(false))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", nil)
	s.admit(rec, req, dispatchEnvelope{JobID: "admit-backstop", TaskType: "image-gen", Payload: json.RawMessage(`{"prompt":"hi"}`)})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if _, ok := jobs.Get("admit-backstop"); ok {
		t.Fatal("a refused request left a job behind")
	}
}
