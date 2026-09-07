package fleetnode

// Scheduling bands, tenant round-robin, aging, the shed rule and the health
// saturation block (0.113.18, fleet-flow chapter L5 + L6, node side).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// orderedStore builds a one-worker store whose first job BLOCKS so every job
// admitted afterwards is pending at once; release() unblocks it and the claim
// order of the rest is recorded in `order`.
func orderedStore(t *testing.T) (j *Jobs, admit func(id string, band int, tenant string), release func(), order func() []string) {
	t.Helper()
	j = newJobs(time.Hour, time.Now, time.Hour, 1)
	t.Cleanup(func() { j.DrainAndStop(time.Second) })
	gate := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	if !j.Admit("blocker", AcceptSpec{}, func(ctx context.Context) (json.RawMessage, error) {
		<-gate
		return nil, nil
	}) {
		t.Fatal("blocker not admitted")
	}
	waitJobState(t, j, "blocker", JobRunning)
	admit = func(id string, band int, tenant string) {
		if !j.Admit(id, AcceptSpec{Band: band, Tenant: tenant}, func(ctx context.Context) (json.RawMessage, error) {
			mu.Lock()
			seen = append(seen, id)
			mu.Unlock()
			return nil, nil
		}) {
			t.Fatalf("%s not admitted", id)
		}
	}
	release = func() { close(gate) }
	order = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
	return j, admit, release, order
}

func waitAllDone(t *testing.T, j *Jobs, ids ...string) {
	t.Helper()
	for _, id := range ids {
		waitJobState(t, j, id, JobDone)
	}
}

// TestJobsClaimOrderIsBandThenTenantThenArrival pins the three keys at once:
// admitted in the order s1(-1,A) a1(0,A) a2(0,A) b1(0,B), the store claims
// a1 (band 0, A and B both unserved → arrival), then b1 (B is the tenant
// served least recently), then a2, and the sheddable s1 last.
func TestJobsClaimOrderIsBandThenTenantThenArrival(t *testing.T) {
	j, admit, release, order := orderedStore(t)
	admit("s1", BandSheddable, "A")
	admit("a1", BandNormal, "A")
	admit("a2", BandNormal, "A")
	admit("b1", BandNormal, "B")
	release()
	waitAllDone(t, j, "s1", "a1", "a2", "b1")
	want := []string{"a1", "b1", "a2", "s1"}
	if got := order(); !equalStrings(got, want) {
		t.Fatalf("claim order = %v, want %v", got, want)
	}
}

// TestJobsAnonymousTenantsKeepArrivalOrder is the compatibility arm: a fleet
// of older delegators stamps neither band nor tenant, and for them the store
// is exactly the FIFO it was.
func TestJobsAnonymousTenantsKeepArrivalOrder(t *testing.T) {
	j, admit, release, order := orderedStore(t)
	for _, id := range []string{"x1", "x2", "x3", "x4"} {
		admit(id, 0, "")
	}
	release()
	waitAllDone(t, j, "x1", "x2", "x3", "x4")
	want := []string{"x1", "x2", "x3", "x4"}
	if got := order(); !equalStrings(got, want) {
		t.Fatalf("claim order = %v, want arrival order %v", got, want)
	}
}

// TestJobsSheddableAgesIntoBandZero: with aging at zero a sheddable job that
// arrived first is claimed first (it counts as band 0 by then); the control
// arm with a one-hour aging keeps it behind the band-0 job.
func TestJobsSheddableAgesIntoBandZero(t *testing.T) {
	t.Run("aged", func(t *testing.T) {
		j, admit, release, order := orderedStore(t)
		j.mu.Lock()
		j.agingAfter = 0
		j.mu.Unlock()
		admit("s1", BandSheddable, "A")
		admit("a1", BandNormal, "B")
		release()
		waitAllDone(t, j, "s1", "a1")
		if got := order(); !equalStrings(got, []string{"s1", "a1"}) {
			t.Fatalf("claim order = %v, want the aged sheddable job first", got)
		}
	})
	t.Run("control", func(t *testing.T) {
		j, admit, release, order := orderedStore(t)
		admit("s1", BandSheddable, "A")
		admit("a1", BandNormal, "B")
		release()
		waitAllDone(t, j, "s1", "a1")
		if got := order(); !equalStrings(got, []string{"a1", "s1"}) {
			t.Fatalf("claim order = %v, want band 0 before an un-aged sheddable job", got)
		}
	})
}

// TestJobsIdleSlot: free worker + empty backlog = idle; a running job on a
// one-worker store is not; unlimited concurrency is idle whenever nothing waits.
func TestJobsIdleSlot(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour, 1)
	defer j.DrainAndStop(time.Second)
	if !j.IdleSlot() {
		t.Fatal("empty store must have an idle slot")
	}
	gate := make(chan struct{})
	j.Accept("r", func(ctx context.Context) (json.RawMessage, error) { <-gate; return nil, nil })
	waitJobState(t, j, "r", JobRunning)
	if j.IdleSlot() {
		t.Fatal("one worker, one running job: no idle slot")
	}
	close(gate)
	waitJobState(t, j, "r", JobDone)
	if !j.IdleSlot() {
		t.Fatal("slot must be idle again once the job is terminal")
	}
}

// TestDispatchSheddableIsShedWithoutAnIdleSlot is the shed rule with its
// control arm in one lifecycle: a busy one-worker node refuses a priority -1
// dispatch (503 "shed"), ADMITS a band-0 dispatch in the same state (it queues
// — busy is not full), and admits the sheddable one once the slot is idle.
//
// On the AGENT lane: the concurrency cap (and so the idle-slot predicate)
// governs the text seat's lane only — media task types are uncapped by design
// (concurrencyCapped), so a running image job leaves the slot idle.
func TestDispatchSheddableIsShedWithoutAnIdleSlot(t *testing.T) {
	release := make(chan struct{})
	s, _ := loopbackAgentServer(t, agentQueueCfg(t, 8, 1), &fakeRunner{fn: blockingAgentRun(release)})

	if rec := dispatchAgent(t, s, "busy-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("first dispatch = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "busy-1", JobRunning)
	shed := dispatchAgentWith(t, s, "shed-1", `"priority":-1`)
	wantErrorShape(t, shed, http.StatusServiceUnavailable, "shed (priority -1)")
	if rec := dispatchAgentWith(t, s, "norm-1", `"priority":0`); rec.Code != http.StatusAccepted {
		t.Fatalf("band-0 dispatch on a busy node = %d, want 202 (busy is not full)", rec.Code)
	}
	close(release)
	pollJob(t, s, "busy-1", JobDone)
	pollJob(t, s, "norm-1", JobDone)
	if rec := dispatchAgentWith(t, s, "shed-2", `"priority":-1`); rec.Code != http.StatusAccepted {
		t.Fatalf("sheddable dispatch on an idle node = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "shed-2", JobDone)
}

// blockingAgentRun parks every agent job until release closes and then
// answers with a minimal done wire.
func blockingAgentRun(release <-chan struct{}) func(ctx context.Context, req core.Request) core.Result {
	return func(ctx context.Context, req core.Request) core.Result {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return core.Result{OK: true, Data: json.RawMessage(`{"schema_version":1,"output":"ok","structured":{"answer":"ok"}}`)}
	}
}

// dispatchAgentWith is dispatchAgent with an extra envelope field (raw JSON).
func dispatchAgentWith(t *testing.T, s *Server, id, extra string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"job_id":"` + id + `","task_type":"agent",` + extra + `,"payload":` +
		`{"schema_version":1,"goal":"summarize","output_schema":{"properties":{"answer":{"type":"string"}}}}}`
	return do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
}

// TestDispatchPriorityIsLenient: the field was accepted-and-ignored before
// 0.113.18, so a non-integer value must still be band 0 (admitted), never 400.
func TestDispatchPriorityIsLenient(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	for i, raw := range []string{`"priority":"high"`, `"priority":null`, `"priority":2.5`, `"priority":{"x":1}`} {
		id := "len-" + string(rune('a'+i))
		if rec := dispatchImageWith(t, s, id, raw, nil); rec.Code != http.StatusAccepted {
			t.Fatalf("%s: dispatch = %d, want 202 (body %s)", raw, rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct {
		raw  string
		want int
	}{{``, BandNormal}, {`-9`, BandSheddable}, {`-1`, BandSheddable}, {`0`, BandNormal}, {`1`, BandUrgent}, {`42`, BandUrgent}, {`"x"`, BandNormal}} {
		if got := bandOf(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("bandOf(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestTenantHeaderIsSanitized: printable ASCII up to the cap survives;
// whitespace inside, control bytes and non-ASCII read as anonymous.
func TestTenantHeaderIsSanitized(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	for _, tc := range []struct{ in, want string }{
		{"session-A", "session-A"},
		{"  qube-1234-99  ", "qube-1234-99"},
		{"has space", ""},
		{"tab\there", ""},
		{"ünïcode", ""},
		{long, long[:maxTenantLen]},
		{"", ""},
	} {
		r := httptest.NewRequest(http.MethodPost, "/fleet/dispatch", nil)
		if tc.in != "" {
			r.Header.Set(TenantHeader, tc.in)
		}
		if got := tenantOf(r); got != tc.want {
			t.Errorf("tenantOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHealthSaturationTracksTheRefusalStates: idle → score 0, not high, idle
// slot; a running job on a one-worker node → score 1 (running/limit), not
// high (the backlog has room), no idle slot; backlog at its ceiling → high;
// a held text lease → high even on an idle node.
func TestHealthSaturationTracksTheRefusalStates(t *testing.T) {
	release := make(chan struct{})
	s, _ := loopbackAgentServer(t, agentQueueCfg(t, 2, 1), &fakeRunner{fn: blockingAgentRun(release)})
	sat := func() SaturationHealth {
		rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
		var p struct {
			Saturation *SaturationHealth `json:"saturation"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Saturation == nil {
			t.Fatalf("health saturation missing: %v %s", err, rec.Body.String())
		}
		return *p.Saturation
	}
	if got := sat(); got != (SaturationHealth{Score: 0, High: false, IdleSlot: true}) {
		t.Fatalf("idle saturation = %+v", got)
	}
	dispatchAgent(t, s, "s-1")
	pollJob(t, s, "s-1", JobRunning)
	if got := sat(); got != (SaturationHealth{Score: 1, High: false, IdleSlot: false}) {
		t.Fatalf("one running of one = %+v, want score 1, not high, no idle slot", got)
	}
	dispatchAgent(t, s, "s-2") // queued: depth 2 of 2
	if got := sat(); !got.High || got.IdleSlot {
		t.Fatalf("backlog at its ceiling = %+v, want high", got)
	}
	close(release)
	pollJob(t, s, "s-1", JobDone)
	pollJob(t, s, "s-2", JobDone)
	if got := sat(); got.High || !got.IdleSlot {
		t.Fatalf("after completion = %+v, want not high with an idle slot", got)
	}

	// Lease arm: an idle node under a text lease is high — dispatch would
	// refuse it — and the media class is not.
	for _, tc := range []struct {
		class gpulease.Class
		high  bool
	}{{gpulease.ClassText, true}, {gpulease.ClassMedia, false}} {
		info := gpulease.Info{Held: true, Class: tc.class, PID: 1, ExpiresAt: time.Now().Add(time.Minute)}
		ls, _ := newTestServer(t, imageCfg(), &fakeRunner{}, &Options{
			NodeID: "leased", Snapshot: goodSnapshot, Footprints: func() []FootprintEntry { return nil },
			Lease: func() gpulease.Info { return info },
		})
		rec := do(t, ls, http.MethodGet, "/fleet/health", "", nil)
		var p struct {
			Saturation *SaturationHealth `json:"saturation"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		if p.Saturation == nil || p.Saturation.High != tc.high {
			t.Fatalf("lease class %s: saturation = %+v, want high=%v", tc.class, p.Saturation, tc.high)
		}
	}
}



func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
