package fleetnode

// /fleet/health and the jobs feed are readable without the bearer on purpose, so a caller that sends none is not refused
// and the journal stays quiet. A caller that PRESENTS a bearer which is not the node's is trying a credential, and gets
// the same throttled journal line every other door writes for it, while still being answered with the view a caller
// without the bearer gets. The line is the usual one (door, caller, reason), shares the usual throttle and never
// carries the value presented.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/storesteward"
)

type publicRoute struct {
	name, label, path string
	masked            func(t *testing.T, rec *httptest.ResponseRecorder) // the view a caller without the bearer gets
}

var publicRoutes = []publicRoute{
	{"health", "health", "/fleet/health", func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("health status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if _, has := decodeMap(t, rec)["store"]; has {
			t.Errorf("a caller without the bearer was shown the store: %s", rec.Body.String())
		}
	}},
	{"jobs feed", "jobs-feed", "/fleet/jobs?limit=5", func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("jobs feed status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "frame-0007") {
			t.Errorf("a caller without the bearer was shown a job's error text: %s", rec.Body.String())
		}
	}},
}

// publicRouteServer is a node with a token beyond loopback that holds a failed job whose error names a path and has a
// store steward, so the masked view differs from the full one on both routes.
func publicRouteServer(t *testing.T) *Server {
	t.Helper()
	r := &fakeRunner{fn: func(context.Context, core.Request) core.Result {
		return core.Result{OK: false, Reason: "render failed writing /srv/node-render/work/pub-1/frame-0007.png"}
	}}
	opts := authOpts(false)
	opts.Store = func() storesteward.Status {
		return storesteward.Status{Root: healthStoreRoot, UsedGB: 61.2, CapGB: 100, Files: 1203}
	}
	s, _ := newTestServer(t, tokenCfg(holderKey), r, opts)
	hold := map[string]string{"Authorization": "Bearer " + holderKey}
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"pub-1","task_type":"image-gen","payload":{"prompt":"hi"}}`, hold)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("setup: dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if m := pollDone(t, s, "pub-1", hold); m["state"] != "error" {
		t.Fatalf("setup: the job did not fail: %v", m)
	}
	return s
}

func getFrom(s *Server, path, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func journalRefusals(logged string) []string {
	var out []string
	for _, l := range strings.Split(logged, "\n") {
		if strings.HasPrefix(l, refusalLinePrefix) {
			out = append(out, l)
		}
	}
	return out
}

// A wrong bearer writes one line, in the usual shape, and the route still answers the masked view.
func TestAWrongBearerOnAPublicRouteWritesOneJournalLineAndIsStillAnswered(t *testing.T) {
	for _, rt := range publicRoutes {
		t.Run(rt.name, func(t *testing.T) {
			s := publicRouteServer(t)
			buf := captureLog(t)
			rec := getFrom(s, rt.path, "192.0.2.50:5000", map[string]string{"Authorization": "Bearer " + wrongKey})
			rt.masked(t, rec)
			lines := journalRefusals(buf.String())
			if len(lines) != 1 {
				t.Fatalf("a wrong bearer wrote %d refusal line(s), want 1; log = %q", len(lines), buf.String())
			}
			if !strings.Contains(lines[0], refusedWrongBearer) || !refusalLineRE(rt.label, "192.0.2.50").MatchString(lines[0]) {
				t.Fatalf("the line is not the usual one for door %q and a wrong bearer: %q", rt.label, lines[0])
			}
			assertCredentialNotCopied(t, buf.String(), rec.Body.String())
			if strings.Contains(buf.String(), wrongKey) {
				t.Fatalf("the journal carries the value the caller presented: %q", buf.String())
			}
		})
	}
}

// A request with no bearer at all is what these routes are open for: it writes nothing. Neither does the right bearer.
func TestNoBearerOrTheRightBearerOnAPublicRouteWritesNothing(t *testing.T) {
	for _, rt := range publicRoutes {
		for name, hdr := range map[string]map[string]string{
			"no bearer":        nil,
			"the right bearer": {"Authorization": "Bearer " + holderKey},
		} {
			t.Run(rt.name+" with "+name, func(t *testing.T) {
				s := publicRouteServer(t)
				buf := captureLog(t)
				if rec := getFrom(s, rt.path, "192.0.2.50:5000", hdr); rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
				}
				if lines := journalRefusals(buf.String()); len(lines) != 0 {
					t.Fatalf("wrote %d refusal line(s), want none: %q", len(lines), lines)
				}
				if n := refusalsNoted(s); n != 0 {
					t.Fatalf("told the journal of %d refusal(s), want none", n)
				}
			})
		}
	}
}

// A node with no token has nothing for a bearer to be wrong about.
func TestABearerPresentedToANodeWithNoTokenIsNotARefusal(t *testing.T) {
	for _, rt := range publicRoutes {
		t.Run(rt.name, func(t *testing.T) {
			s, _ := newTestServer(t, tokenCfg(""), &fakeRunner{}, healthOpts(true))
			buf := captureLog(t)
			if rec := getFrom(s, rt.path, "127.0.0.1:5000", map[string]string{"Authorization": "Bearer " + wrongKey}); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			if lines := journalRefusals(buf.String()); len(lines) != 0 {
				t.Fatalf("wrote %d refusal line(s), want none: %q", len(lines), lines)
			}
		})
	}
}

// Guessing at these routes costs the journal one line per caller and reason in a window, however many guesses and on
// whichever of the doors, and the next window writes one more.
func TestWrongBearersOnThePublicRoutesShareTheJournalThrottle(t *testing.T) {
	s := publicRouteServer(t)
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.refusals.now = func() time.Time { return clock }
	buf := captureLog(t)
	guess := func(i int) {
		for _, rt := range publicRoutes {
			getFrom(s, rt.path, "192.0.2.50:5000", map[string]string{"Authorization": "Bearer " + wrongKey + string(rune('a'+i))})
		}
		req := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", strings.NewReader(`{"job_id":"pub-2","task_type":"image-gen","payload":{}}`))
		req.RemoteAddr = "192.0.2.50:5000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+wrongKey)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	for i := 0; i < 20; i++ {
		guess(i)
	}
	lines := journalRefusals(buf.String())
	if len(lines) != 1 {
		t.Fatalf("20 rounds of guesses at the public routes and a door wrote %d lines, want 1; log = %q", len(lines), buf.String())
	}
	if !refusalLineRE("health", "192.0.2.50").MatchString(lines[0]) {
		t.Fatalf("the one line is not the first guess, at health: %q", lines[0])
	}
	clock = clock.Add(refusalLogEvery + time.Second)
	guess(0)
	if n := len(journalRefusals(buf.String())); n != 2 {
		t.Fatalf("the first guess after the interval wrote %d lines in all, want 2", n)
	}
}
