package rosterprobe

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// A wrong or missing fleet_auth_token is invisible to every read of a node: /fleet/health ignores the
// Authorization header, so a token mismatch reads healthy until the first dispatch comes back 401, which
// the delegator does not re-place. CheckToken asks the node the one question health cannot: does it accept
// THIS bearer. It is how `doctor` shows the answer before a call is spent on it (membership review F11).
//
// The question is an agent dispatch with no job_id. The node's dispatch handler runs the bearer check
// first (the agent lane is token-gated) and validates the envelope after, so:
//
//	401  the node has a token and this one is not it
//	403  the node has none and listens beyond loopback, so it refuses the agent lane for every caller
//	400  `job_id required`: the bearer passed and the node refused the incomplete envelope
//
// No job is created and nothing is queued on any of the three, which is what makes it safe to send to a
// live node. The ordering is the node's documented, test-pinned contract (docs/FLEET-NODE.md; the
// "auth precedes envelope validation" tests in internal/fleetnode/auth_test.go), and it is re-proved here
// against the real node handler.
const tokenProbeBody = `{"task_type":"agent"}`

// tokenProbeTimeout bounds one token probe. Health answers in milliseconds from a cache; the dispatch
// handler's auth and envelope checks are as cheap, so a node that cannot answer in this is not going to.
const tokenProbeTimeout = 5 * time.Second

// tokenClient rides netguard.SafeTransport like every client that dials a roster member: the bearer is
// sensitive, and the dial-time check is what keeps it on loopback and the tailnet.
//
// It never follows a redirect: the question is "does THIS node accept this bearer", asked of the base the
// operator listed, and a node (or something in front of it) that answers 3xx would otherwise carry the bearer
// to a Location it chose. The 3xx itself is the answer (TokenUnknown: it says nothing about the token).
var tokenClient = &http.Client{
	Transport:     netguard.SafeTransport(nil),
	Timeout:       tokenProbeTimeout,
	CheckRedirect: NoRedirect,
}

// TokenState is what a node's answer to the token probe means.
type TokenState int

const (
	// TokenUnknown: the answer says nothing about the token (an older node, a proxy, another status).
	TokenUnknown TokenState = iota
	// TokenAccepted: the bearer passed the node's check.
	TokenAccepted
	// TokenMismatch: the node has a token and this bearer is not it (401).
	TokenMismatch
	// TokenNodeHasNone: the node has no fleet_auth_token and listens beyond loopback (403).
	TokenNodeHasNone
)

// ClassifyToken reads a node's answer to the probe. A 400 counts as acceptance only when it is the
// `job_id required` refusal: any other 400 (a node that predates the agent lane says `unsupported
// task_type`) proves nothing about the bearer.
func ClassifyToken(status int, body string) TokenState {
	switch status {
	case http.StatusUnauthorized:
		return TokenMismatch
	case http.StatusForbidden:
		return TokenNodeHasNone
	case http.StatusBadRequest:
		if strings.Contains(body, "job_id required") {
			return TokenAccepted
		}
	}
	return TokenUnknown
}

// CheckToken sends the token probe to base with the bearer and returns the HTTP status and a short
// excerpt of the body (for ClassifyToken; callers do not print it). An empty token sends no
// Authorization header, which asks the same question of a node that wants none. A base the tailnet
// guard refuses is never sent the bearer: the error is a *Refusal. A transport failure is the error.
func CheckToken(ctx context.Context, base, token string) (status int, body string, err error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if gerr := netguard.TailnetURL(base); gerr != nil {
		return 0, "", &Refusal{Reason: gerr}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/fleet/dispatch", strings.NewReader(tokenProbeBody))
	if err != nil {
		return 0, "", Scrubbed(base, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := tokenClient.Do(req)
	if err != nil {
		return 0, "", Scrubbed(base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, string(b), nil
}

// NoRedirect is the CheckRedirect of every client that sends the fleet bearer to a roster node: it returns the
// 3xx to the caller instead of following it. The bearer is meant for the node the operator listed, and a node
// (or a proxy in front of it) that redirects would otherwise have the client replay the request, headers
// included, at a Location it chose (net/http keeps Authorization across a redirect that changes only the port,
// and re-sends a 307/308 body). The dial gate still confines the destination to the tailnet; this keeps the
// bearer on the entry that was named.
func NoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
