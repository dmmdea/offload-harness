package ledger

import (
	"os"
	"strings"
	"sync"
)

// Origin is the identity of the PROCESS that recorded a row: the session it
// served, and the writer's pid/ppid for a reader that groups by process.
//
// Why (register D-101, 2026-09-15): the harness-share gate enforces a
// per-session floor by summing this ledger, and until 0.124.0 every row was
// anonymous — the hook summed every row in the session's time window, so a
// session was credited with every concurrent session's delegations, and a
// session that routed everything it had could still be blocked. One MCP server
// is one Claude Code session and inherits that session's environment, so the
// session id is free at record time; no process-tree walk is needed.
type Origin struct {
	// Session names the calling session. LOCAL_OFFLOAD_ORIGIN when set (a
	// non-Claude caller naming itself), else CLAUDE_CODE_SESSION_ID — which
	// Claude Code exports to every child process, MCP servers included
	// (verified on three live servers, 2026-09-15). Empty for a service, a
	// fleet node, or a bare shell: unattributed, never "some other session".
	Session string
	PID     int
	PPID    int
}

// maxOriginLen bounds the session label the way the dispatch tenant is
// bounded: printable ASCII, at most 96 bytes, else anonymous — a row must stay
// one small O_APPEND-atomic line whatever the environment holds.
const maxOriginLen = 96

var (
	originOnce sync.Once
	originVal  Origin
)

// ProcessOrigin resolves this process's origin ONCE: a long-lived MCP server's
// environment never changes, so a per-row read would only add a per-row
// inconsistency risk. Record stamps it on every row that carries none.
func ProcessOrigin() Origin {
	originOnce.Do(func() { originVal = originFrom(os.Getenv, os.Getpid(), os.Getppid()) })
	return originVal
}

// originFrom is the pure resolver behind ProcessOrigin; tests feed it a map.
// The explicit label wins over the inherited session id so a wrapper that
// knows better (a printed CLI, an opencode session) can name itself.
func originFrom(getenv func(string) string, pid, ppid int) Origin {
	o := Origin{PID: pid, PPID: ppid}
	for _, key := range []string{"LOCAL_OFFLOAD_ORIGIN", "CLAUDE_CODE_SESSION_ID"} {
		if v := cleanOrigin(getenv(key)); v != "" {
			o.Session = v
			return o
		}
	}
	return o
}

// cleanOrigin trims s and refuses anything that is not printable ASCII within
// maxOriginLen, so a malformed or hostile environment value cannot bloat or
// break the one-line record.
func cleanOrigin(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxOriginLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}

// stampOrigin fills the provenance columns from ProcessOrigin when the caller
// set none — every writer in the binary is attributed by this one rule, and a
// caller that names an origin of its own (a node re-recording a delegator's
// job) keeps it.
func stampOrigin(e *Entry) {
	if e.OriginSession != "" || e.OriginPID != 0 {
		return
	}
	o := ProcessOrigin()
	e.OriginSession, e.OriginPID, e.OriginPPID = o.Session, o.PID, o.PPID
}

// cardsTokens is the row's tokens through the cards (see Entry.CardsTokens):
// the seat's prompt work plus what it generated. TokensIn and SeatTokensIn are
// the same prompt measurement under two names (never both set on one row), so
// the larger is the prompt figure. A cache hit did no card work. A completed
// row that recorded no token counts at all (a render, whose "tokens" are its
// prompt) is estimated from input_chars the way the share gate always did
// (chars/4); a row that deferred before any model ran is 0, not an estimate.
func cardsTokens(e Entry) int {
	if e.CacheHit {
		return 0
	}
	in := e.TokensIn
	if e.SeatTokensIn > in {
		in = e.SeatTokensIn
	}
	if in == 0 && e.TokensOut == 0 {
		if e.Deferred || e.InputChars <= 0 {
			return 0
		}
		return (e.InputChars + 2) / 4
	}
	return in + e.TokensOut
}
