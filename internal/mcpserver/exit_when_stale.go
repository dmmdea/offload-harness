package mcpserver

// exit_when_stale.go: an idle MCP server whose image changed on disk exits by itself, so the client starts the installed
// build at its next call.
//
// The stale-image and stale-config notices (stale_image.go, stale_notice.go, stalebinary.go, stale_config.go) only SAY that a
// server runs an old image, and the way out they name (/mcp, Reconnect) is one nobody takes across a few dozen sessions. Measured
// on one box 2.5 days after two releases: 36 such servers were running, 7 from a build two releases old and 29 from the previous
// one, and none ran the installed build, so a fix that had shipped reached no session. The agent client this was built for
// (clientInfo.name "claude-code", checked with version 2.1.330.0) starts a stopped stdio server again by itself at the next tool
// call: a server was stopped, the next call of that session was answered normally by a NEW process that ran the installed build,
// and no error reached the model.
//
// So a server that finds itself stale and idle returns from Run, and the client does the rest. Everything here is about being
// sure the exit costs nothing:
//
//   - Client gate. The name the client sent in `initialize` must be on exitWhenStaleClients (compared case-insensitively) and its
//     version at or above exitWhenStaleMinClient, the build the restart was seen with. Whether another client, or an older
//     build, starts a stopped server again is not verified, so for those the notice is all there is, as before.
//   - Operator switch. mcp_exit_when_stale (default on) turns it off.
//   - Idle. No tools/call is in flight and none ended in the last exitStaleSettle (the middleware returns before the SDK writes the
//     answer, so the settle is what makes the last answer written).
//   - A definite verdict. The image check says stale (never "cannot read", never "no startup identity"), and the installed file is
//     at least exitStaleMinAge old: a deploy still being written, or rolled back a moment later, is not acted on. A changed
//     CONFIG file is not a reason: a server must never restart onto a config it cannot load, the only check that one loads is
//     config.Load, and that loader installs process-wide gates (the GPU lease root and the load fence among them) on every
//     exit, the error exits included, so a running server must not call it. A changed config keeps its notice (stale_config.go).
//   - Up for exitStaleMinUptime, so a misfire can never become a restart loop.
//   - Nothing that outlives a tool call depends on this process (ownsLiveWork): no media ticket it runs, and no live GPU lease
//     owned by the session it serves. The session registry lists THIS process as the one carrying the session, and a lease whose
//     session has no live registered process reads as owner gone and, past the grace, as orphaned, with the takeover command
//     shown to every other session. A server that left while its session held a render's lease would have put that render up
//     for takeover, so it stays until the lease is released.
//
// The exit is Run returning nil (the deferred cleanup runs, the session registry entry is removed, the exit code stays 0); the
// server never calls os.Exit from the middle of a request.

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
)

// exitWhenStaleClients are the clients (clientInfo.name from `initialize`, compared case-insensitively) known to start a stopped
// stdio server again at their next tool call. A client is added here only after that was seen with a real session.
var exitWhenStaleClients = []string{"claude-code"}

// The intervals are variables so a test does not wait real tens of seconds.
var (
	exitStaleEvery     = 30 * time.Second // the background check: one stat while nothing moved
	exitStaleSettle    = 5 * time.Second  // no call may have ended more recently than this
	exitStaleMinAge    = 10 * time.Second // the changed file must be at least this old
	exitStaleMinUptime = 60 * time.Second // the process must have been up this long
	exitStaleSlack     = 250 * time.Millisecond
)

// exitWatch is the state the exit decision reads: who the client is, which calls are running and when the last one ended.
// The zero value is ready; a Server that was never Run only records (nothing exits it).
type exitWatch struct {
	mu            sync.Mutex
	clientName    string
	clientVersion string
	inflight      int
	lastEnd       time.Time
	started       time.Time
	exited        bool
	kick          chan struct{}
}

func (w *exitWatch) client() (name, version string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.clientName, w.clientVersion
}

func (w *exitWatch) hasExited() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exited
}

// kicked is the channel a finished tool call signals on, so the check also runs shortly after a call.
func (w *exitWatch) kicked() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.kick == nil {
		w.kick = make(chan struct{}, 1)
	}
	return w.kick
}

func (w *exitWatch) signal() {
	w.mu.Lock()
	k := w.kick
	w.mu.Unlock()
	if k == nil {
		return
	}
	select {
	case k <- struct{}{}:
	default:
	}
}

// noteClient keeps the client's name and version from the initialize parameters the SDK holds (nil, or without clientInfo: nothing).
func (w *exitWatch) noteClient(p *mcp.InitializeParams) {
	if p == nil || p.ClientInfo == nil {
		return
	}
	w.mu.Lock()
	w.clientName, w.clientVersion = p.ClientInfo.Name, p.ClientInfo.Version
	w.mu.Unlock()
}

// exitWatchMiddleware records the client's name and version from `initialize` and counts the tools/call requests in flight. It
// is registered last, so it wraps outside every other middleware: a call counts as running until the whole chain has returned.
func (s *Server) exitWatchMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "initialize" {
			if p, ok := req.GetParams().(*mcp.InitializeParams); ok {
				s.xw.noteClient(p)
			}
		} else if name, _ := s.xw.client(); name == "" {
			// A client of the per-request protocol (clientInfo in each request's _meta, which the SDK's own client speaks) sends no
			// initialize at all; the session holds what it named from its first request on.
			if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
				s.xw.noteClient(ss.InitializeParams())
			}
		}
		switch method {
		case "tools/call":
			s.xw.mu.Lock()
			s.xw.inflight++
			s.xw.mu.Unlock()
			defer func() {
				s.xw.mu.Lock()
				s.xw.inflight--
				s.xw.lastEnd = time.Now()
				s.xw.mu.Unlock()
				s.xw.signal()
			}()
		}
		return next(ctx, method, req)
	}
}

// exitGate is the client gate and the operator switch: whether this server may exit by itself, and when it may not, why in a few
// words (offload_status's process block carries it as exit_when_stale).
func (s *Server) exitGate() (armed bool, why string) {
	if s.p != nil && !s.p.Cfg().MCPExitWhenStaleOn() {
		return false, "switched off by config (mcp_exit_when_stale is false)"
	}
	name, _ := s.xw.client()
	if name == "" {
		return false, "no client seen yet"
	}
	listed := false
	for _, c := range exitWhenStaleClients {
		if strings.EqualFold(strings.TrimSpace(name), c) {
			listed = true
		}
	}
	if !listed {
		return false, fmt.Sprintf("client %q is not on the list (%s)", name, strings.Join(exitWhenStaleClients, ", "))
	}
	if _, version := s.xw.client(); !versionAtLeast(version, exitWhenStaleMinClient) {
		return false, fmt.Sprintf("client version %q is not %s or later, the oldest whose restart of a stopped server was seen", version, dotted(exitWhenStaleMinClient))
	}
	return true, ""
}

// exitWhenStaleMinClient is the oldest version of a listed client whose restart of a stopped stdio server was seen with a real
// session. An older build, and a version that cannot be read, keeps the notice alone: a client that does not start the server
// again would leave its session with no harness tools and no message.
var exitWhenStaleMinClient = []int{2, 1, 330}

// versionAtLeast reports whether a dotted version ("2.1.330", "2.1.330.0", "2.2.0-beta.1") is at or above min, component by
// component. A version with no leading number is not.
func versionAtLeast(version string, min []int) bool {
	var got []int
	for _, part := range strings.Split(strings.TrimSpace(version), ".") {
		n, digits := 0, 0
		for _, c := range part {
			if c < '0' || c > '9' || digits == 9 {
				break
			}
			n, digits = n*10+int(c-'0'), digits+1
		}
		if digits == 0 {
			break
		}
		got = append(got, n)
		if digits != len(part) {
			break // "330-beta": the number counts and nothing after it does
		}
	}
	if len(got) == 0 {
		return false
	}
	for i, m := range min {
		g := 0
		if i < len(got) {
			g = got[i]
		}
		if g != m {
			return g > m
		}
	}
	return true
}

func dotted(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ".")
}

// exitArmed is the gate as a bool, for the notices (which say the server exits by itself only when it will).
func (s *Server) exitArmed() bool {
	armed, _ := s.exitGate()
	return armed
}

// exitWhenStaleState is the value of exit_when_stale in the process block: "armed", "armed, held by: <what this process still
// owns>", or "off: <why>".
func (s *Server) exitWhenStaleState() string {
	if armed, why := s.exitGate(); !armed {
		return "off: " + why
	}
	if held := s.ownsLiveWork(); held != "" {
		return "armed, held by: " + held
	}
	return "armed"
}

// ownsLiveWork says why an armed, idle server must not exit yet: work that outlives a tool call and is lost, or reads as
// abandoned, when this process goes. "" when there is none.
//   - A media ticket this process runs (mediaremote.OwnedLive): the ticket's request body lives here only.
//   - A live GPU lease owned by the session this server serves: this process is that session's entry in the session registry
//     (registerSession), so without it the lease's owner reads as gone and the lease, past the grace, as orphaned.
//
// A server that resolves no lease directory registered no session either (registerSession returns before it writes), so no
// lease can read as orphaned for its going and nothing holds it.
func (s *Server) ownsLiveWork() string {
	if s.p == nil {
		return ""
	}
	cfg := s.p.Cfg()
	if n := mediaremote.OwnedLive(cfg); n > 0 {
		return fmt.Sprintf("%d media ticket(s) this process runs have not ended", n)
	}
	if s.originSession == nil {
		return ""
	}
	session := s.originSession()
	if session == "" {
		return ""
	}
	dir, err := gpulease.LeaseDir(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		return ""
	}
	for _, l := range gpulease.InspectLeases(dir) {
		if l.Owner != nil && l.Owner.Session == session {
			return fmt.Sprintf("the session this server serves owns a live GPU lease (%s, epoch %d)", l.Class, l.Epoch)
		}
	}
	return ""
}

// idleLocked: no call in flight and none ended within the settle. w.mu is held.
func (w *exitWatch) idleLocked(now time.Time) bool {
	return w.inflight == 0 && (w.lastEnd.IsZero() || now.Sub(w.lastEnd) >= exitStaleSettle)
}

// exitReason is "" while the server stays, and otherwise the line it logs as it exits. It also marks the server as exiting.
func (s *Server) exitReason(now time.Time) string {
	if armed, _ := s.exitGate(); !armed {
		return ""
	}
	w := &s.xw
	w.mu.Lock()
	ok := !w.started.IsZero() && now.Sub(w.started) >= exitStaleMinUptime && w.idleLocked(now)
	w.mu.Unlock()
	if !ok {
		return ""
	}

	var why string
	if st := s.startup; st != nil && st.Path != "" {
		// Hashing the installed file is the one costly step; it comes only after the cheap gates above passed.
		if chk := s.installed.check(*st); chk.stale != nil && *chk.stale {
			if fi, err := os.Stat(st.Path); err == nil && now.Sub(fi.ModTime()) >= exitStaleMinAge {
				installed := chk.installedVersion
				if installed == "" {
					installed = "an unreadable version"
				}
				why = fmt.Sprintf("this server runs %s and the installed build is %s; no call is in flight, so it exits now and the client starts the installed build at its next call", st.Version, installed)
			}
		}
	}
	if why == "" {
		return "" // the usual tick: one stat of an installed file that has not moved, and nothing else is read
	}

	// The slow steps are done: decide on the state as it is now, so a lease the session took or a call that arrived meanwhile
	// keeps the server. The ownership reads (two directory listings) come last and only for a server that is about to go.
	if s.ownsLiveWork() != "" {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.idleLocked(time.Now()) {
		return ""
	}
	w.exited = true
	return why
}

// startExitWatch runs the background check until ctx ends: every exitStaleEvery, and shortly after each tool call finishes. When
// the server decides to exit it logs why and calls stop, which makes Run return.
func (s *Server) startExitWatch(ctx context.Context, stop context.CancelFunc) {
	w := &s.xw
	w.mu.Lock()
	w.started = time.Now()
	w.mu.Unlock()
	kick := w.kicked()
	go func() {
		tick := time.NewTicker(exitStaleEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			case <-kick:
				// Let the quiet window after the call pass, then look.
				select {
				case <-ctx.Done():
					return
				case <-time.After(exitStaleSettle + exitStaleSlack):
				}
			}
			if why := s.exitReason(time.Now()); why != "" {
				name, version := s.xw.client()
				log.Printf("mcpserver: exiting by itself (client %s %s): %s", name, version, why)
				stop()
				return
			}
		}
	}()
}
