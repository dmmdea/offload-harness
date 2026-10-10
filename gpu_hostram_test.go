package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// `gpu reserve` and the host RAM a lease declares (the paging incident of 2026-10-09). The wrapped
// command is the test binary's own helper (TestHelperWriteLeaseEnv), with the arguments of a render
// helper call appended after it: the test binary ignores positional arguments, and the estimator
// reads them as the call the lease wraps.

// hostAt installs a host with 100 GiB physical and the given committed memory, for the length of the test.
func hostAt(t *testing.T, commit float64) {
	t.Helper()
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) {
		return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 100 - commit/2, CommitUsedGiB: commit, CommitLimitGiB: 160}, true
	})
	t.Cleanup(restore)
}

// reserveWithCmd is a `gpu reserve` of the helper command followed by extra words (the render call).
func reserveWithCmd(cfg string, flags []string, extra ...string) []string {
	args := append([]string{"--config", cfg}, flags...)
	args = append(args, "--", os.Args[0], "-test.run=TestHelperWriteLeaseEnv", "-test.timeout=30s")
	return append(args, extra...)
}

func krea2Words() []string {
	return strings.Fields("render/comfy-generate.mjs --batch jobs.jsonl --family krea2 --ckpt krea2_turbo_bf16.safetensors")
}

// emptyComfyDir points the fixture's config at an install with no model tree. scopedLeaseFixture writes
// only state_dir and the card-scoped switch, so without this the config would be config.Default()'s
// ComfyDir (C:/ComfyUI on Windows), and an estimate would read the weights of whatever install the
// machine running the tests has: a result that depends on the machine, and a test that reads a live
// install. With nothing to size, the documented per-family sizes apply, on every machine.
func emptyComfyDir(t *testing.T, cfgPath string) {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["comfy_dir"] = t.TempDir()
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The estimate reaches the lease: a krea2 bf16 call on a 16 GiB card declares the unet plus the text
// encoder (the documented family sizes, 24.5 + 8.3 = 32.8 GiB, since this fixture binds no model tree),
// on the record the next grant and `gpu status` read.
func TestReserveDeclaresTheEstimatedHostRAMOnTheLease(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	emptyComfyDir(t, cfg)
	useCardTable(t, "")
	t.Setenv("LO_HELPER_SLEEP_MS", "2500")
	t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--devices", "2", "--wait", "0", "--reason", "krea2"}, krea2Words()...))
	}()
	leases := waitForLeases(t, m, 1)
	// 32.8, not the 32.75 a real krea2 install sums to: the documented sizes, whatever this machine has.
	if got := leases[0].HostRAMGiB; math.Abs(got-32.8) > 0.01 {
		t.Fatalf("the lease declares %.2f GiB, want the documented krea2 unet + text encoder (24.5 + 8.3 = 32.8)", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("reserve: %v", err)
	}
}

// The operator's --ram beats the estimate, and 0 is a value that says "needs no host RAM".
func TestReserveExplicitRamBeatsTheEstimate(t *testing.T) {
	for _, tc := range []struct {
		ram  string
		want float64
	}{{"5", 5}, {"0", 0}} {
		cfg, m := scopedLeaseFixture(t)
		useCardTable(t, "")
		t.Setenv("LO_HELPER_SLEEP_MS", "2000")
		t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
		done := make(chan error, 1)
		go func() {
			done <- runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--devices", "2", "--wait", "0", "--ram", tc.ram}, krea2Words()...))
		}()
		leases := waitForLeases(t, m, 1)
		if leases[0].HostRAMGiB != tc.want {
			t.Fatalf("--ram %s: the lease declares %.2f GiB, want %.2f", tc.ram, leases[0].HostRAMGiB, tc.want)
		}
		if err := <-done; err != nil {
			t.Fatalf("reserve --ram %s: %v", tc.ram, err)
		}
	}
}

// A text reservation declares nothing, and so is admitted by a box that is over: a bench that adds
// no host memory is not made to wait behind a memory shortage.
func TestReserveTextLeaseDeclaresNothingAndIsAdmittedOnAnOverCommittedHost(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	hostAt(t, 400) // far over physical RAM
	t.Setenv("LO_HELPER_SLEEP_MS", "1500")
	t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(reserveWithCmd(cfg, []string{"--class", "text", "--devices", "2", "--wait", "0", "--reason", "kv bench"}))
	}()
	leases := waitForLeases(t, m, 1)
	if leases[0].HostRAMGiB != 0 {
		t.Fatalf("a text lease declares no host RAM, got %.2f", leases[0].HostRAMGiB)
	}
	if err := <-done; err != nil {
		t.Fatalf("a text reservation must be admitted on an over-committed host: %v", err)
	}
}

// --wait 0 refuses with the host-RAM text, the way out, and no lease: the incident's second lane,
// refused while the first holds the memory.
func TestReserveWaitZeroRefusesWithTheHostRAMText(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	hostAt(t, 90)
	err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--devices", "2", "--wait", "0", "--ram", "30"}))
	if err == nil {
		t.Fatal("30 GiB on a 100 GiB box with 90 committed must be refused")
	}
	var short *gpulease.ErrHostRAM
	if !errors.As(err, &short) {
		t.Fatalf("want the host-RAM refusal, got %T: %v", err, err)
	}
	for _, want := range []string{"waiting for host RAM: needs 30.0 GiB, committed 90.0 of 100.0 GiB physical, 8.0 GiB headroom", "--wait", "--ram"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must contain %q: %v", want, err)
		}
	}
	if l := m.Leases(); len(l) != 0 {
		t.Fatalf("a refused reserve holds nothing, got %+v", l)
	}
}

// A reserve that waits keeps its place in line, says so once, and runs when the host has the room.
func TestReserveWaitsForHostRAMInTheQueueAndRunsWhenItFrees(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	commit := newHostCommit(90)
	useHostCommit(t, commit)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
	done := make(chan error, 1)
	told := make(chan string, 1)
	go func() {
		told <- captureStderr(t, func() {
			done <- runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--devices", "2", "--wait", "30s", "--ram", "30"}))
		})
	}()
	// It is queued, marked as waiting on host RAM.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ws := m.Waiters(); len(ws) == 1 && ws[0].WaitingFor == gpulease.WaitHostRAM {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ws := m.Waiters(); len(ws) != 1 || ws[0].WaitingFor != gpulease.WaitHostRAM || ws[0].HostRAMGiB != 30 {
		t.Fatalf("the reserve must be a waiter marked host-ram with its declared need, got %+v", ws)
	}
	commit.set(40) // the other lane finished: 40 + 30 <= 92
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("it runs once the host has the room: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("never ran after the host freed")
	}
	stderr := <-told
	if n := strings.Count(stderr, "waiting for host RAM"); n != 1 {
		t.Fatalf("the wait is told once, not per poll; told %d times in:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "acquired after") {
		t.Fatalf("it says when the wait ended:\n%s", stderr)
	}
}

// An impossible need ends the reserve at once, naming the flag and the key to change.
func TestReserveAnImpossibleNeedIsRefusedAtOnce(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "")
	hostAt(t, 10)
	start := time.Now()
	err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--devices", "2", "--wait", "5m", "--ram", "99"}))
	if err == nil || !strings.Contains(err.Error(), "can never admit") || !strings.Contains(err.Error(), "--ram") || !strings.Contains(err.Error(), "gpu_host_ram_headroom_gib") {
		t.Fatalf("want the impossible refusal naming --ram and the headroom key, got: %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("an impossible need waited %s", time.Since(start))
	}
}

// A negative --ram is a mistake, not a request.
func TestReserveRefusesANegativeRam(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--ram", "-4"}))
	if err == nil || !strings.Contains(err.Error(), "--ram") {
		t.Fatalf("a negative --ram must be refused, got: %v", err)
	}
}

// The detached holder gets the number the parent resolved, on every path (named cards, allocated
// cards, the whole node), 0 included, and does not re-resolve it.
func TestHoldArgsCarryTheResolvedRamOnEveryPath(t *testing.T) {
	word := func(args []string, flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return "<absent>"
	}
	named := holdArgs("media", time.Hour, time.Minute, gpulease.Options{Devices: []string{"gpu-aaaa0000-x"}, HostRAMGiB: 32.8}, nil, "")
	whole := holdArgs("media", time.Hour, time.Minute, gpulease.Options{HostRAMGiB: 12}, nil, "")
	zero := holdArgs("text", time.Hour, time.Minute, gpulease.Options{}, nil, "")
	auto := holdArgs("media", time.Hour, time.Minute, gpulease.Options{HostRAMGiB: 32.8}, &reserveDeviceFlags{cards: "1", vramGiB: 4, ramGiB: 32.8}, "")
	for name, tc := range map[string]struct {
		args []string
		want string
	}{"named cards": {named, "32.8"}, "whole node": {whole, "12"}, "a lease that declares nothing": {zero, "0"}, "allocated cards": {auto, "32.8"}} {
		if got := word(tc.args, "--ram"); got != tc.want {
			t.Errorf("%s: --ram %s, want %s (%v)", name, got, tc.want, tc.args)
		}
	}
	if n := strings.Count(strings.Join(auto, " "), "--ram"); n != 1 {
		t.Errorf("--ram must appear once on the allocated path, appeared %d times: %v", n, auto)
	}
}

// resolveReserveHostRAM reads the card table only when an estimate needs a card's VRAM: a text bench,
// or any reservation with an explicit --ram, pays no nvidia-smi.
func TestResolveReserveHostRAMReadsTheCardTableOnlyWhenItMatters(t *testing.T) {
	useCardTable(t, "")
	inner := cardTableFn
	reads := 0
	cardTableFn = func(ctx context.Context, cfg config.Config) ([]gpuprobe.Card, string, error) {
		reads++
		return inner(ctx, cfg)
	}
	t.Cleanup(func() { cardTableFn = inner })
	cases := []struct {
		name     string
		explicit bool
		class    gpulease.Class
		args     []string
		reads    int
	}{
		{"a text bench", false, gpulease.ClassText, []string{"python", "bench.py"}, 0},
		{"an explicit --ram on a media lease", true, gpulease.ClassMedia, krea2Words(), 0},
		{"a media lease with an unrecognised command (the class default sizes against the card)", false, gpulease.ClassMedia, []string{"python", "x.py"}, 1},
		{"a recognised render call", false, gpulease.ClassText, krea2Words(), 1},
	}
	for _, c := range cases {
		reads = 0
		var out bytes.Buffer
		resolveReserveHostRAM(c.explicit, 5, c.class, c.args, nil, config.Config{}, &out)
		if reads != c.reads {
			t.Errorf("%s: read the card table %d times, want %d", c.name, reads, c.reads)
		}
	}
	// A text bench declares nothing and says nothing.
	var out bytes.Buffer
	if n := resolveReserveHostRAM(false, 0, gpulease.ClassText, []string{"python", "bench.py"}, nil, config.Config{}, &out); n.GiB != 0 || n.Source != hostneed.SourceNone || out.Len() != 0 {
		t.Fatalf("a text bench declares nothing and says nothing: %v %q", n, out.String())
	}
}

// hostCommit is committed memory a test moves while reserves poll it.
type hostCommit struct {
	mu sync.Mutex
	v  float64
}

func newHostCommit(v float64) *hostCommit { return &hostCommit{v: v} }
func (h *hostCommit) get() float64        { h.mu.Lock(); defer h.mu.Unlock(); return h.v }
func (h *hostCommit) set(v float64)       { h.mu.Lock(); defer h.mu.Unlock(); h.v = v }

// useHostCommit installs a 100 GiB host whose committed memory the test moves, on BOTH readings of it:
// the card allocator's seam (buildAllocInput) and the lease grant's (gpuprobe), which are one rule on
// two doors and must see one host.
func useHostCommit(t *testing.T, commit *hostCommit) {
	t.Helper()
	read := func() (gpuprobe.HostMemory, bool) {
		return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 10, CommitUsedGiB: commit.get(), CommitLimitGiB: 160}, true
	}
	restore := gpuprobe.UseHostMemoryReader(read)
	old := hostMemoryFn
	hostMemoryFn = read
	t.Cleanup(func() { restore(); hostMemoryFn = old })
}

// `--cards N` is judged by the same rule before a card is picked: refused at once with the text on
// --wait 0, with no lease taken.
func TestReserveCardsWaitZeroRefusesWithTheHostRAMText(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	commit := newHostCommit(90)
	useHostCommit(t, commit)
	err := runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--cards", "1", "--wait", "0", "--ram", "30"}))
	if err == nil || !strings.Contains(err.Error(), "waiting for host RAM: needs 30.0 GiB, committed 90.0 of 100.0 GiB physical, 8.0 GiB headroom") {
		t.Fatalf("--cards on a host without the room must refuse with the host-RAM text, got: %v", err)
	}
	if l := m.Leases(); len(l) != 0 {
		t.Fatalf("a refused reserve holds nothing, got %+v", l)
	}
}

// And it waits, picks its card and runs when the host frees (the allocator polls while only the host is short).
func TestReserveCardsWaitsForHostRAMThenRuns(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	commit := newHostCommit(90)
	useHostCommit(t, commit)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	t.Setenv("LO_HELPER_ENV_OUT", t.TempDir()+"/env.txt")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(reserveWithCmd(cfg, []string{"--class", "media", "--cards", "1", "--wait", "40s", "--ram", "30"}))
	}()
	time.Sleep(1500 * time.Millisecond)
	if l := m.Leases(); len(l) != 0 {
		t.Fatalf("it must not run while the host is short, but a lease exists: %+v", l)
	}
	commit.set(40)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("it runs once the host has the room: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("never ran after the host freed")
	}
}

// `gpu status`: the host's memory, the declared needs of the live leases, a waiter that waits on the
// host, and the verdict OK / NEAR / OVER, in the text and in --json.

func statusJSON(t *testing.T, cfg string) map[string]any {
	t.Helper()
	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("one JSON document: %v\n%s", err, out)
	}
	return doc
}

func TestGPUStatusJSONCarriesTheHostBlockAndAnOverVerdict(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) {
		return gpuprobe.HostMemory{PhysicalGiB: 127.7, AvailableGiB: 3.2, CommitUsedGiB: 162.9, CommitLimitGiB: 187.7}, true
	})
	defer restore()
	// A live lease that declared 30 GiB of host RAM (neither admitted nor refused by this test's host:
	// a lease that declares a need is taken on a roomy one first, then the host turns over-committed).
	roomy := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return roomyTestHost, true })
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "krea2 lane", TTL: time.Hour, Devices: []string{"gpu-cccc0000-x"}, HostRAMGiB: 30})
	roomy()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	doc := statusJSON(t, cfg)
	hm, _ := doc["host_memory"].(map[string]any)
	if hm == nil {
		t.Fatalf("gpu status --json carries no host_memory block: %v", doc)
	}
	for k, want := range map[string]any{"verdict": "OVER", "physical_gib": 127.7, "commit_used_gib": 162.9, "commit_limit_gib": 187.7, "available_gib": 3.2, "declared_live_gib": 30.0, "headroom_gib": 8.0} {
		if hm[k] != want {
			t.Errorf("host_memory.%s = %v, want %v", k, hm[k], want)
		}
	}
	if note, _ := hm["note"].(string); !strings.Contains(note, "paging") {
		t.Errorf("an OVER verdict says what it means: %q", note)
	}
	leases, _ := doc["leases"].([]any)
	if len(leases) != 1 || leases[0].(map[string]any)["host_ram_gib"] != 30.0 {
		t.Errorf("the lease row must carry what it declared: %v", doc["leases"])
	}

	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"host memory: OVER", "127.7 GiB physical", "commit 162.9 of 187.7 GiB", "live leases declared 30.0 GiB", "host RAM 30.0 GiB", "never an acceptable state"} {
		if !strings.Contains(out, want) {
			t.Errorf("the text status lacks %q:\n%s", want, out)
		}
	}
}

func TestGPUStatusShowsOKAndAFreeCardWithTheHostLine(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "host memory: OK") || !strings.Contains(out, "no live lease declared host RAM") {
		t.Errorf("a free card still shows the host line:\n%s", out)
	}
	if doc := statusJSON(t, cfg); doc["host_memory"].(map[string]any)["verdict"] != "OK" {
		t.Errorf("verdict = %v", doc["host_memory"])
	}
}

// A reserve that waits on the host is listed as such, with what it declared.
func TestGPUStatusListsAWaiterThatWaitsOnHostRAM(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	commit := newHostCommit(90)
	useHostCommit(t, commit)
	done := make(chan error, 1)
	go func() {
		l, err := m.Acquire(gpulease.ClassMedia, gpulease.Options{Reason: "krea2 lane", TTL: time.Hour, Devices: []string{"gpu-cccc0000-x"}, HostRAMGiB: 30, Wait: 30 * time.Second, WaitOut: true})
		if err == nil {
			_ = l.Release()
		}
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ws := m.Waiters(); len(ws) == 1 && ws[0].WaitingFor == gpulease.WaitHostRAM {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	doc := statusJSON(t, cfg)
	queued, _ := doc["queued"].([]any)
	if len(queued) != 1 {
		t.Fatalf("queued = %v", doc["queued"])
	}
	row := queued[0].(map[string]any)
	if row["waiting_for"] != "host-ram" || row["host_ram_gib"] != 30.0 {
		t.Fatalf("the queue row must say what it waits for: %v", row)
	}
	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "waiting for host RAM (needs 30.0 GiB)") {
		t.Errorf("the text queue line must say it waits on host RAM:\n%s", out)
	}
	commit.set(40)
	if err := <-done; err != nil {
		t.Fatalf("it is granted once the host has the room: %v", err)
	}
}
