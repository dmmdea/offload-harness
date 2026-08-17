// Agent-lane bearer auth (multi-node delegation, Task 3; scope per the plan's
// reshape delta 10): ONLY the agent lane is token-gated in v1 — dispatches
// whose decoded task_type is "agent", and /fleet/jobs/{id} polls of jobs an
// agent dispatch created. Every media path (dispatch, job poll, media files,
// health) ignores the token entirely, so already-deployed tokenless media
// clients keep working byte-identically; whole-fleet token enforcement is a
// recorded follow-up for a coordinated whole-fleet deploy window. The guards
// themselves live at the two decision points in server.go (handleDispatch /
// handleJob) because their PLACEMENT in the handler sequence is load-bearing
// and documented there; this file holds the shared credential check.

package fleetnode

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// agentLaneTokenRequired is the 403 refusal for an agent dispatch on a
// non-loopback listener with no fleet_auth_token configured — a loud
// misconfiguration signal, spec'd verbatim (the delegator client keys on it).
const agentLaneTokenRequired = "agent lane requires fleet_auth_token on a non-loopback listener"

// bearerOK reports whether r carries `Authorization: Bearer <token>`. Both
// sides are SHA-256 hashed before subtle.ConstantTimeCompare: the hash makes
// the comparison length-independent (ConstantTimeCompare alone returns early
// on unequal lengths, leaking the token's length), and the compare itself is
// constant-time over the digests. The scheme name is matched case-
// insensitively per RFC 7235; the token bytes are exact.
func bearerOK(r *http.Request, token string) bool {
	const scheme = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) < len(scheme) || !strings.EqualFold(auth[:len(scheme)], scheme) {
		return false
	}
	presented := sha256.Sum256([]byte(auth[len(scheme):]))
	want := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(presented[:], want[:]) == 1
}
