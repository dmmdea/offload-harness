package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Register D-101 (ADR 0085): the session and run class a fleet node's row is stamped with come from the wire, so
// the cleaners that decide whether a label is valid are exported and used at both ends, and Record keeps a
// session its caller named while still saying which process recorded the row.

// A row that names a session but no process (a fleet node's row for an asker's session) takes the pids of the
// process that recorded it: "process P recorded this while serving session S", the documented meaning of the
// columns. A caller that names a pid keeps both.
func TestStampOriginKeepsACallerSessionButFillsThePids(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Task: "agent", OriginSession: "sess-abc"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Task: "agent", OriginSession: "other", OriginPID: 99}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	rows, err := ReadAll(p)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %d (%v), want 2", len(rows), err)
	}
	if rows[0].OriginSession != "sess-abc" || rows[0].OriginPID != os.Getpid() || rows[0].OriginPPID != os.Getppid() {
		t.Fatalf("session-only row = session %q pid %d ppid %d, want sess-abc and this process (%d, %d)",
			rows[0].OriginSession, rows[0].OriginPID, rows[0].OriginPPID, os.Getpid(), os.Getppid())
	}
	if rows[1].OriginSession != "other" || rows[1].OriginPID != 99 {
		t.Fatalf("a caller-named process must be kept: %+v", rows[1])
	}
}

// The session rule, as the wire applies it: printable ASCII (interior spaces allowed), at most 96 bytes, and a
// value over the cap is refused whole. It is never cut to length: a truncated id would name another session.
func TestCleanOriginIsTheWireRule(t *testing.T) {
	for in, want := range map[string]string{
		"sess-abc":                     "sess-abc",
		"  sess-abc  ":                 "sess-abc",
		"two words":                    "two words",
		strings.Repeat("x", 96):        strings.Repeat("x", 96),
		strings.Repeat("x", 97):        "",
		strings.Repeat("x", 300):       "",
		"":                             "",
		"   ":                          "",
		"\x1b[31mred":                  "",
		"line\nbreak":                  "",
		"café":                         "",
		"del\x7f":                      "",
		"  " + strings.Repeat("x", 96): strings.Repeat("x", 96),
	} {
		if got := CleanOrigin(in); got != want {
			t.Errorf("CleanOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// The class rule, as the wire applies it: a short [a-z0-9_-] token, lower-cased, and "production" is no class.
func TestCleanClassIsTheWireRule(t *testing.T) {
	for in, want := range map[string]string{
		"bake":                  "bake",
		"Bake ":                 "bake",
		"soak-2_b":              "soak-2_b",
		strings.Repeat("a", 32): strings.Repeat("a", 32),
		strings.Repeat("a", 33): "",
		"x y":                   "",
		"production":            "",
		"PROD":                  "",
		"":                      "",
		"bäke":                  "",
		"bake;rm":               "",
	} {
		if got := CleanClass(in); got != want {
			t.Errorf("CleanClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// SessionFromEnv is the session ProcessOrigin would resolve from the same environment (the explicit label wins
// over the inherited session id, and a refused label falls through), without the pid walk.
func TestSessionFromEnvMatchesTheOriginResolver(t *testing.T) {
	env := map[string]string{"LOCAL_OFFLOAD_ORIGIN": " opencode-9 ", "CLAUDE_CODE_SESSION_ID": "7c561f0b"}
	if got := SessionFromEnv(envMap(env)); got != "opencode-9" {
		t.Fatalf("explicit label = %q, want opencode-9", got)
	}
	env["LOCAL_OFFLOAD_ORIGIN"] = strings.Repeat("x", 97)
	if got := SessionFromEnv(envMap(env)); got != "7c561f0b" {
		t.Fatalf("a refused label must fall through to the session id, got %q", got)
	}
	if got := SessionFromEnv(envMap(nil)); got != "" {
		t.Fatalf("no environment = %q, want empty", got)
	}
	for _, e := range []map[string]string{env, {"CLAUDE_CODE_SESSION_ID": "s1"}, nil} {
		if a, b := SessionFromEnv(envMap(e)), originFrom(envMap(e), 1, 1).Session; a != b {
			t.Fatalf("SessionFromEnv %q != originFrom %q for %v", a, b, e)
		}
	}
}

// A session label can carry a class of its own, and the node refuses such a label from a caller it cannot
// authenticate (ClassOfSession is the question it asks).
func TestClassOfSessionReadsTheClassALabelCarries(t *testing.T) {
	for in, want := range map[string]string{
		"sess-abc":        "",
		"bake:run-7":      "bake",
		"measurement:x":   "measurement",
		"test:unit":       "test",
		"go-test":         "test",
		"tenant:acme":     "",
		"":                "",
		"bake":            "",
		"production:real": "",
	} {
		if got := ClassOfSession(in); got != want {
			t.Errorf("ClassOfSession(%q) = %q, want %q", in, got, want)
		}
	}
}
