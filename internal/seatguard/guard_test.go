package seatguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

const seat3 = "qwen3.8-27b-vllm-3card"

// fakeRunning is this box's llama-swap reduced to what the guard reads: GET
// /running, whose rows the test sets, or a 500 when broken. Every other path
// is a test failure — the guard must never touch a seat or an /upstream path.
type fakeRunning struct {
	srv    *httptest.Server
	mu     sync.Mutex
	rows   []string // "<id>:<state>"
	broken bool
	// jsonDown answers 503 with a JSON body that decodes as "nothing
	// running" — what a restarting llama-swap can send.
	jsonDown bool
	hits     atomic.Int64
}

func newFakeRunning(t *testing.T, rows ...string) *fakeRunning {
	t.Helper()
	f := &fakeRunning{rows: rows}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/running" {
			t.Errorf("the guard requested %s; it may read /running only", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		f.hits.Add(1)
		f.mu.Lock()
		broken, rows := f.broken, append([]string(nil), f.rows...)
		f.mu.Unlock()
		if broken {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		if f.jsonDown {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"running": nil})
			return
		}
		parts := make([]map[string]string, 0, len(rows))
		for _, e := range rows {
			id, state, _ := strings.Cut(e, ":")
			parts = append(parts, map[string]string{"model": id, "state": state, "proxy": "http://127.0.0.1:1"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": parts})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRunning) set(broken bool, rows ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broken, f.rows = broken, rows
}

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func guardCfg(endpoint, servingConfig string, seats ...string) config.Config {
	cfg := config.Default()
	cfg.Endpoint = endpoint
	cfg.ServingConfigPath = servingConfig
	cfg.VLLMSeats = seats
	return cfg
}

// testClock is a settable clock for the TTL and staleness windows.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestGuard(t *testing.T, cfg config.Config) (*Guard, *testClock) {
	t.Helper()
	g := New(cfg)
	if g == nil {
		t.Fatal("New returned nil for a config that declares a vLLM seat")
	}
	clk := &testClock{t: time.Unix(1_800_000_000, 0)}
	g.now = clk.now
	return g, clk
}

// TestGuardIsInertWithoutASeatToProtect: no declared vLLM seat, or the flag
// off, builds no guard at all, and a nil guard answers "clear" for everything
// — the cascade is byte-identical to a build without the guard.
func TestGuardIsInertWithoutASeatToProtect(t *testing.T) {
	f := newFakeRunning(t, seat3+":ready")
	if g := New(guardCfg(f.srv.URL, "" /* no seats */)); g != nil {
		t.Fatal("a box that declares no vllm_seats must get no guard")
	}
	off := guardCfg(f.srv.URL, "", seat3)
	no := false
	off.CascadeSeatGuard = &no
	if g := New(off); g != nil {
		t.Fatal("cascade_seat_guard=false must build no guard")
	}
	var g *Guard
	if v := g.Check(context.Background(), "gemma-4-e4b"); v.Protect || v.Seat != "" || len(v.Seats) != 0 || v.Stale || v.Reason != "" {
		t.Fatalf("a nil guard answered %+v, want the zero verdict", v)
	}
	if n := f.hits.Load(); n != 0 {
		t.Fatalf("an inert guard read /running %d time(s)", n)
	}
}

// TestGuardClearWhenNoVLLMSeatIsLoaded is the control: nothing to protect,
// nothing changes — even with a cascade rung already loaded.
func TestGuardClearWhenNoVLLMSeatIsLoaded(t *testing.T) {
	f := newFakeRunning(t, "embeddinggemma:ready")
	g, clk := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	if v := g.Check(context.Background(), "gemma-4-e4b"); v.Protect {
		t.Fatalf("no seat loaded: verdict %+v, want clear", v)
	}
	f.set(false, "embeddinggemma:ready", "gemma-4-e2b:ready")
	clk.advance(SnapshotTTL)
	if v := g.Check(context.Background(), "gemma-4-e4b"); v.Protect {
		t.Fatalf("a rung loaded, no seat: verdict %+v, want clear", v)
	}
}

// TestGuardProtectsALoadedSeat is the measured case: the three-card seat is
// loaded beside the embedder and a rung in its exclusive set is asked for.
func TestGuardProtectsALoadedSeat(t *testing.T) {
	f := newFakeRunning(t, "embeddinggemma:ready", seat3+":ready")
	g, clk := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	v := g.Check(context.Background(), "gemma-4-e4b")
	if !v.Protect || v.Seat != seat3 || v.Stale {
		t.Fatalf("verdict %+v, want Protect of %s from a fresh reading", v, seat3)
	}
	for _, want := range []string{"model=gemma-4-e4b", "set=interactive", "evict=[" + seat3 + "]"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason %q lacks %q", v.Reason, want)
		}
	}
	// A seat that is still loading is protected too: evicting it throws the
	// load away. A seat already leaving is not.
	f.set(false, "embeddinggemma:ready", seat3+":starting")
	clk.advance(SnapshotTTL)
	if v := g.Check(context.Background(), "offload-e4b"); !v.Protect {
		t.Fatalf("starting seat: verdict %+v, want Protect", v)
	}
	f.set(false, "embeddinggemma:ready", seat3+":stopping")
	clk.advance(SnapshotTTL)
	if v := g.Check(context.Background(), "gemma-4-e4b"); v.Protect {
		t.Fatalf("stopping seat: verdict %+v, want clear", v)
	}
}

// TestGuardLetsTheSeatAndItsNeighboursThrough: asking for the seat itself
// (by id or alias), or for a model a set runs BESIDE it, evicts nothing.
func TestGuardLetsTheSeatAndItsNeighboursThrough(t *testing.T) {
	f := newFakeRunning(t, "embeddinggemma:ready", seat3+":ready")
	g, _ := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	for _, m := range []string{seat3, "agent-pool", "gemma-4-e4b-display", "embeddinggemma"} {
		if v := g.Check(context.Background(), m); v.Protect {
			t.Errorf("%s: verdict %+v, want clear", m, v)
		}
	}
}

// TestGuardSeatDeclaredByAlias: vllm_seats may name a seat by an alias; the
// config resolves it, and the verdict names the seat as DECLARED, so the
// pipeline's vLLM-seat check (DeclaresVLLMSeat) recognises the substitute.
func TestGuardSeatDeclaredByAlias(t *testing.T) {
	f := newFakeRunning(t, seat3+":ready")
	g, _ := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), "agent-pool"))
	if v := g.Check(context.Background(), "gemma-4-12b"); !v.Protect || v.Seat != "agent-pool" {
		t.Fatalf("verdict %+v, want Protect naming the declared seat agent-pool", v)
	}
}

// TestGuardStaleReadingFailsTowardTheSeat: once /running stops answering,
// the last reading that saw the seat loaded keeps protecting it for
// StaleMax (a loaded seat that has not been seen to leave may still be
// there); past that, and with no reading at all, the state is UNKNOWN and
// still reads as Protect — with no seat to name, so the caller can route
// the call elsewhere but has nothing to substitute.
func TestGuardStaleReadingFailsTowardTheSeat(t *testing.T) {
	f := newFakeRunning(t, "embeddinggemma:ready", seat3+":ready")
	g, clk := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	if v := g.Check(context.Background(), "gemma-4-e4b"); !v.Protect || v.Stale {
		t.Fatalf("fresh: %+v", v)
	}
	f.set(true)
	clk.advance(2 * SnapshotTTL)
	v := g.Check(context.Background(), "gemma-4-e4b")
	if !v.Protect || !v.Stale || v.Seat != seat3 {
		t.Fatalf("stale inside StaleMax: verdict %+v, want Protect of %s marked stale", v, seat3)
	}
	// Staleness never manufactures an eviction the config rules out.
	if v := g.Check(context.Background(), "gemma-4-e4b-display"); v.Protect {
		t.Fatalf("stale, coexisting twin: verdict %+v, want clear", v)
	}
	clk.advance(StaleMax)
	v = g.Check(context.Background(), "gemma-4-e4b")
	if !v.Protect || !v.Stale || v.Seat != "" {
		t.Fatalf("stale past StaleMax: verdict %+v, want Protect with no seat named", v)
	}
}

// TestGuardUnknownFromTheStartProtects: a guard that has never read the box
// cannot say the seat is absent, so it does not say "clear".
func TestGuardUnknownFromTheStartProtects(t *testing.T) {
	f := newFakeRunning(t)
	f.set(true)
	g, _ := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	v := g.Check(context.Background(), "gemma-4-e4b")
	if !v.Protect || !v.Stale || v.Seat != "" || !strings.Contains(v.Reason, "could not be read") {
		t.Fatalf("verdict %+v, want an unknown Protect naming no seat", v)
	}
}

// TestGuardUnreadableConfigTreatsCoResidencyAsUnknown: with the seat known
// loaded but the serving config missing, co-residency cannot be derived, and
// the guard fails toward the seat rather than toward the rung.
func TestGuardUnreadableConfigTreatsCoResidencyAsUnknown(t *testing.T) {
	f := newFakeRunning(t, seat3+":ready")
	for name, path := range map[string]string{
		"unset":   "",
		"missing": filepath.Join(t.TempDir(), "nope.yaml"),
		"broken":  writeConfig(t, "models: [\n"),
	} {
		g, _ := newTestGuard(t, guardCfg(f.srv.URL, path, seat3))
		v := g.Check(context.Background(), "gemma-4-e4b")
		if !v.Protect || v.Seat != seat3 || !strings.Contains(v.Reason, "co-residency is unknown") {
			t.Errorf("%s config: verdict %+v, want Protect of the seat with co-residency unknown", name, v)
		}
	}
}

// TestGuardFollowsTheConfigWhenItChanges: the rule is the operator's file,
// re-read when it changes — a set added that runs the rung beside the seat
// makes the rung clear without a restart.
func TestGuardFollowsTheConfigWhenItChanges(t *testing.T) {
	f := newFakeRunning(t, seat3+":ready")
	path := writeConfig(t, matrixYAML)
	g, clk := newTestGuard(t, guardCfg(f.srv.URL, path, seat3))
	if v := g.Check(context.Background(), "gemma-4-e4b"); !v.Protect {
		t.Fatalf("before: %+v", v)
	}
	widened := strings.Replace(matrixYAML, `display: "+residents & q38v3 & ge4d"`, `display: "+residents & q38v3 & (ge4d | ge4)"`, 1)
	if err := os.WriteFile(path, []byte(widened), 0o644); err != nil {
		t.Fatal(err)
	}
	clk.advance(SnapshotTTL)
	if v := g.Check(context.Background(), "gemma-4-e4b"); v.Protect {
		t.Fatalf("after the operator let e4b run beside the seat: %+v, want clear", v)
	}
}

// TestGuardReadsOncePerWindow: a burst of cascade calls costs one /running
// read per SnapshotTTL, not one per rung per call.
func TestGuardReadsOncePerWindow(t *testing.T) {
	f := newFakeRunning(t, seat3+":ready")
	g, clk := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	for i := 0; i < 20; i++ {
		g.Check(context.Background(), "gemma-4-e4b")
	}
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("20 checks in one window read /running %d times, want 1", n)
	}
	clk.advance(SnapshotTTL)
	g.Check(context.Background(), "gemma-4-e4b")
	if n := f.hits.Load(); n != 2 {
		t.Fatalf("after the window: %d reads, want 2", n)
	}
}

// TestSharedIsOnePerIdentity: every pipeline in a process (the MCP server
// builds one per contract for the in-loop offload) shares one reading.
func TestSharedIsOnePerIdentity(t *testing.T) {
	a := guardCfg("http://127.0.0.1:1", "x.yaml", seat3)
	b := guardCfg("http://127.0.0.1:1", "x.yaml", seat3)
	if Shared(a) == nil || Shared(a) != Shared(b) {
		t.Fatal("two identical configs must share one guard")
	}
	c := guardCfg("http://127.0.0.1:2", "x.yaml", seat3)
	if Shared(c) == Shared(a) {
		t.Fatal("a different endpoint must get its own guard")
	}
	if Shared(guardCfg("http://127.0.0.1:1", "x.yaml")) != nil {
		t.Fatal("no declared seat: Shared must be nil")
	}
}

// TestGuardA503WithAJSONBodyIsUnknownNotEmpty (review CRITICAL 1): a 503
// whose body decodes as "nothing running" must take the fail-safe path —
// the last reading that saw the seat keeps protecting it — never read as a
// box with no seat loaded.
func TestGuardA503WithAJSONBodyIsUnknownNotEmpty(t *testing.T) {
	f := newFakeRunning(t, "embeddinggemma:ready", seat3+":ready")
	g, clk := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	if v := g.Check(context.Background(), "gemma-4-e4b"); !v.Protect || v.Stale {
		t.Fatalf("fresh: %+v", v)
	}
	f.mu.Lock()
	f.jsonDown = true
	f.mu.Unlock()
	clk.advance(SnapshotTTL)
	v := g.Check(context.Background(), "gemma-4-e4b")
	if !v.Protect || !v.Stale || v.Seat != seat3 {
		t.Fatalf("503 {\"running\":null}: verdict %+v, want the stale Protect of %s", v, seat3)
	}
	// And with no earlier reading at all: unknown, never clear.
	g2, _ := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	if v := g2.Check(context.Background(), "gemma-4-e4b"); !v.Protect || !v.Stale {
		t.Fatalf("503 from the start: verdict %+v, want an unknown Protect", v)
	}
}

// TestGuardWarnShowsAChangedReason (review MEDIUM 4): the warning is
// rate-limited per cause, but a cause whose error TEXT changes must still say
// so — "connection refused" then "status 503" are two different faults, and
// the second one used to be swallowed for a minute. The same text repeated
// inside the window is still one line.
func TestGuardWarnShowsAChangedReason(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	g, clk := newTestGuard(t, guardCfg("http://127.0.0.1:1", writeConfig(t, matrixYAML), seat3))
	errs := []error{errors.New("connection refused"), errors.New("connection refused"), errors.New("status 503")}
	i := 0
	g.readRunning = func(context.Context) ([]seatload.Occupant, error) {
		e := errs[i]
		i++
		return nil, e
	}
	for range errs {
		g.Check(context.Background(), "gemma-4-e4b")
		clk.advance(SnapshotTTL) // inside warnEvery (1 min) every time
	}
	out := buf.String()
	if n := strings.Count(out, "connection refused"); n != 1 {
		t.Errorf("the repeated error was logged %d times inside the window, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "status 503") {
		t.Errorf("the changed error was swallowed by the rate limit:\n%s", out)
	}
}

// TestGuardNamesEverySeatItProtects (review MEDIUM 5): with two vLLM seats
// loaded and co-residency unknown (no serving config), the verdict and its log
// line name BOTH — naming only the first hid the second seat at risk. Seat is
// still the one the pipeline substitutes; Seats is the full list.
func TestGuardNamesEverySeatItProtects(t *testing.T) {
	const pair = "qwen3.8-27b-vllm"
	f := newFakeRunning(t, "embeddinggemma:ready", pair+":ready", seat3+":ready")
	g, _ := newTestGuard(t, guardCfg(f.srv.URL, "", pair, seat3))
	v := g.Check(context.Background(), "gemma-4-e4b")
	if !v.Protect || v.Seat != pair {
		t.Fatalf("verdict %+v, want Protect with %s as the substitute", v, pair)
	}
	if strings.Join(v.Seats, ",") != pair+","+seat3 {
		t.Errorf("Seats = %v, want both loaded seats", v.Seats)
	}
	if !strings.Contains(v.Reason, "evict=["+pair+" "+seat3+"]") {
		t.Errorf("reason %q does not name both seats", v.Reason)
	}
	// Known co-residency: every loaded seat the solver evicts is named too.
	g2, _ := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), pair, seat3))
	if v := g2.Check(context.Background(), "gemma-4-e4b"); strings.Join(v.Seats, ",") != pair+","+seat3 {
		t.Errorf("known co-residency: Seats = %v, want both evicted seats", v.Seats)
	}
}

// TestGuardWarnsOnceAboutAModelInNoSet (review NOTE): a model in no matrix
// set runs alone in llama-swap and evicts everything; the guard protects the
// seat from it and says, once, that the config is why.
func TestGuardWarnsOnceAboutAModelInNoSet(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	f := newFakeRunning(t, "embeddinggemma:ready", seat3+":ready")
	g, clk := newTestGuard(t, guardCfg(f.srv.URL, writeConfig(t, matrixYAML), seat3))
	for i := 0; i < 3; i++ {
		if v := g.Check(context.Background(), "loner"); !v.Protect || v.Seat != seat3 {
			t.Fatalf("verdict %+v, want Protect: a model in no set evicts the seat", v)
		}
		clk.advance(2 * warnEvery)
	}
	if n := strings.Count(buf.String(), "loner appears in no matrix set"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, buf.String())
	}
}

// TestSharedHitBuildsNothing (review round 2, item 2): Shared is called once
// per contract (every pipeline the MCP server builds), so a cache HIT must not
// construct a Guard — and its http.Client — only to throw it away. One build on
// the first call, none after, and the same pointer every time.
func TestSharedHitBuildsNothing(t *testing.T) {
	cfg := guardCfg("http://127.0.0.1:59"+fmt.Sprint(time.Now().UnixNano()%1000), "hit.yaml", seat3)
	before := constructed.Load()
	first := Shared(cfg)
	if first == nil {
		t.Fatal("Shared returned nil for a guardable config")
	}
	afterMiss := constructed.Load()
	for i := 0; i < 5; i++ {
		if g := Shared(cfg); g != first {
			t.Fatal("a hit returned a different guard")
		}
	}
	if n := afterMiss - before; n != 1 {
		t.Errorf("the first (missing) call built %d guards, want 1", n)
	}
	if n := constructed.Load() - afterMiss; n != 0 {
		t.Errorf("5 cache hits built %d guards, want 0", n)
	}
	// An inert config builds nothing either.
	mark := constructed.Load()
	if Shared(guardCfg("http://127.0.0.1:1", "x.yaml")) != nil || constructed.Load() != mark {
		t.Error("an inert config (no vllm_seats) must return nil without building a guard")
	}
}
