package rosterprobe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

type noRunner struct{}

func (noRunner) Run(context.Context, core.Request) core.Result { return core.Result{} }

// realNode serves the REAL fleet node handler, so the token probe is proved against the node's own
// ordering (bearer first, envelope after), not against a stand-in that merely agrees with it.
// loopback says whether the node believes its listener is loopback, the fact the 403 rests on.
func realNode(t *testing.T, token string, loopback bool) (*httptest.Server, *fleetnode.Jobs) {
	t.Helper()
	cfg := config.Config{FleetAuthToken: token, MediaDir: t.TempDir()}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	s := fleetnode.New(noRunner{}, jobs, fleetnode.Options{
		NodeID: "node-b", Cfg: cfg, GpuVendor: "nvidia", GpuArch: "ampere", LoopbackListener: loopback,
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Footprints: func() []fleetnode.FootprintEntry { return nil },
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, jobs
}

// The three answers a node can give a bearer, read through the real handler. Each is the node's own
// documented behaviour, and none of them creates a job.
func TestCheckTokenReadsTheRealNodesThreeAnswers(t *testing.T) {
	cases := []struct {
		name       string
		nodeToken  string
		loopback   bool
		bearer     string
		wantStatus int
		want       TokenState
	}{
		{"the node's token", "s3cret", false, "s3cret", http.StatusBadRequest, TokenAccepted},
		{"a different token", "s3cret", false, "other", http.StatusUnauthorized, TokenMismatch},
		{"no token sent", "s3cret", false, "", http.StatusUnauthorized, TokenMismatch},
		{"a node with no token beyond loopback", "", false, "s3cret", http.StatusForbidden, TokenNodeHasNone},
		{"a loopback node with no token", "", true, "s3cret", http.StatusBadRequest, TokenAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, jobs := realNode(t, tc.nodeToken, tc.loopback)
			status, body, err := CheckToken(context.Background(), srv.URL, tc.bearer)
			if err != nil {
				t.Fatal(err)
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %d (%s), want %d", status, body, tc.wantStatus)
			}
			if got := ClassifyToken(status, body); got != tc.want {
				t.Errorf("ClassifyToken(%d, %q) = %v, want %v", status, body, got, tc.want)
			}
			if q, r := jobs.Counts(); q+r != 0 {
				t.Errorf("the probe left %d job(s) on the node: it must create nothing", q+r)
			}
		})
	}
}

// Only the `job_id required` refusal proves the bearer passed. A node that predates the agent lane
// says `unsupported task_type`, a proxy may answer anything: none of those says the token is right.
func TestClassifyTokenIsConservative(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   TokenState
	}{
		{400, `{"error":"job_id required"}`, TokenAccepted},
		{400, `{"error":"unsupported task_type \"agent\""}`, TokenUnknown},
		{400, ``, TokenUnknown},
		{401, `{"error":"unauthorized"}`, TokenMismatch},
		{403, `{"error":"agent lane requires fleet_auth_token on a non-loopback listener"}`, TokenNodeHasNone},
		{404, ``, TokenUnknown},
		{500, `boom`, TokenUnknown},
		{200, `{}`, TokenUnknown},
		{202, `{}`, TokenUnknown},
	} {
		if got := ClassifyToken(tc.status, tc.body); got != tc.want {
			t.Errorf("ClassifyToken(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// The bearer is sensitive: it is never sent to a base the tailnet guard refuses. The refusal is a
// *Refusal, and no connection is made.
func TestCheckTokenNeverSendsTheBearerToARefusedBase(t *testing.T) {
	rostertest.Zones(t)
	_, _, err := CheckToken(context.Background(), "http://node-x.tailkkkkkk.ts.net:18811", "s3cret")
	if !IsRefusal(err) {
		t.Fatalf("err = %v, want a Refusal: the guard must judge the base before the bearer leaves", err)
	}
	_, _, err = CheckToken(context.Background(), "http://203.0.113.9:18811", "s3cret")
	if !IsRefusal(err) {
		t.Fatalf("a public literal: err = %v, want a Refusal", err)
	}
}

// A node that never answers is a transport error, bounded by the context, never a classification.
func TestCheckTokenReportsATransportFailure(t *testing.T) {
	hole := rostertest.NewBlackHole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	status, body, err := CheckToken(ctx, hole.URL(), "s3cret")
	if err == nil {
		t.Fatalf("a black hole answered: %d %q", status, body)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error %q carries the bearer", err)
	}
}

// The bearer rides the Authorization header only: a fake node that records what it was sent sees the
// token there and nowhere in the URL.
func TestCheckTokenSendsTheBearerOnlyInTheHeader(t *testing.T) {
	var gotAuth, gotPath, gotBody, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotType = r.Header.Get("Authorization"), r.URL.String(), r.Header.Get("Content-Type")
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"job_id required"}`))
	}))
	defer srv.Close()
	if _, _, err := CheckToken(context.Background(), srv.URL+"/", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer s3cret" || gotPath != "/fleet/dispatch" || gotType != "application/json" {
		t.Errorf("request = auth %q, url %q, type %q", gotAuth, gotPath, gotType)
	}
	if gotBody != `{"task_type":"agent"}` {
		t.Errorf("body = %q: the probe is an agent envelope with no job_id", gotBody)
	}
}
