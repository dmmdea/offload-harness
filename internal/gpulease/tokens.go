package gpulease

// Place-keeping tokens (plan P13, invariant I4).
//
// "A busy card is a place in line" was true of a caller that stayed: a `gpu reserve` waits as
// long as it is told to. A media tool call cannot stay. It waits its window (gpu_wait_ms, 90 s)
// and must then answer, and the answer used to be a refusal ("gpu busy") that sent the caller
// to the back of the line on its next try, or away for good. A token is the other answer: the
// call leaves a record of its place and hands the caller its name; the caller re-calls with the
// name and the call resumes the place it left, with the arrival time it had.
//
// WHY A TOKEN IS NOT A WAITER. A waiter record is a process that is polling: its pid, and a
// heartbeat it refreshes every tick, are what let everyone else tell it from debris. A token has
// no process behind it. The MCP server that wrote it is alive for as long as the client's
// session is, which says nothing about whether the client is coming back. So a token's life is
// its LAST POLL, with two clocks:
//
//   - TokenGrace (30 s): the place is held for this long after the poller left. Later waiters
//     on the same cards queue behind it, so a client that re-calls promptly does not lose its
//     turn between two calls.
//   - after that the token is ABSENT. Every waiter skips it, and a whole-node barrier ignores
//     it, so a client that wandered off never blocks the line.
//   - TokenTTL (10 min): an absent token can still be resumed, with its original arrival time,
//     so a client that comes back is ahead of everyone who arrived after it. Past that it is
//     pruned.
//
// WHERE THEY LIVE. In <gpu>/tokens, never among the waiters. A binary that predates tokens prunes
// every waiter record whose process has stopped refreshing it, and would delete a token it can
// neither refresh nor read; it does not look in this directory. It also does not honour tokens,
// so on a host that mixes versions an old binary can take a card ahead of a token holder. That
// costs the holder its place, never exclusivity: the O_EXCL claim is still the only arbiter of
// who HOLDS a card.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	tokensDirName = "tokens"

	// TokenGrace is how long after its poller left a token still holds its place in line.
	TokenGrace = 30 * time.Second
	// TokenTTL is how long after its poller left a token can still be resumed.
	TokenTTL = 10 * time.Minute
)

// tokenIDPattern is what a token id looks like. It arrives from a tool caller, so it is checked
// before it is ever turned into a path.
var tokenIDPattern = regexp.MustCompile(`^tk-[a-z0-9]{8,32}$`)

// Token is one place in line left by a call that could not wait any longer.
type Token struct {
	ID    string `json:"id"`
	Class Class  `json:"class"`
	// Reason is what the call was for, as the line shows it.
	Reason string `json:"reason,omitempty"`
	// Devices are the cards the call wants (lease ids); empty = the whole node.
	Devices []string `json:"devices,omitempty"`
	// SinceMs is when the call first joined the line: the arrival time every resume keeps.
	SinceMs int64 `json:"since_ms"`
	// PolledMs is when the call last proved it was there: the clock the grace and the TTL run on.
	PolledMs int64 `json:"polled_ms"`
}

// Since is when the call first joined the line.
func (t Token) Since() time.Time { return time.UnixMilli(t.SinceMs) }

func (m *Manager) tokensDir() string { return filepath.Join(m.gpuDir(), tokensDirName) }

func (m *Manager) grace() time.Duration {
	if m.tokenGrace > 0 {
		return m.tokenGrace
	}
	return TokenGrace
}

func tokenPath(dir, id string) string { return filepath.Join(dir, id+".json") }

// LeaveToken records that a call gave up its wait still wanting a card, and returns the token
// that names its place. since is when the call first joined the line (a call that resumed a
// token passes the arrival time it resumed with, so every give-up keeps the original place).
// opts.ResumeToken, when it is a well-formed id, is reused: the caller keeps one name for its
// place across re-calls. opts.Devices is the set it wants (empty = the whole node).
func (m *Manager) LeaveToken(class Class, opts Options, since time.Time) (Token, error) {
	if !class.Valid() {
		return Token{}, fmt.Errorf("gpulease: unknown class %q", class)
	}
	devs, err := NormalizeDevices(opts.Devices)
	if err != nil {
		return Token{}, err
	}
	id := strings.TrimSpace(opts.ResumeToken)
	if !tokenIDPattern.MatchString(id) {
		id = "tk-" + randomToken()
	}
	t := Token{
		ID: id, Class: class, Reason: clipCommand(opts.Reason), Devices: devs,
		SinceMs: since.UnixMilli(), PolledMs: m.now().UnixMilli(),
	}
	b, err := json.Marshal(t)
	if err != nil {
		return Token{}, err
	}
	dir := m.tokensDir()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return Token{}, fmt.Errorf("gpulease: cannot keep a place in line: %w", err)
	}
	path := tokenPath(dir, id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return Token{}, fmt.Errorf("gpulease: cannot keep a place in line: %w", err)
	}
	if err := renameReplacing(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return Token{}, fmt.Errorf("gpulease: cannot keep a place in line: %w", err)
	}
	return t, nil
}

// tokenRead says what reading one token file found.
type tokenRead int

const (
	tokenOK   tokenRead = iota
	tokenGone           // missing, or unreadable right now (a rename in flight): leave it alone
	tokenBad            // readable but not a token: debris
)

// readToken parses one token file.
func readToken(path string) (Token, tokenRead) {
	b, err := readWaiterFile(path)
	if err != nil {
		return Token{}, tokenGone
	}
	var t Token
	if json.Unmarshal(b, &t) != nil || !tokenIDPattern.MatchString(t.ID) {
		return Token{}, tokenBad
	}
	return t, tokenOK
}

// ResumeToken finds a token that can still be resumed: its poller left no more than TokenTTL ago.
// It does not consume it; registering a waiter that resumes it does. A malformed id, an id that
// names no token and an expired token all answer false: the call is then a new arrival, never an
// error.
func (m *Manager) ResumeToken(id string) (Token, bool) {
	id = strings.TrimSpace(id)
	if !tokenIDPattern.MatchString(id) {
		return Token{}, false
	}
	path := tokenPath(m.tokensDir(), id)
	t, st := readToken(path)
	if st != tokenOK {
		return Token{}, false
	}
	if m.now().Sub(time.UnixMilli(t.PolledMs)) > TokenTTL {
		_ = os.Remove(path)
		return Token{}, false
	}
	return t, true
}

// DropToken forgets a token (the call got its card, or no longer wants it). A malformed id, or one
// that names nothing, is harmless.
func (m *Manager) DropToken(id string) {
	id = strings.TrimSpace(id)
	if !tokenIDPattern.MatchString(id) {
		return
	}
	_ = removeClaim(tokenPath(m.tokensDir(), id))
}

// Tokens lists the places in line that can still be resumed (not past TokenTTL since their last
// poll), oldest arrival first. Expired and unreadable records are removed as they are read. A
// token inside its grace holds its place; one past it is listed but skipped for ordering
// (tokenLive).
func (m *Manager) Tokens() []Token {
	dir := m.tokensDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Token
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		t, st := readToken(path)
		switch st {
		case tokenGone:
			continue
		case tokenBad:
			_ = os.Remove(path)
			continue
		}
		if m.now().Sub(time.UnixMilli(t.PolledMs)) > TokenTTL {
			_ = os.Remove(path)
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SinceMs != out[j].SinceMs {
			return out[i].SinceMs < out[j].SinceMs
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// tokenLive reports whether t still holds its place: its poller left within the grace.
func (m *Manager) tokenLive(t Token) bool {
	return m.now().Sub(time.UnixMilli(t.PolledMs)) <= m.grace()
}

// tokenBlocks reports whether a live token ahead of self wants cards self wants. A token is a
// waiter that is not there: it holds its place for the grace and is ignored after it, by every
// waiter, a whole-node barrier included. self's own token (a waiter that resumed one) never
// blocks it.
func (m *Manager) tokenBlocks(self Waiter) bool {
	for _, t := range m.Tokens() {
		if t.ID == self.Token || !m.tokenLive(t) {
			continue
		}
		if t.SinceMs < self.SinceMs && devicesConflict(t.Devices, self.Devices) {
			return true
		}
	}
	return false
}

// QueuePosition is where a request for devices (empty = the whole node) that joined the line at
// since stands: 1 plus the waiters and live tokens that arrived before it and want any of the
// same cards. exclude names a token that is the caller's own (never counted).
func (m *Manager) QueuePosition(devices []string, since time.Time, exclude string) int {
	sinceMs := since.UnixMilli()
	n := 1
	for _, w := range m.Waiters() {
		if exclude != "" && w.Token == exclude {
			continue
		}
		if w.SinceMs < sinceMs && devicesConflict(w.Devices, devices) {
			n++
		}
	}
	for _, t := range m.Tokens() {
		if t.ID == exclude || !m.tokenLive(t) {
			continue
		}
		if t.SinceMs < sinceMs && devicesConflict(t.Devices, devices) {
			n++
		}
	}
	return n
}
