package research

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A timeout must end the whole tree the binary started, not only the program the harness launched. On Windows the
// documented shape is a wrapper .cmd that runs the interpreter, so the program that holds the scratch directory is a
// grandchild; on POSIX it is whatever a wrapper script backgrounds. treeWrapper (a per-OS file) builds that wrapper:
// it starts a sleeper, which writes its pid to the returned file.

func readPID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

func waitUntilDead(pid int, within time.Duration) bool {
	for end := time.Now().Add(within); ; time.Sleep(25 * time.Millisecond) {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

func TestSubtitleTimeoutEndsTheWholeProcessTree(t *testing.T) {
	pinYouTube(t)
	wrapper, pidFile := treeWrapper(t)
	opt, root := ytOpts(t)
	opt.YtDlpPath = wrapper

	// the run is ended once the grandchild exists, so the cancel never comes before the process it has to reach
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pidc := make(chan int, 1)
	go func() {
		defer cancel()
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if pid := readPID(pidFile); pid > 0 {
				pidc <- pid
				return
			}
		}
		pidc <- 0
	}()
	got := Fetch(ctx, "https://www.youtube.com/watch?v="+idPlain, opt)
	pid := <-pidc
	if pid == 0 {
		t.Fatalf("test premise: the wrapper never started its grandchild (result %+v)", got)
	}
	t.Cleanup(func() {
		if processAlive(pid) {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	})

	if got.ErrKind != ErrKindTimeout {
		t.Fatalf("got %+v, want %s", got, ErrKindTimeout)
	}
	if !waitUntilDead(pid, 5*time.Second) {
		t.Errorf("the grandchild (pid %d) is still running after the run ended: only the direct child was killed", pid)
	}
	if left := leftovers(t, root); len(left) != 0 && got.CleanupErr == "" {
		t.Errorf("the scratch directory %v is still there and the failure to remove it was not recorded (CleanupErr is empty)", left)
	}
	if got.CleanupErr != "" {
		t.Logf("scratch removal failure recorded: %s", got.CleanupErr)
	}
}

// A binary that exits 0 while a child it started still holds the output pipe is a success once WaitDelay has run
// out: exec reports ErrWaitDelay for it, and the subtitle file is there to read.
func TestSubtitleExitZeroWithALingeringPipeHolderIsSuccess(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idLinger, opt)
	// the lingering child is a copy of the test binary: let it end before the test does, so nothing holds the
	// binary or the scratch root while they are cleaned up
	if pid := readPID(filepath.Join(root, "linger.pid")); pid > 0 {
		waitUntilDead(pid, 15*time.Second)
	}
	if got.Err != "" || got.ErrKind != "" {
		t.Fatalf("a clean exit with a lingering pipe holder was reported as a failure: %s (%s)", got.Err, got.ErrKind)
	}
	if got.Text != "Hello & welcome.\nSecond cue here." {
		t.Errorf("text = %q", got.Text)
	}
}

// ---- the scratch directory ------------------------------------------------

func TestStaleScratchDirectoriesAreSweptAtTheNextCall(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	age := func(path string, by time.Duration) {
		t.Helper()
		when := time.Now().Add(-by)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(name string, by time.Duration) string {
		t.Helper()
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "part.vtt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		age(d, by)
		return d
	}
	stale := mkdir("ytdlp-stale", 3*time.Hour)
	fresh := mkdir("ytdlp-fresh", time.Minute)
	foreign := mkdir("somebody-elses-old", 3*time.Hour)
	oldFile := filepath.Join(root, "ytdlp-a-file")
	if err := os.WriteFile(oldFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(oldFile, 3*time.Hour)

	if got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt); got.Err != "" {
		t.Fatalf("fetch failed: %s", got.Err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a ytdlp-* directory older than any run can be was not swept (stat err %v)", err)
	}
	for _, keep := range []string{fresh, foreign, oldFile} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s must be left alone: %v", filepath.Base(keep), err)
		}
	}
}

func TestScratchRemovalIsRetriedBriefly(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	real := removeAll
	attempts := 0
	removeAll = func(p string) error {
		attempts++
		if attempts < 3 {
			return errors.New("busy")
		}
		return real(p)
	}
	t.Cleanup(func() { removeAll = real })

	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
	if got.Err != "" || got.CleanupErr != "" {
		t.Fatalf("a removal that works on the third try is no failure: err %q cleanup %q", got.Err, got.CleanupErr)
	}
	if attempts != 3 {
		t.Errorf("removal attempted %d times, want 3 (two failures and the success)", attempts)
	}
	if left := leftovers(t, root); len(left) != 0 {
		t.Errorf("per-call directory left behind: %v", left)
	}
}

func TestAScratchRemovalThatKeepsFailingIsRecordedAndBounded(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	real := removeAll
	attempts := 0
	removeAll = func(string) error { attempts++; return errors.New("busy: held by another process") }
	t.Cleanup(func() { removeAll = real })

	start := time.Now()
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
	if got.Err != "" || got.Text == "" {
		t.Fatalf("a scratch directory that cannot be removed must not cost the text: %+v", got)
	}
	if !strings.Contains(got.CleanupErr, "busy") {
		t.Errorf("CleanupErr = %q, want the removal error recorded", got.CleanupErr)
	}
	if attempts < 2 || attempts > 10 {
		t.Errorf("removal attempted %d times, want a few (retried, but bounded)", attempts)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the retries took %v, want a few hundred milliseconds", took)
	}

	// the same on a run that failed: the typed error stays, the note rides beside it
	attempts = 0
	broke := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idBreak, opt)
	if broke.ErrKind != ErrKindSiteBreak || !strings.Contains(broke.CleanupErr, "busy") {
		t.Errorf("got kind %q cleanup %q, want %s and the removal error", broke.ErrKind, broke.CleanupErr, ErrKindSiteBreak)
	}
}
