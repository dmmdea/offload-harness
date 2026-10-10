package comfyinst

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A kept ComfyUI instance lives no longer than the GPU lease it was launched under (plan P13).
// render/comfy-lifecycle.mjs records that lease's epoch in the instance's launch marker; the
// holder of the lease stops the instance when it releases. Everything here is synthetic: a
// marker file, an httptest server standing in for the instance's endpoint, and injected
// liveness and kill. Nothing signals a real process except the one test that spawns its own.

var testArgs = []string{"main.py", "--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0", "--port", "8189"}

type instance struct {
	srv   *httptest.Server
	mu    sync.Mutex
	freed int
	argv  []string // what /system_stats reports
	down  bool     // the endpoint stops answering
	// onStats runs while /system_stats is being answered: the window between the proof and the
	// stop, in which another lease can take the instance over.
	onStats func()
	// slowStats is how many of the next /system_stats requests are answered only after slowFor: an
	// instance in the middle of a long prompt (the qwen-image prompts in the field run 550 to 585 s).
	slowStats atomic.Int32
	slowFor   time.Duration
}

func startInstance(t *testing.T, argv []string) *instance {
	t.Helper()
	in := &instance{argv: argv}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/system_stats" && in.slowStats.Add(-1) >= 0 {
			time.Sleep(in.slowFor)
		}
		in.mu.Lock()
		defer in.mu.Unlock()
		if in.down {
			http.Error(w, "gone", http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.URL.Path == "/system_stats":
			if in.onStats != nil {
				in.onStats()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"system": map[string]any{"argv": in.argv}})
		case r.URL.Path == "/free" && r.Method == http.MethodPost:
			in.freed++
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(in.srv.Close)
	return in
}

func (in *instance) port(t *testing.T) int {
	t.Helper()
	u, err := url.Parse(in.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (in *instance) timesFreed() int { in.mu.Lock(); defer in.mu.Unlock(); return in.freed }

func writeMarker(t *testing.T, dir, file string, rec map[string]any) string {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeHost is the process side: who is alive, when each started, and what a kill does.
type fakeHost struct {
	mu     sync.Mutex
	alive  map[int]bool
	start  map[int]int64
	killed []int
	// killWorks false = the process survives a kill (it does not die).
	killWorks bool
	onKill    func(pid int)
}

func newHost() *fakeHost {
	return &fakeHost{alive: map[int]bool{}, start: map[int]int64{}, killWorks: true}
}

func (h *fakeHost) deps(in *instance) Deps {
	return Deps{
		Alive: func(pid int) bool { h.mu.Lock(); defer h.mu.Unlock(); return h.alive[pid] },
		Start: func(pid int) (int64, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			s, ok := h.start[pid]
			return s, ok
		},
		Kill: func(pid int) error {
			h.mu.Lock()
			h.killed = append(h.killed, pid)
			if h.killWorks {
				h.alive[pid] = false
			}
			cb := h.onKill
			h.mu.Unlock()
			if h.killWorks && in != nil {
				in.mu.Lock()
				in.down = true
				in.mu.Unlock()
			}
			if cb != nil {
				cb(pid)
			}
			return nil
		},
		Sleep:       func(time.Duration) {},
		QuietPolls:  3,
		HTTPTimeout: 2 * time.Second,
	}
}

func (h *fakeHost) killedPIDs() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.killed...)
}

func keyedMarker(port, pid int, epoch any, extra map[string]any) map[string]any {
	rec := map[string]any{
		"startedAt": 1_760_000_000_000, "pid": pid, "ownerPid": 111, "args": testArgs,
		"profile": map[string]any{"cardUuid": "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"},
		"key":     "gaaaa1111", "port": port,
	}
	if epoch != nil {
		rec["leaseEpoch"] = epoch
	}
	for k, v := range extra {
		rec[k] = v
	}
	return rec
}

// TestInstanceStoppedWithLease: the holder of lease epoch 7 releases it, and the instance
// launched under epoch 7 is freed, stopped and its marker cleared.
func TestInstanceStoppedWithLease(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000 // the process began just before its marker was written
	marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))

	got := StopForLease(context.Background(), dir, 7, host.deps(in))

	if len(got) != 1 || !got[0].Stopped || got[0].PID != 4242 || got[0].Key != "gaaaa1111" {
		t.Fatalf("outcomes = %+v, want the one instance stopped", got)
	}
	if in.timesFreed() != 1 {
		t.Errorf("POST /free = %d times, want 1 (the instance's models are dropped before it stops)", in.timesFreed())
	}
	if pids := host.killedPIDs(); len(pids) != 1 || pids[0] != 4242 {
		t.Errorf("killed %v, want only the marker's pid 4242", pids)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the marker must be cleared once the instance is gone (stat err: %v)", err)
	}
}

// A holder that is stopping epoch 7's instance proves it is ours (live pid, the exact argv), and
// that proof takes a round trip to the instance. A lease that reuses the instance in that window
// re-stamps its marker (render/comfy-lifecycle.mjs): the instance is epoch 8's now, and stopping
// it would kill the job that lease is running on it. The marker is read again at the moment of
// the stop, and an instance that changed hands is left alone.
func TestAnInstanceThatChangedHandsDuringTheProofIsNotStopped(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))
	in.onStats = func() { // the next lease reuses the instance and takes its marker over
		writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 8, nil))
	}

	got := StopForLease(context.Background(), dir, 7, host.deps(in))

	if len(got) != 1 || got[0].Stopped {
		t.Fatalf("outcomes = %+v, want the instance reported and not stopped", got)
	}
	if !strings.Contains(got[0].Why, "changed hands") || !strings.Contains(got[0].Why, "8") {
		t.Errorf("why = %q, want it to say the instance changed hands and name the new lease", got[0].Why)
	}
	if len(host.killedPIDs()) != 0 || in.timesFreed() != 0 {
		t.Errorf("an instance another lease just took over was touched: killed %v, freed %d", host.killedPIDs(), in.timesFreed())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the new lease's marker must stay: %v", err)
	}
}

// A marker that vanished (the instance was stopped by someone else) or became unreadable in the
// same window is not proof of anything either.
func TestAnInstanceWhoseMarkerVanishedDuringTheProofIsNotStopped(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))
	in.onStats = func() { _ = os.Remove(marker) }

	got := StopForLease(context.Background(), dir, 7, host.deps(in))

	if len(got) != 1 || got[0].Stopped || len(host.killedPIDs()) != 0 {
		t.Fatalf("outcomes = %+v killed %v, want nothing stopped", got, host.killedPIDs())
	}
}

// An instance launched under another lease is not this lease's to stop.
func TestAnotherLeasesInstanceIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 8, nil))

	got := StopForLease(context.Background(), dir, 7, host.deps(in))

	if len(got) != 0 {
		t.Fatalf("outcomes = %+v, want none for another lease's instance", got)
	}
	if len(host.killedPIDs()) != 0 || in.timesFreed() != 0 {
		t.Errorf("another lease's instance was touched: killed %v, freed %d", host.killedPIDs(), in.timesFreed())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the marker must stay: %v", err)
	}
}

// A marker that records no lease epoch is never touched: the default instance's marker, and
// a keyed instance launched outside a lease, belong to whoever kept them.
func TestMarkerWithoutALeaseEpochIsNeverTouched(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242], host.alive[4343] = true, true
	keyed := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, nil, nil))
	def := writeMarker(t, dir, ".offload-launch.json", map[string]any{"startedAt": 1, "pid": 4343, "ownerPid": 1, "args": testArgs, "profile": map[string]any{}})

	got := StopForLease(context.Background(), dir, 7, host.deps(in))

	if len(got) != 0 || len(host.killedPIDs()) != 0 {
		t.Fatalf("outcomes %+v, killed %v: a marker with no lease epoch must never be touched", got, host.killedPIDs())
	}
	for _, p := range []string{keyed, def} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), err)
		}
	}
}

// The default instance is never stopped even if a (hand-edited) marker names this epoch: it has
// no key, and 8188 is not a per-card instance.
func TestDefaultInstanceMarkerIsNeverStoppedEvenWithAnEpoch(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4343] = true
	writeMarker(t, dir, ".offload-launch.json", map[string]any{"startedAt": 1, "pid": 4343, "ownerPid": 1, "args": testArgs, "port": in.port(t), "leaseEpoch": 7})
	got := StopForLease(context.Background(), dir, 7, host.deps(in))
	if len(got) != 0 || len(host.killedPIDs()) != 0 {
		t.Fatalf("outcomes %+v killed %v: the default instance is not the lease's to stop", got, host.killedPIDs())
	}
}

// Whatever answers on the marker's port must be shown to be that instance: the exact argv.
func TestAnInstanceNotShownToBeOursIsNotKilled(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		down bool
		want string
	}{
		{"another argv", []string{"main.py", "--port", "8189", "--something-else"}, false, "argv"},
		{"nothing answers", nil, true, "answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := startInstance(t, tc.argv)
			in.down = tc.down
			host := newHost()
			host.alive[4242] = true
			marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))

			got := StopForLease(context.Background(), dir, 7, host.deps(in))

			if len(got) != 1 || got[0].Stopped || !strings.Contains(got[0].Why, tc.want) {
				t.Fatalf("outcomes = %+v, want one not-stopped outcome whose reason mentions %q", got, tc.want)
			}
			if len(host.killedPIDs()) != 0 {
				t.Errorf("killed %v: an instance that is not shown to be ours is never killed", host.killedPIDs())
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("the marker must stay for the operator to read: %v", err)
			}
		})
	}
}

// A pid that began after the marker was written is not the instance (the pid was recycled).
func TestARecycledPidIsNotKilled(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 + 600_000 // began ten minutes AFTER the marker
	marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))

	got := StopForLease(context.Background(), dir, 7, host.deps(in))

	if len(host.killedPIDs()) != 0 {
		t.Fatalf("killed %v: the pid now names an unrelated process", host.killedPIDs())
	}
	if len(got) != 1 || got[0].Stopped || !strings.Contains(got[0].Why, "recycled") {
		t.Fatalf("outcomes = %+v, want a not-stopped outcome naming the recycled pid", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a marker whose pid is recycled is stale and is cleared (stat err: %v)", err)
	}
}

func TestADeadInstanceJustClearsItsMarker(t *testing.T) {
	dir := t.TempDir()
	host := newHost() // pid 4242 is not alive
	marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(1, 4242, 7, nil))

	got := StopForLease(context.Background(), dir, 7, host.deps(nil))

	if len(got) != 1 || got[0].Stopped || !strings.Contains(got[0].Why, "already gone") {
		t.Fatalf("outcomes = %+v, want an already-gone outcome", got)
	}
	if len(host.killedPIDs()) != 0 {
		t.Errorf("killed %v: nothing to kill", host.killedPIDs())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a dead instance's marker is cleared (stat err: %v)", err)
	}
}

// A kill that does not take says so and leaves the marker: the lease is gone but the instance
// is not, and that has to be visible rather than assumed away.
func TestAKillThatDoesNotTakeLeavesTheMarkerAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.killWorks = false
	marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))

	got := StopForLease(context.Background(), dir, 7, host.deps(in))

	if len(got) != 1 || got[0].Stopped || !strings.Contains(got[0].Why, "still running") {
		t.Fatalf("outcomes = %+v, want a not-stopped outcome saying the instance is still running", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the marker must stay while the instance lives: %v", err)
	}
}

func TestUnreadableOrIncompleteMarkersAreSkipped(t *testing.T) {
	dir := t.TempDir()
	host := newHost()
	host.alive[4242] = true
	os.WriteFile(filepath.Join(dir, ".offload-launch-bad.json"), []byte("{not json"), 0o644)
	writeMarker(t, dir, ".offload-launch-nokey.json", map[string]any{"pid": 4242, "port": 8189, "leaseEpoch": 7, "args": testArgs})
	writeMarker(t, dir, ".offload-launch-noport.json", map[string]any{"pid": 4242, "key": "k", "leaseEpoch": 7, "args": testArgs})
	writeMarker(t, dir, ".offload-launch-nopid.json", map[string]any{"key": "k", "port": 8189, "leaseEpoch": 7, "args": testArgs})
	writeMarker(t, dir, "unrelated.json", keyedMarker(8189, 4242, 7, nil))
	got := StopForLease(context.Background(), dir, 7, host.deps(nil))
	if len(got) != 0 || len(host.killedPIDs()) != 0 {
		t.Fatalf("outcomes %+v killed %v: nothing here is a complete keyed marker for epoch 7", got, host.killedPIDs())
	}
}

func TestAMissingOrUnboundComfyDirIsANoOp(t *testing.T) {
	host := newHost()
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "nope")} {
		if got := StopForLease(context.Background(), dir, 7, host.deps(nil)); len(got) != 0 {
			t.Errorf("dir %q: outcomes = %+v, want none", dir, got)
		}
	}
	if got := StopForLease(context.Background(), t.TempDir(), 0, host.deps(nil)); len(got) != 0 {
		t.Errorf("epoch 0 is no lease: outcomes = %+v", got)
	}
}

// Two instances launched under one lease are both stopped, and the order is stable.
func TestEveryInstanceOfTheLeaseIsStopped(t *testing.T) {
	dir := t.TempDir()
	a := startInstance(t, testArgs)
	b := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242], host.alive[4343] = true, true
	hostDeps := host.deps(nil)
	host.onKill = func(pid int) {
		in := a
		if pid == 4343 {
			in = b
		}
		in.mu.Lock()
		in.down = true
		in.mu.Unlock()
	}
	writeMarker(t, dir, ".offload-launch-gbbbb2222.json", keyedMarker(b.port(t), 4343, 7, map[string]any{"key": "gbbbb2222"}))
	writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(a.port(t), 4242, 7, nil))

	got := StopForLease(context.Background(), dir, 7, hostDeps)

	if len(got) != 2 || !got[0].Stopped || !got[1].Stopped || got[0].Key != "gaaaa1111" || got[1].Key != "gbbbb2222" {
		t.Fatalf("outcomes = %+v, want both stopped in key order", got)
	}
}

// The real kill primitive, on a real child, because the injected one proves nothing about
// whether the platform call works. Spawns this test binary as a sleeper.
func TestTerminateStopsARealProcess(t *testing.T) {
	if os.Getenv("COMFYINST_SLEEPER") == "1" {
		time.Sleep(60 * time.Second)
		return
	}
	cmd := helperSleeper(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if !realAlive(cmd.Process.Pid) {
		t.Fatal("the helper process is not alive before the kill")
	}
	if err := terminate(cmd.Process.Pid); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("pid %d is still running after terminate", cmd.Process.Pid)
	}
}

func TestRealDepsAreWiredToThePlatform(t *testing.T) {
	d := RealDeps()
	if d.Alive == nil || d.Start == nil || d.Kill == nil || d.Sleep == nil || d.QuietPolls < 1 || d.HTTPTimeout <= 0 {
		t.Fatalf("RealDeps incomplete: %+v", d)
	}
	if !d.Alive(os.Getpid()) {
		t.Error("the real liveness probe says this process is dead")
	}
	_ = fmt.Sprint(d)
}

func helperSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminateStopsARealProcess$")
	cmd.Env = append(os.Environ(), "COMFYINST_SLEEPER=1")
	return cmd
}

func realAlive(pid int) bool { return gpulease.PIDAlive(pid) }

// THE PATH THAT LEFT A KEPT INSTANCE BEHIND WITH ITS MODELS. The holder stopping a lease's instance
// first proves it is the harness's own by asking it for its launch argv, and an instance in the middle
// of a long prompt answers late. With one short try that read "did not answer ... left running": the
// instance outlived its lease holding its weights, and the next lease reused it and loaded another
// family beside them. The proof is asked for more than once now.
func TestASlowInstanceIsStillProvenAndStopped(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	in.slowFor = 400 * time.Millisecond
	in.slowStats.Store(2) // the first two answers arrive after the client has given up
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))
	deps := host.deps(in)
	deps.HTTPTimeout, deps.ProofAttempts = 150*time.Millisecond, 3

	got := StopForLease(context.Background(), dir, 7, deps)

	if len(got) != 1 || !got[0].Stopped {
		t.Fatalf("outcomes = %+v: an instance that answers on the third try is proven and stopped", got)
	}
	if in.timesFreed() != 1 || len(host.killedPIDs()) != 1 {
		t.Errorf("freed %d, killed %v: the proven instance is freed then stopped", in.timesFreed(), host.killedPIDs())
	}
}

// One try is the old behaviour, and it reports the instance and leaves it; the message says how many
// attempts it made and what happens next, so the operator does not read it as the end of the story.
func TestOneSlowAnswerWithOneAttemptLeavesTheInstanceAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	in.slowFor = 400 * time.Millisecond
	in.slowStats.Store(1)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))
	deps := host.deps(in)
	deps.HTTPTimeout, deps.ProofAttempts = 150*time.Millisecond, 1

	got := StopForLease(context.Background(), dir, 7, deps)

	if len(got) != 1 || got[0].Stopped || len(host.killedPIDs()) != 0 {
		t.Fatalf("outcomes = %+v killed %v: with a single attempt the slow instance is left running", got, host.killedPIDs())
	}
	if !strings.Contains(got[0].Why, "after 1 attempt(s)") || !strings.Contains(got[0].Why, "frees it before its first job") {
		t.Errorf("why = %q, want the attempts and what happens next", got[0].Why)
	}
}

// The production wiring asks more than once, for longer than the old three seconds.
func TestRealDepsAskForTheProofMoreThanOnce(t *testing.T) {
	d := RealDeps()
	if d.ProofAttempts < 2 || d.HTTPTimeout <= 3*time.Second {
		t.Fatalf("RealDeps proof = %d attempts of %s: a busy instance answers late, and it is the one holding the memory", d.ProofAttempts, d.HTTPTimeout)
	}
}
