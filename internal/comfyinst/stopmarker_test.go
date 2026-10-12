package comfyinst

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The stop primitives a takeover is built from (plan P10, used by P11): stop the instance a
// launch marker names, whatever lease it was launched under, after the same proof the lease's own
// holder gives, and ask it to stop its prompt before it is killed. Synthetic throughout: a marker
// file, an httptest endpoint and injected liveness and kill.

// A marker's instance is interrupted, freed and only then killed, and only after its argv proves
// it is the harness's own.
func TestStopMarkerInterruptsFreesThenKillsAfterTheFingerprintIsVerified(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))

	got := StopMarker(context.Background(), dir, "gaaaa1111", 0, host.deps(in))

	if !got.Stopped || got.PID != 4242 || got.Key != "gaaaa1111" {
		t.Fatalf("outcome = %+v, want the instance stopped", got)
	}
	in.mu.Lock()
	order := append([]string(nil), in.order...)
	in.mu.Unlock()
	if len(order) != 2 || order[0] != "interrupt" || order[1] != "free" {
		t.Errorf("calls to the instance = %v, want interrupt then free (the running prompt is told to stop before its models are dropped)", order)
	}
	if pids := host.killedPIDs(); len(pids) != 1 || pids[0] != 4242 {
		t.Errorf("killed %v, want only the marker's pid", pids)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the marker is cleared once the instance is gone (stat err: %v)", err)
	}
}

// A takeover can name an instance whose marker records no lease at all (kept outside any lease):
// it is stopped the same way, and a lease's own release still never touches it.
func TestStopMarkerStopsAnInstanceThatNamesNoLease(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, nil, nil))
	if got := StopForLease(context.Background(), dir, 7, host.deps(in)); len(got) != 0 {
		t.Fatalf("a lease's release must not touch an instance that names no lease: %+v", got)
	}
	if got := StopMarker(context.Background(), dir, "gaaaa1111", 0, host.deps(in)); !got.Stopped {
		t.Fatalf("an explicit stop of the marker's instance: %+v", got)
	}
}

// The default instance's marker records no port (only keyed instances do), so the caller names
// it; with neither, the instance is not shown to be ours and is left running.
func TestStopMarkerNeedsAPortForTheDefaultInstance(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	rec := keyedMarker(0, 4242, nil, nil)
	delete(rec, "key")
	delete(rec, "port")
	writeMarker(t, dir, ".offload-launch.json", rec)

	if got := StopMarker(context.Background(), dir, "", 0, host.deps(in)); got.Stopped || len(host.killedPIDs()) != 0 || !strings.Contains(got.Why, "records no port") {
		t.Fatalf("with no port the instance is left running and the reason says so: %+v", got)
	}
	if got := StopMarker(context.Background(), dir, "", in.port(t), host.deps(in)); !got.Stopped {
		t.Fatalf("with the port it is stopped: %+v", got)
	}
}

// An instance whose argv is not the one the launcher recorded is somebody else's: it is neither
// interrupted nor freed nor killed.
func TestStopMarkerLeavesAnInstanceThatIsNotShownToBeOurs(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, []string{"main.py", "--port", "8189", "--someone-elses-flag"})
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))

	got := StopMarker(context.Background(), dir, "gaaaa1111", 0, host.deps(in))

	if got.Stopped || !strings.Contains(got.Why, "not shown to be the harness's own") {
		t.Fatalf("outcome = %+v", got)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.interrupted != 0 || in.freed != 0 || len(host.killedPIDs()) != 0 {
		t.Fatalf("a foreign instance was touched: interrupted %d freed %d killed %v", in.interrupted, in.freed, host.killedPIDs())
	}
}

// A marker whose process is gone, or whose pid began after the marker was written, is cleared and
// nothing is killed.
func TestStopMarkerClearsAMarkerWhoseProcessIsGoneOrRecycled(t *testing.T) {
	for name, setup := range map[string]func(h *fakeHost){
		"gone":     func(h *fakeHost) {},
		"recycled": func(h *fakeHost) { h.alive[4242] = true; h.start[4242] = 1_760_000_000_000 + 600_000 },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			in := startInstance(t, testArgs)
			host := newHost()
			setup(host)
			marker := writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))
			got := StopMarker(context.Background(), dir, "gaaaa1111", 0, host.deps(in))
			if got.Stopped || len(host.killedPIDs()) != 0 {
				t.Fatalf("nothing is killed: %+v", got)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("the dead marker is cleared (stat err: %v)", err)
			}
		})
	}
}

// A wrapper that lost its lease stops its own tree but spares what another lease took over: the
// live processes of markers that name a lease other than its own.
func TestPIDsOfOtherLeasesNamesOnlyLiveInstancesOfOtherLeases(t *testing.T) {
	dir := t.TempDir()
	host := newHost()
	for _, pid := range []int{101, 102, 103, 104} {
		host.alive[pid] = true
	}
	host.alive[105] = false
	writeMarker(t, dir, ".offload-launch-mine.json", keyedMarker(8189, 101, 7, map[string]any{"key": "mine"}))
	writeMarker(t, dir, ".offload-launch-theirs.json", keyedMarker(8190, 102, 8, map[string]any{"key": "theirs"}))
	writeMarker(t, dir, ".offload-launch-nolease.json", keyedMarker(8191, 103, nil, map[string]any{"key": "nolease"}))
	writeMarker(t, dir, ".offload-launch-dead.json", keyedMarker(8192, 105, 9, map[string]any{"key": "dead"}))
	writeMarker(t, dir, ".offload-launch.json", map[string]any{"startedAt": 1, "pid": 104, "args": []string{"main.py"}})

	got := PIDsOfOtherLeases(dir, 7, host.deps(nil).Alive)
	if len(got) != 1 || got[0] != 102 {
		t.Fatalf("PIDsOfOtherLeases = %v, want only 102 (another lease's, alive)", got)
	}
	if got := PIDsOfOtherLeases("", 7, host.deps(nil).Alive); got != nil {
		t.Fatalf("no comfy dir: %v", got)
	}
}

// The proof takes a round trip to the instance; if the marker is rewritten for another process in
// that window (the instance was relaunched), the explicit stop leaves it alone.
func TestStopMarkerLeavesAnInstanceRelaunchedDuringTheProof(t *testing.T) {
	dir := t.TempDir()
	in := startInstance(t, testArgs)
	host := newHost()
	host.alive[4242] = true
	host.start[4242] = 1_760_000_000_000 - 4000
	writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 4242, 7, nil))
	in.onStats = func() {
		writeMarker(t, dir, ".offload-launch-gaaaa1111.json", keyedMarker(in.port(t), 5555, 7, nil))
	}

	got := StopMarker(context.Background(), dir, "gaaaa1111", 0, host.deps(in))

	if got.Stopped || !strings.Contains(got.Why, "relaunched") {
		t.Fatalf("outcome = %+v, want the relaunched instance reported and left", got)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.interrupted != 0 || in.freed != 0 || len(host.killedPIDs()) != 0 {
		t.Fatalf("a relaunched instance was touched: interrupted %d freed %d killed %v", in.interrupted, in.freed, host.killedPIDs())
	}
}
