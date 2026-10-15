package main

// F8: the wrapper form of `gpu reserve` declares a progress contract for a render helper that heartbeats,
// hands the file to the command, and the lease then renews for as long as the helper writes and is labelled
// stalled when it stops. The tests run the REAL runGPUReserve against a scratch state root and a helper
// process (this test binary) that plays the part of render/lease-progress.mjs; none touches a real lease, a
// GPU or nvidia-smi. The node half is render/lease-progress.test.mjs.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// ---- the decision, as a pure function --------------------------------------------------------

func TestAutoProgressIsDeclaredOnlyForAHeartbeatingHelperTheCallerLeftUndeclared(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "progress")
	now := time.Unix(1_791_000_000, 123)
	cases := []struct {
		name string
		opts gpulease.Options
		cmd  []string
		want string // the helper recognised, "" = nothing declared
	}{
		{"comfy-render behind node", gpulease.Options{}, []string{"node", "render/comfy-render.mjs", "out.png", "a cat"}, "comfy-render.mjs"},
		{"comfy-generate batch, Windows separators", gpulease.Options{}, []string{"node", `D:\x\render\comfy-generate.mjs`, "--batch", "jobs.jsonl"}, "comfy-generate.mjs"},
		{"every poll-loop runner", gpulease.Options{}, []string{"node", "/x/render/Comfy-Video.mjs", "o.mp4"}, "comfy-video.mjs"},
		{"run-graph", gpulease.Options{}, []string{"node", "comfy-run-graph.mjs", "--graph", "g.json"}, "comfy-run-graph.mjs"},
		{"the caller declared a contract: it is theirs", gpulease.Options{ProgressFile: "/abs/log.jsonl", Stall: time.Hour}, []string{"node", "render/comfy-render.mjs"}, ""},
		{"a bench is not a helper", gpulease.Options{}, []string{"bash", "bench.sh"}, ""},
		{"an engine that does not poll ComfyUI would read stalled", gpulease.Options{}, []string{"node", "render/sdcpp-generate.mjs", "o.png"}, ""},
		{"nor does the audio engine", gpulease.Options{}, []string{"node", "render/audiocpp-generate.mjs"}, ""},
		{"a Go verb may overflow to another node and never poll here", gpulease.Options{}, []string{"local-offload", "generate-image", "a cat"}, ""},
		{"a script that merely mentions a helper in an argument", gpulease.Options{}, []string{"node", "render/other.mjs", "--note", "comfy-render.mjs is slow"}, ""},
	}
	for _, c := range cases {
		path, helper, ok := autoProgressFile(c.opts, c.cmd, dir, 4242, now, false)
		if c.want == "" {
			if ok || path != "" {
				t.Errorf("%s: declared %q for %q", c.name, path, helper)
			}
			continue
		}
		if !ok || helper != c.want {
			t.Errorf("%s: ok=%v helper=%q, want %q", c.name, ok, helper, c.want)
			continue
		}
		if !filepath.IsAbs(path) || filepath.Dir(path) != dir {
			t.Errorf("%s: %q must be an absolute path directly under %s (the lease refuses a relative one)", c.name, path, dir)
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, gpulease.MintedProgressPrefix+"4242-") || !strings.HasSuffix(base, gpulease.MintedProgressSuffix) {
			t.Errorf("%s: %q is not a minted name (wrap-<pid>-<nanos>.jsonl): the sweep would never remove it", c.name, base)
		}
	}
	// Two wrappers of one process at the same instant... never share a name; two instants never do.
	a, _, _ := autoProgressFile(gpulease.Options{}, []string{"comfy-render.mjs"}, dir, 1, now, false)
	b, _, _ := autoProgressFile(gpulease.Options{}, []string{"comfy-render.mjs"}, dir, 1, now.Add(time.Nanosecond), false)
	c, _, _ := autoProgressFile(gpulease.Options{}, []string{"comfy-render.mjs"}, dir, 2, now, false)
	if a == b || a == c {
		t.Errorf("minted names collide: %q %q %q", a, b, c)
	}
}

// ---- the lists that must not drift ----------------------------------------------------------------

// A helper missing from gpulease.HeartbeatHelperNames runs under a lease with no contract (the defect); one that is
// listed but never beats would be labelled stalled. The list is therefore what render/ contains: every
// runner that calls pollOutputs, plus comfy-generate.mjs, whose children do.
func TestHeartbeatHelpersPinTheRenderDirectory(t *testing.T) {
	entries, err := os.ReadDir("render")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".test.mjs") || name == "comfy-submit.mjs" {
			continue
		}
		src, err := os.ReadFile(filepath.Join("render", name))
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`\bpollOutputs\s*\(`).Match(src) {
			want[name] = true
		}
	}
	// comfy-generate.mjs spawns comfy-render.mjs per picture: its children poll, it beats between pictures.
	gen, err := os.ReadFile(filepath.Join("render", "comfy-generate.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gen), "comfy-render.mjs") || !strings.Contains(string(gen), "leaseBeat") {
		t.Fatal("comfy-generate.mjs no longer runs comfy-render.mjs children / beats between pictures: revisit this list")
	}
	want["comfy-generate.mjs"] = true
	got := gpulease.HeartbeatHelperNames()
	var wantL []string
	for n := range want {
		wantL = append(wantL, n)
	}
	sort.Strings(wantL)
	if strings.Join(got, ",") != strings.Join(wantL, ",") {
		t.Fatalf("gpulease.HeartbeatHelperNames() = %v, but render/ heartbeats through %v", got, wantL)
	}
}

// The wrapper and the helper agree on the variable name and on how the stall window relates to the beat.
func TestWrapperAndHelperAgreeOnTheEnvVarAndTheCadence(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("render", "lease-progress.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"`+gpuProgressFileEnv+`"`) {
		t.Errorf("render/lease-progress.mjs does not read %s", gpuProgressFileEnv)
	}
	m := regexp.MustCompile(`BEAT_EVERY_MS\s*=\s*([0-9_]+)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("render/lease-progress.mjs declares no BEAT_EVERY_MS")
	}
	ms, err := strconv.Atoi(strings.ReplaceAll(string(m[1]), "_", ""))
	if err != nil {
		t.Fatal(err)
	}
	if beat := time.Duration(ms) * time.Millisecond; autoProgressStall < 10*beat {
		t.Errorf("the stall window %s is under ten beats of %s: one slow poll would read as a stall", autoProgressStall, beat)
	}
}

func TestSweepMintedProgressRemovesOnlyOldMintedFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("wrap-1-1.jsonl", 8*24*time.Hour) // left by a killed wrapper
	write("wrap-2-2.jsonl", time.Hour)      // a live wrapper's
	write("notes.jsonl", 30*24*time.Hour)   // not ours
	write("wrap-3-3.log", 30*24*time.Hour)  // not ours
	if err := os.Mkdir(filepath.Join(dir, "wrap-4-4.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := gpulease.SweepMintedProgress(dir, mintedProgressMaxAge, now); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "notes.jsonl,wrap-2-2.jsonl,wrap-3-3.log,wrap-4-4.jsonl" {
		t.Errorf("left %v", names)
	}
	if n := gpulease.SweepMintedProgress(filepath.Join(dir, "absent"), time.Hour, now); n != 0 {
		t.Errorf("a missing directory is not an error: %d", n)
	}
}

// ---- the wrapper form, end to end ------------------------------------------------------------------

// TestHelperBeatProgress is the wrapped command of the tests below. It plays the part of a render helper:
// it writes the variable it was handed to LO_HELPER_ENV_OUT, then for LO_HELPER_RUN_MS it appends a line to
// that file every LO_HELPER_BEAT_MS (0 = never: a hung helper). When LO_HELPER_STOP_FILE names a file, the
// helper runs until that file exists (LO_HELPER_RUN_MS is then only the safety cap), appends one last line
// and exits at once: the test, not a stopwatch, decides how long the helper lives.
func TestHelperBeatProgress(t *testing.T) {
	run, _ := strconv.Atoi(os.Getenv("LO_HELPER_RUN_MS"))
	if run <= 0 {
		return
	}
	file, set := os.LookupEnv(gpuProgressFileEnv)
	if out := os.Getenv("LO_HELPER_ENV_OUT"); out != "" {
		body := "unset"
		if set {
			body = "set:" + file
		}
		_ = os.WriteFile(out, []byte(body), 0o644)
	}
	beat, _ := strconv.Atoi(os.Getenv("LO_HELPER_BEAT_MS"))
	stopFile := os.Getenv("LO_HELPER_STOP_FILE")
	end := time.Now().Add(time.Duration(run) * time.Millisecond)
	for time.Now().Before(end) {
		stopping := false
		if stopFile != "" {
			_, err := os.Stat(stopFile)
			stopping = err == nil
		}
		if beat > 0 && file != "" {
			if f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
				_, _ = f.WriteString(`{"detail":"ComfyUI is running prompt p1"}` + "\n")
				_ = f.Close()
			}
		}
		if stopping {
			return // the last beat is written; out at once
		}
		if beat > 0 && file != "" {
			time.Sleep(time.Duration(beat) * time.Millisecond)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// helperBeatCmd wraps this test binary as `node <script>`: the script name is what the wrapper recognises.
func helperBeatCmd(script string) []string {
	return []string{"--", os.Args[0], "-test.run=^TestHelperBeatProgress$", "-test.timeout=60s", script}
}

type reserveRun struct {
	err    error
	stderr string
}

// startReserve runs the wrapper in the background against the scratch root and returns where its result
// will arrive.
func startReserve(t *testing.T, cfg string, args ...string) chan reserveRun {
	t.Helper()
	done := make(chan reserveRun, 1)
	go func() {
		var err error
		stderr := captureStderr(t, func() {
			err = runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--wait", "0", "--reason", "render"}, args...))
		})
		done <- reserveRun{err, stderr}
	}()
	return done
}

func waitReserve(t *testing.T, done chan reserveRun) reserveRun {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(60 * time.Second):
		t.Fatal("the wrapped command never finished")
		return reserveRun{}
	}
}

func progressTestFixture(t *testing.T, stall time.Duration) (cfg string, m *gpulease.Manager) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	// The cards read idle: nothing but the progress file can vouch for the lease.
	termFixedUtil(t, false)
	oldTick, oldStall := wrapperTickEvery, autoProgressStall
	wrapperTickEvery, autoProgressStall = 150*time.Millisecond, stall
	t.Cleanup(func() { wrapperTickEvery, autoProgressStall = oldTick, oldStall })
	return leaseFixture(t)
}

// waitLimit is the instant a wait must give up: max from now, and never past the test binary's own deadline
// (less a margin for the cleanup), so a regression fails with a message instead of a package timeout.
// t.Deadline reports none when the binary runs without -test.timeout, hence the cap.
func waitLimit(t *testing.T, max time.Duration) time.Time {
	t.Helper()
	limit := time.Now().Add(max)
	if d, ok := t.Deadline(); ok {
		if e := d.Add(-10 * time.Second); e.Before(limit) {
			limit = e
		}
	}
	return limit
}

// THE DEFECT, END TO END: a healthy helper under a lease, nobody to vouch for it but its own progress file.
// The lease must renew at the end of its term and never be labelled expired while the helper keeps beating.
//
// WHY IT IS BUILT THIS WAY (it was a load flake, and widening windows around a fixed run made it worse):
//   - The helper lives as long as the TEST needs, not for a stopwatch. A helper that ran a fixed 3.5 s ended
//     while the wrapper, another process on a starved host, had not yet reaped the exit; a term check landed in
//     that gap, read a file nobody wrote to any more, and labelled the healthy lease. So the helper beats until
//     the test has seen what it needs and writes a stop file, and the lease is judged only up to that moment,
//     while the helper is beating. What the wrapper does with the silence after the helper is gone is the
//     rule of TestWrapperLabelsTheLeaseOfAHelperThatStoppedBeating, not of this one.
//   - A renewal must prove the beats moved the file. The progress file's mtime is its creation, so a helper
//     that wrote nothing still reads fresh until one stall window has passed. The term (5.5 s) is longer than
//     the window (3 s) and the test waits until the record shows a term end that came from a check made more
//     than one window after the file was created (a term end E is stamped by a check at or after E minus the
//     term, in both branches of nextEnd): that check had to find a file written to after its creation, which a
//     helper that never beats cannot give. It is labelled instead, and the wait ends in a failure.
//   - The window is thirty beats of the 100 ms helper, so a beat or two held up by a starved host is no stall.
//     Every wait is bounded, so a regression fails with a message.
func TestWrapperRenewsTheLeaseOfAHelperThatKeepsBeating(t *testing.T) {
	const (
		stall = 3 * time.Second         // thirty beats
		term  = 5500 * time.Millisecond // longer than the window by more than the wrapper takes to start the helper
		slack = 250 * time.Millisecond  // timestamp granularity and clock reads
	)
	cfg, m := progressTestFixture(t, stall)
	envOut := filepath.Join(t.TempDir(), "env.txt")
	stopFile := filepath.Join(t.TempDir(), "stop")
	t.Setenv("LO_HELPER_ENV_OUT", envOut)
	t.Setenv("LO_HELPER_STOP_FILE", stopFile)
	t.Setenv("LO_HELPER_RUN_MS", "50000") // only the safety cap: the stop file ends the helper
	t.Setenv("LO_HELPER_BEAT_MS", "100")
	done := startReserve(t, cfg, append([]string{"--for", term.String()}, helperBeatCmd("render/comfy-render.mjs")...)...)

	// finish stops the helper (once, whatever path got here) and collects the wrapper: it also runs when the
	// test fails half way, so no wrapper is left running with os.Stderr redirected into the next test.
	var r reserveRun
	finished := false
	finish := func() {
		_ = os.WriteFile(stopFile, []byte("stop"), 0o644)
		if finished {
			return
		}
		select {
		case r = <-done:
			finished = true
		case <-time.After(30 * time.Second):
			t.Error("the wrapped helper never finished after it was told to stop")
		}
	}
	t.Cleanup(finish)

	waitForLeases(t, m, 1)
	in := m.Inspect()
	if in.Progress == nil || in.Progress.StallMs != autoProgressStall.Milliseconds() {
		t.Fatalf("the wrapper must declare the contract on the lease record: %+v", in.Progress)
	}
	if in.Term != term {
		t.Fatalf("the lease records a term of %s, want the %s it was asked for", in.Term, term)
	}
	minted := in.Progress.File
	if !filepath.IsAbs(minted) || filepath.Dir(minted) != gpulease.ProgressDir(m.Root()) {
		t.Fatalf("the minted file %q is not under %s", minted, gpulease.ProgressDir(m.Root()))
	}

	// Watch the lease while the helper beats, until a renewal proves the beats (see above).
	limit := waitLimit(t, 45*time.Second)
	var created time.Time // the file's mtime when first seen: no earlier than its creation, and what the term check reads
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for proven := false; !proven; {
		select {
		case r = <-done:
			finished = true
			t.Fatalf("the wrapped helper ended before the test stopped it: %v\n%s", r.err, r.stderr)
		case <-tick.C:
		}
		in = m.Inspect()
		if in.Expired {
			finish()
			t.Fatalf("the lease of a helper that kept beating was labelled expired (the F8 incident): %s\n%s", in.ExpiredWhy, r.stderr)
		}
		if time.Now().After(limit) {
			finish()
			t.Fatalf("no term end was renewed after the stall window passed since the progress file was created (file seen at %s, term ends %s):\n%s",
				created.Format(time.RFC3339Nano), in.ExpiresAt.Format(time.RFC3339Nano), r.stderr)
		}
		if created.IsZero() {
			if fi, err := os.Stat(minted); err == nil {
				created = fi.ModTime()
			}
			continue
		}
		proven = in.ExpiresAt.Add(-in.Term).After(created.Add(stall + slack))
	}
	finish()

	if r.err != nil {
		t.Fatalf("the wrapped helper failed: %v\n%s", r.err, r.stderr)
	}
	for _, want := range []string{"declaring a progress contract for comfy-render.mjs", "was renewed to"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("the wrapper's stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	body, _ := os.ReadFile(envOut)
	if string(body) != "set:"+minted {
		t.Errorf("the helper was handed %q, want the lease's own file %q", body, minted)
	}
	if _, err := os.Stat(minted); !os.IsNotExist(err) {
		t.Errorf("the wrapper must remove the progress file it minted when the command ends (stat err %v)", err)
	}
}

// A helper that is hung (alive, wrapper heartbeating, the card busy or not) writes nothing: the file goes
// stale, the lease reads held-stalled, its term is not renewed, and the command is NOT killed.
func TestWrapperLabelsTheLeaseOfAHelperThatStoppedBeating(t *testing.T) {
	cfg, m := progressTestFixture(t, 400*time.Millisecond)
	t.Setenv("LO_HELPER_RUN_MS", "3000")
	t.Setenv("LO_HELPER_BEAT_MS", "0")
	done := startReserve(t, cfg, append([]string{"--for", "1s"}, helperBeatCmd("render/comfy-render.mjs")...)...)
	waitForLeases(t, m, 1)

	stalled, expired := false, false
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline) && !(stalled && expired); time.Sleep(25 * time.Millisecond) {
		in := m.Inspect()
		if in.Expired {
			expired = true
		}
		if st := m.StandingReadOnly(in, 0); st.Stalled {
			stalled = true
		}
	}
	if !stalled {
		t.Error("a progress file that stopped moving past its stall window must read stalled")
	}
	if !expired {
		t.Error("a term that ends with a stalled contract must be labelled expired, not renewed")
	}
	r := waitReserve(t, done)
	if r.err != nil {
		t.Fatalf("a stalled lease is a label, not a kill: the command must finish normally: %v\n%s", r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "term ended and was not renewed") || strings.Contains(r.stderr, "LEASE LOST") {
		t.Errorf("the wrapper says once that the term ended unrenewed, and the lease was not lost:\n%s", r.stderr)
	}
}

// A caller that declared its own contract keeps it: the helper is handed THAT path, nothing is minted, and
// the caller's file survives the command (it may be a log they keep).
func TestWrapperHandsDownACallersOwnProgressFileAndKeepsIt(t *testing.T) {
	cfg, m := progressTestFixture(t, 15*time.Minute)
	own := filepath.Join(t.TempDir(), "my-progress.jsonl")
	envOut := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("LO_HELPER_ENV_OUT", envOut)
	t.Setenv("LO_HELPER_RUN_MS", "600")
	t.Setenv("LO_HELPER_BEAT_MS", "100")
	done := startReserve(t, cfg, append([]string{"--for", "1h", "--progress-file", own, "--stall", "5m"}, helperBeatCmd("render/comfy-render.mjs")...)...)
	waitForLeases(t, m, 1)
	in := m.Inspect()
	if in.Progress == nil || in.Progress.File != own || in.Progress.StallMs != (5*time.Minute).Milliseconds() {
		t.Fatalf("the caller's contract must stand as declared: %+v", in.Progress)
	}
	r := waitReserve(t, done)
	if r.err != nil {
		t.Fatal(r.err, r.stderr)
	}
	if strings.Contains(r.stderr, "declaring a progress contract") {
		t.Errorf("the wrapper declared a contract over the caller's own:\n%s", r.stderr)
	}
	body, _ := os.ReadFile(envOut)
	if string(body) != "set:"+own {
		t.Errorf("the helper was handed %q, want the caller's file %q", body, own)
	}
	data, err := os.ReadFile(own)
	if err != nil || !strings.Contains(string(data), "ComfyUI is running prompt p1") {
		t.Errorf("the caller's file must survive the command with the helper's lines in it: %v %q", err, data)
	}
	if entries, _ := os.ReadDir(gpulease.ProgressDir(m.Root())); len(entries) != 0 {
		t.Errorf("nothing is minted when the caller declared a contract: %v", entries)
	}
}

// A command that is not a heartbeating helper is judged exactly as before: no contract, no variable.
func TestWrapperDeclaresNothingForACommandThatDoesNotHeartbeat(t *testing.T) {
	cfg, m := progressTestFixture(t, 15*time.Minute)
	envOut := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("LO_HELPER_ENV_OUT", envOut)
	t.Setenv("LO_HELPER_RUN_MS", "400")
	t.Setenv("LO_HELPER_BEAT_MS", "0")
	done := startReserve(t, cfg, append([]string{"--for", "1h"}, helperBeatCmd("bench.sh")...)...)
	waitForLeases(t, m, 1)
	if in := m.Inspect(); in.Progress != nil {
		t.Errorf("a bench must not be handed a progress contract it would never keep: %+v", in.Progress)
	}
	r := waitReserve(t, done)
	if r.err != nil {
		t.Fatal(r.err, r.stderr)
	}
	if strings.Contains(r.stderr, "progress contract") {
		t.Errorf("the wrapper said something about a progress contract for a bench:\n%s", r.stderr)
	}
	if body, _ := os.ReadFile(envOut); string(body) != "unset" {
		t.Errorf("the command was handed a progress file it has no lease contract for: %q", body)
	}
}
