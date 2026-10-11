package delegate

import (
	"bytes"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
)

// quietLogKeep is how much log text the suite's ring remembers: enough to read the
// lines around a failure, small enough that a chatty run never grows without bound.
const quietLogKeep = 256 << 10

// logRing is a log sink that never waits: it keeps the newest quietLogKeep bytes in
// memory and drops the rest. The standard logger's default sink is the process's
// stderr, and a write to it is a synchronous call into whatever stands behind that
// handle: a file on a busy disk, or the pipe `go test` reads late when the box is
// loaded. This package's tests race real wall-clock windows (a 300 ms call deadline,
// a 250 ms unwind allowance), and the code under test logs inside them (a probe that
// failed, a withdraw that was not confirmed), so a stalled write inside the window
// reads as a slow node. Measured on a slow Windows PC (register F66): the test that
// fails most often there ran 64 times with the logger on stderr and failed 8 times,
// the goroutine the call had to abandon stuck in log.Printf's WriteFile and its seat
// starting up to 1.3 s late; with the logger on io.Discard it ran 64 times and
// failed 0, its slowest start 13 ms.
type logRing struct {
	mu  sync.Mutex
	buf []byte
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - quietLogKeep; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	return len(p), nil
}

// text is what the ring holds, cut to start at a line boundary when it dropped the head.
func (r *logRing) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := string(r.buf)
	if len(r.buf) >= quietLogKeep {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s
}

// runQuietLog runs the suite with the standard logger on a logRing and, only when the
// suite failed, replays the ring to stderr: a green run prints nothing, and a red one
// keeps the log lines `go test` would have shown, now in one block after the failures.
// A test that asserts on log lines still takes the logger itself (captureLog) and puts
// it back to the ring.
func runQuietLog(run func() int, stderr io.Writer) int {
	ring := &logRing{}
	old := log.Writer()
	log.SetOutput(ring)
	code := run()
	log.SetOutput(old)
	if code != 0 {
		if text := ring.text(); text != "" {
			_, _ = io.WriteString(stderr, "delegate tests: the log lines of the failed run (the newest "+
				"256 KiB; the logger was kept off stderr so a slow stderr cannot stall a timed test):\n"+text)
		}
	}
	return code
}

// TestMainKeepsTheLoggerOffStderr pins that the suite's logger is the ring, so a
// log.Printf in the code under test cannot wait on stderr inside a timed window.
func TestMainKeepsTheLoggerOffStderr(t *testing.T) {
	if _, ok := log.Writer().(*logRing); !ok {
		t.Fatalf("the standard logger writes to %T, want the suite's logRing (TestMain runs the suite under runQuietLog)", log.Writer())
	}
}

func TestRunQuietLogReplaysTheRingOnlyWhenTheSuiteFailed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     int
		wantText bool
	}{{"a green suite prints nothing", 0, false}, {"a red suite replays its log", 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			outer := log.Writer()
			var out bytes.Buffer
			got := runQuietLog(func() int {
				if _, ok := log.Writer().(*logRing); !ok {
					t.Errorf("during the run the logger writes to %T, want the ring", log.Writer())
				}
				log.Print("a line the run logged")
				return tc.code
			}, &out)
			if got != tc.code {
				t.Fatalf("runQuietLog = %d, want the suite's own code %d", got, tc.code)
			}
			if log.Writer() != outer {
				t.Fatal("the logger was not put back after the run")
			}
			if has := strings.Contains(out.String(), "a line the run logged"); has != tc.wantText {
				t.Fatalf("replayed = %v, want %v; stderr got %q", has, tc.wantText, out.String())
			}
		})
	}
}

func TestLogRingKeepsTheNewestLinesAndNeverFails(t *testing.T) {
	r := &logRing{}
	line := strings.Repeat("x", 99) + "\n"
	total := quietLogKeep/len(line) + 50
	for i := 0; i < total; i++ {
		if n, err := r.Write([]byte(line)); n != len(line) || err != nil {
			t.Fatalf("Write = %d, %v: a log sink must never fail the logger", n, err)
		}
	}
	if _, err := r.Write([]byte("the newest line\n")); err != nil {
		t.Fatal(err)
	}
	got := r.text()
	if len(got) > quietLogKeep {
		t.Fatalf("the ring holds %d bytes, want at most %d", len(got), quietLogKeep)
	}
	if !strings.HasSuffix(got, "the newest line\n") {
		t.Fatalf("the ring lost its newest line: ...%q", got[max(0, len(got)-40):])
	}
	if !strings.HasPrefix(got, line) {
		t.Fatalf("the ring starts mid-line: %q...", got[:min(len(got), 40)])
	}
}
