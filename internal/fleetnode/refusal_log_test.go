package fleetnode

// A node that refuses a request without the bearer says so in its journal, but the journal is the one thing a
// caller with no credential must not be able to grow without bound. The log is throttled by who is asking and why
// it was refused, never by a field the caller chooses (the task type is the caller's text), and the lines written
// in a window are capped across callers, with one summary line for the rest.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// refusalLines counts the lines a node wrote for refused requests.
func refusalLines(logged string) int {
	n := 0
	for _, l := range strings.Split(logged, "\n") {
		if strings.Contains(l, "refused a ") {
			n++
		}
	}
	return n
}

func TestTheRefusalLogDoesNotGrowWithTheTaskTypeACallerChooses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"no bearer presented to a tokened node", "s3cret"},
		{"a non-loopback node with no token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t, tokenCfg(tc.token), &fakeRunner{}, authOpts(false))
			buf := captureLog(t)
			for i := 0; i < 1000; i++ {
				do(t, s, http.MethodPost, "/fleet/dispatch", fmt.Sprintf(`{"job_id":"j%d","task_type":"t%d","payload":{}}`, i, i), nil)
			}
			n := refusalLines(buf.String())
			if n == 0 {
				t.Fatal("a node under probing wrote no line at all")
			}
			if n > 40 {
				t.Fatalf("1000 refusals from one caller varying only the task type wrote %d lines (%d bytes); the log must be bounded", n, len(buf.String()))
			}
		})
	}
}

func TestTheRefusalLogCountsWhatItDidNotWriteAndSaysSoOnce(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, authOpts(false))
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.refusals.now = func() time.Time { return clock }
	buf := captureLog(t)
	for i := 0; i < 300; i++ {
		do(t, s, http.MethodPost, "/fleet/dispatch", fmt.Sprintf(`{"job_id":"j%d","task_type":"t%d","payload":{}}`, i, i), nil)
	}
	before := refusalLines(buf.String())
	if before == 0 || before > 40 {
		t.Fatalf("the burst wrote %d lines, want a few", before)
	}
	clock = clock.Add(3 * time.Minute)
	do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"later","task_type":"later","payload":{}}`, nil)
	logged := buf.String()
	if !strings.Contains(logged, "not logged") {
		t.Fatalf("the window ended with refusals unwritten and no summary line; log = %q", logged)
	}
	if strings.Contains(logged, "s3cret") {
		t.Fatalf("the log carries the token: %q", logged)
	}
}

func TestTheRefusalLogStillSpeaksForEachCaller(t *testing.T) {
	var l refusalLog
	buf := captureLog(t)
	note := func(addr, taskType, why string) {
		r := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", nil)
		r.RemoteAddr = addr
		l.note(r, taskType, why)
	}
	note("192.0.2.10:5000", "run-graph", "no fleet bearer presented")
	note("192.0.2.10:5001", "image-gen", "no fleet bearer presented") // same caller, same reason: not again
	note("192.0.2.11:5000", "run-graph", "no fleet bearer presented") // another caller
	note("192.0.2.10:5002", "run-graph", "the fleet bearer presented is not this node's")
	if n := refusalLines(buf.String()); n != 3 {
		t.Fatalf("wrote %d lines for 3 distinct (caller, reason) pairs, want 3; log = %q", n, buf.String())
	}
}

// Many callers cannot grow the journal either: at most refusalLogBudget lines are written a window across all of them, and
// the first refusal after the window counts the rest.
func TestTheRefusalLogCapsWhatManyCallersWrite(t *testing.T) {
	var l refusalLog
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	l.now = func() time.Time { return clock }
	buf := captureLog(t)
	note := func(addr string) {
		r := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", nil)
		r.RemoteAddr = addr
		l.note(r, "run-graph", "no fleet bearer presented")
	}
	for i := 0; i < 250; i++ {
		note(fmt.Sprintf("192.0.2.%d:5000", i))
	}
	if n := refusalLines(buf.String()); n == 0 || n > refusalLogBudget {
		t.Fatalf("250 callers wrote %d lines in one window, want between 1 and %d", n, refusalLogBudget)
	}
	clock = clock.Add(2 * refusalLogWindow)
	note("198.51.100.7:5000")
	logged := buf.String()
	if !strings.Contains(logged, fmt.Sprintf("%d more refusal(s) were not logged one by one", 250-refusalLogBudget)) {
		t.Fatalf("the window ended with %d refusals unwritten and no summary naming that count; log = %q", 250-refusalLogBudget, logged)
	}
}
