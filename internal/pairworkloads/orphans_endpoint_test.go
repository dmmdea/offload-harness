package pairworkloads

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// plantMarker writes a register file by hand: the marker of a producer that is
// gone (pid 0 reads as dead whatever the liveness seam says), holding the
// running frame of one job. endpoint "" writes a LEGACY marker, the shape
// every marker had before the register recorded where a card was posted.
func plantMarker(t *testing.T, dir, jobID, endpoint string) string {
	t.Helper()
	info := map[string]json.RawMessage{
		"id":    mustJSON(jobID),
		"state": mustJSON("running"),
	}
	body, err := json.Marshal(openMarker{PID: 0, WrittenMs: time.Now().UnixMilli(), Endpoint: endpoint, Info: info})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, markerName(0, jobID))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// roundTripFunc lets a test answer an emitter's posts without a network, so a
// sweeper configured for the DEFAULT endpoint never reaches a real PAIR.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// answerWith is an HTTP client whose every post is answered with status code.
func answerWith(code int) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: code, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
	})}
}

// defaultEndpointSweeper is an enabled emitter whose endpoint is the default
// ingress, with its HTTP client replaced by a recorder of the posted URLs.
func defaultEndpointSweeper(r *orphanRig) (*Emitter, *[]string) {
	var mu sync.Mutex
	var urls []string
	e := New(Config{Enabled: true, Endpoint: DefaultEndpoint, AppDir: r.appDir, OpenDir: r.dir})
	e.alive = dead
	e.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		urls = append(urls, req.URL.String())
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
	})}
	return e, &urls
}

// captureLogs redirects the standard logger for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// D3: a marker records the endpoint its card was posted to.
func TestMarkerRecordsItsEndpoint(t *testing.T) {
	r := newOrphanRig(t)
	p := r.emitter(nil, nil)
	runningJob(p, "agd-ep")
	read := func() openMarker {
		t.Helper()
		files := r.files(t)
		if len(files) != 1 {
			t.Fatalf("want one marker, got %v", files)
		}
		raw, err := os.ReadFile(filepath.Join(r.dir, files[0]))
		if err != nil {
			t.Fatal(err)
		}
		var m openMarker
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := read(); m.Endpoint != r.url {
		t.Fatalf("an in-flight marker must record its emitter's endpoint %q, got %q", r.url, m.Endpoint)
	}
	// An undeliverable terminal frame is rewritten as a pending marker: it
	// must keep the endpoint too, or the next sweep could not tell whose it is.
	p.client = answerWith(http.StatusServiceUnavailable)
	p.Emit(Event{JobID: "agd-ep", Model: "agent-pool", Engine: "vllm", State: "completed", CreatedAt: 1_000, StartedAt: 2_000, CompletedAt: 3_000})
	p.Wait()
	if m := read(); !m.Pending || m.Endpoint != r.url {
		t.Fatalf("a pending marker must record the endpoint %q too: pending=%v endpoint=%q", r.url, m.Pending, m.Endpoint)
	}
}

// D3 (the structural twin of D1): a sweeper that is not the marker's own
// ingress must not post it, delete it, or lock it. A foreign endpoint is
// another PAIR (or a test's httptest server): closing the card there leaves the
// real card open and destroys the only record of it.
func TestForeignSweeperLeavesADefaultEndpointMarker(t *testing.T) {
	r := newOrphanRig(t)
	path := plantMarker(t, r.dir, "agd-real", DefaultEndpoint)
	s := r.emitter(dead, nil) // the rig's httptest ingress is not the default one
	if n := s.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("a foreign sweeper closed %d cards", n)
	}
	if r.pair.count() != 0 {
		t.Fatalf("a foreign sweeper posted %d frames to its own endpoint", r.pair.count())
	}
	if files := r.files(t); len(files) != 1 || files[0] != filepath.Base(path) {
		t.Fatalf("the marker must stay untouched (no delete, no lock): %v", files)
	}
}

// A foreign sweeper does not even age-drop another endpoint's marker: past the
// leak cap it is still not this sweeper's card.
func TestForeignSweeperDoesNotAgeDropAMarker(t *testing.T) {
	r := newOrphanRig(t)
	path := plantMarker(t, r.dir, "agd-old", DefaultEndpoint)
	s := r.emitter(dead, nil)
	later := time.Now().Add(openGiveUp + time.Hour)
	s.now = func() time.Time { return later }
	if n := s.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("closed %d", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a foreign sweeper dropped a marker that is not its own: %v", err)
	}
}

// A legacy marker (written before the register recorded endpoints) counts as
// the default endpoint: the default sweeper closes it, once, at that URL.
func TestDefaultSweeperClosesALegacyMarker(t *testing.T) {
	r := newOrphanRig(t)
	plantMarker(t, r.dir, "agd-legacy", "")
	s, urls := defaultEndpointSweeper(r)
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("the default-endpoint sweeper closed %d cards, want 1", n)
	}
	if len(*urls) != 1 || (*urls)[0] != DefaultEndpoint {
		t.Fatalf("close must go to the default ingress: %v", *urls)
	}
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("a closed marker must be gone: %v", files)
	}
}

// ... and a foreign sweeper leaves it, because a legacy marker is the default
// endpoint's, not the first sweeper's.
func TestForeignSweeperLeavesALegacyMarker(t *testing.T) {
	r := newOrphanRig(t)
	path := plantMarker(t, r.dir, "agd-legacy", "")
	s := r.emitter(dead, nil)
	if n := s.SweepOrphans(context.Background()); n != 0 || r.pair.count() != 0 {
		t.Fatalf("foreign sweeper closed %d / posted %d", n, r.pair.count())
	}
	if files := r.files(t); len(files) != 1 || files[0] != filepath.Base(path) {
		t.Fatalf("legacy marker must stay: %v", files)
	}
}

// The same ingress written two ways is one endpoint; two ingresses differing in
// scheme, host, port or path are two.
func TestSameEndpointNormalizes(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://127.0.0.1:14324/v1/workloads/events", "http://127.0.0.1:14324/v1/workloads/events", true},
		{"HTTP://LocalHost:80/x", "http://localhost/x", true},
		{"https://Example.test:443/x", "https://example.test/x", true},
		{"http://192.0.2.1:14324/a", "http://192.0.2.1:14325/a", false},
		{"http://192.0.2.1:14324/a", "http://192.0.2.1:14324/b", false},
		{"http://192.0.2.1:14324/a", "http://192.0.2.2:14324/a", false},
		{"http://192.0.2.1/a", "https://192.0.2.1/a", false},
	}
	for _, c := range cases {
		if got := sameEndpoint(c.a, c.b); got != c.want {
			t.Errorf("sameEndpoint(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// answerByID is an ingress that answers status[id] (200 when the job is not
// listed) and records the ids it was asked to close.
type answerByID struct {
	mu     sync.Mutex
	status map[string]int
	seen   []string
}

func (a *answerByID) handler(w http.ResponseWriter, r *http.Request) {
	var f map[string]any
	_ = json.NewDecoder(r.Body).Decode(&f)
	params, _ := f["params"].(map[string]any)
	info, _ := params["workloadInfo"].(map[string]any)
	id, _ := info["id"].(string)
	a.mu.Lock()
	a.seen = append(a.seen, id)
	code := a.status[id]
	a.mu.Unlock()
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
}

func (a *answerByID) ids() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// D4: one marker PAIR rejects (HTTP 4xx) must not starve the markers behind it.
// ReadDir order is by name, so the rejected job sorts FIRST.
func TestSweepContinuesPastARejectedMarker(t *testing.T) {
	r := newOrphanRig(t)
	ans := &answerByID{status: map[string]int{"agd-1-bad": http.StatusBadRequest}}
	srv := httptest.NewServer(http.HandlerFunc(ans.handler))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	for _, id := range []string{"agd-1-bad", "agd-2-ok", "agd-3-ok"} {
		plantMarker(t, r.dir, id, srv.URL)
	}
	logs := captureLogs(t)
	s := r.emitter(dead, nil)
	if n := s.SweepOrphans(context.Background()); n != 2 {
		t.Fatalf("the sweep closed %d cards, want the 2 behind the rejected one (seen %v)", n, ans.ids())
	}
	if got := strings.Join(ans.ids(), ","); got != "agd-1-bad,agd-2-ok,agd-3-ok" {
		t.Fatalf("every marker must be tried in the one pass, got %s", got)
	}
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("a rejected marker is dropped (PAIR will never accept it) and its lock released, left %v", files)
	}
	if out := logs.String(); !strings.Contains(out, "agd-1-bad") || !strings.Contains(out, "400") {
		t.Fatalf("the drop must be logged with the job id and the status: %q", out)
	}
}

// D4's other half: a failing PAIR (5xx) still ends the pass and keeps every
// marker, lock released, for the next sweep.
func TestSweepStillStopsWhenPairIsFailing(t *testing.T) {
	r := newOrphanRig(t)
	ans := &answerByID{status: map[string]int{"agd-1-down": http.StatusServiceUnavailable}}
	srv := httptest.NewServer(http.HandlerFunc(ans.handler))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	for _, id := range []string{"agd-1-down", "agd-2-ok"} {
		plantMarker(t, r.dir, id, srv.URL)
	}
	s := r.emitter(dead, nil)
	if n := s.SweepOrphans(context.Background()); n != 0 {
		t.Fatalf("closed %d against a failing PAIR", n)
	}
	if got := strings.Join(ans.ids(), ","); got != "agd-1-down" {
		t.Fatalf("a failing PAIR must end the pass at once, asked %s", got)
	}
	if files := r.files(t); len(files) != 2 {
		t.Fatalf("both markers stay, with no lock left behind: %v", files)
	}
}

// D4 for a live producer: its terminal frame rejected with 4xx is dropped with
// a log line, not parked as a pending marker that could never be delivered.
func TestRejectedTerminalFrameIsDroppedNotPended(t *testing.T) {
	r := newOrphanRig(t)
	logs := captureLogs(t)
	p := r.emitter(nil, nil)
	runningJob(p, "agd-rej")
	finish := func() {
		p.Emit(Event{JobID: "agd-rej", Model: "agent-pool", Engine: "vllm", State: "completed", CreatedAt: 1_000, StartedAt: 2_000, CompletedAt: 3_000})
		p.Wait()
	}
	p.client = answerWith(http.StatusBadRequest)
	finish()
	if files := r.files(t); len(files) != 0 {
		t.Fatalf("a terminal frame PAIR rejected must not stay as a pending marker: %v", files)
	}
	if out := logs.String(); !strings.Contains(out, "agd-rej") || !strings.Contains(out, "400") {
		t.Fatalf("the drop must be logged with the job id and the status: %q", out)
	}

	// Unreachable (5xx) is unchanged: the verdict is kept as a pending marker.
	runningJob(p, "agd-rej")
	p.client = answerWith(http.StatusServiceUnavailable)
	finish()
	files := r.files(t)
	if len(files) != 1 {
		t.Fatalf("an undeliverable (5xx) terminal frame must stay pending: %v", files)
	}
	raw, err := os.ReadFile(filepath.Join(r.dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	var m openMarker
	if json.Unmarshal(raw, &m) != nil || !m.Pending {
		t.Fatalf("marker must be pending: %s", raw)
	}
}

// Only a status that is a verdict on the frame is a rejection: 408 and 429 ask
// for a retry, and 5xx is PAIR failing, so none of them may drop a marker.
func TestRetryableClientStatusesAreNotRejections(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusBadRequest:          true,
		http.StatusUnprocessableEntity: true,
		http.StatusNotFound:            true,
		http.StatusRequestTimeout:      false,
		http.StatusTooManyRequests:     false,
		http.StatusInternalServerError: false,
		http.StatusServiceUnavailable:  false,
		http.StatusMovedPermanently:    false,
	} {
		if got := rejection(status); got != want {
			t.Errorf("rejection(%d) = %v, want %v", status, got, want)
		}
	}
}
