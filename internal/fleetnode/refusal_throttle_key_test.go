package fleetnode

// The refusal journal is throttled by who is asking and why, and by nothing the caller chooses: a caller with no
// credential must not be able to spend the whole window's budget by changing a header on each request, because the
// lines it spends are the lines other callers' refusals would have used. The tests that vary the task type, the door
// and the address are in refusal_log_test.go and refusal_every_door_test.go; this one varies the headers and the rest
// of the request from one address and one reason, and requires one line.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTheRefusalThrottleIgnoresEveryFieldTheCallerChooses(t *testing.T) {
	const requests = 3 * refusalLogBudget // more than one window's budget, so a key that follows the field shows
	for _, tc := range []struct {
		name string
		vary func(i int, r *http.Request)
	}{
		{"User-Agent", func(i int, r *http.Request) { r.Header.Set("User-Agent", fmt.Sprintf("agent-%d", i)) }},
		{"presented bearer", func(i int, r *http.Request) { r.Header.Set("Authorization", fmt.Sprintf("Bearer wrong-%d", i)) }},
		{"presented scheme", func(i int, r *http.Request) { r.Header.Set("Authorization", fmt.Sprintf("Basic wrong-%d", i)) }},
		{"X-Forwarded-For", func(i int, r *http.Request) { r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i)) }},
		{"Forwarded", func(i int, r *http.Request) { r.Header.Set("Forwarded", fmt.Sprintf("for=198.51.100.%d", i)) }},
		{"Origin", func(i int, r *http.Request) { r.Header.Set("Origin", fmt.Sprintf("https://site-%d.example.test", i)) }},
		{"Referer", func(i int, r *http.Request) { r.Header.Set("Referer", fmt.Sprintf("https://site-%d.example.test/", i)) }},
		{"Host", func(i int, r *http.Request) { r.Host = fmt.Sprintf("name-%d.example.test", i) }},
		{"Content-Type", func(i int, r *http.Request) { r.Header.Set("Content-Type", fmt.Sprintf("text/x-%d", i)) }},
		{"query string", func(i int, r *http.Request) { r.URL.RawQuery = fmt.Sprintf("q=%d", i) }},
		{"source port", func(i int, r *http.Request) { r.RemoteAddr = fmt.Sprintf("192.0.2.10:%d", 4000+i) }},
		{"job id and task type in the body", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t, tokenCfg(holderKey), &fakeRunner{}, authOpts(false))
			clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			s.refusals.now = func() time.Time { return clock }
			buf := captureLog(t)
			for i := 0; i < requests; i++ {
				body := `{"job_id":"j","task_type":"image-gen","payload":{}}`
				if tc.vary == nil {
					body = fmt.Sprintf(`{"job_id":"j%d","task_type":"t%d","payload":{}}`, i, i)
				}
				req := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", strings.NewReader(body))
				req.RemoteAddr = "192.0.2.10:4000"
				if tc.vary != nil {
					tc.vary(i, req)
				}
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("request %d: status = %d, want 401 (body %s)", i, rec.Code, rec.Body.String())
				}
			}
			if n := refusalLines(buf.String()); n != 1 {
				t.Fatalf("one caller, one reason, %d requests varying only the %s wrote %d journal lines, want 1; log = %q", requests, tc.name, n, buf.String())
			}
			if got := refusalsNoted(s); got != requests {
				t.Fatalf("the journal counted %d refusals, want %d (every one is noted, one is written)", got, requests)
			}
		})
	}
}

// refusalsNoted is how many refusals the server's journal has been told of in the current window: the ones it wrote and
// the ones it kept out.
func refusalsNoted(s *Server) int {
	s.refusals.mu.Lock()
	defer s.refusals.mu.Unlock()
	return s.refusals.written + s.refusals.skipped
}
