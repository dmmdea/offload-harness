// Fleet bearer auth. The agent lane was the first lane gated (multi-node delegation, Task 3, whose v1 scope was
// that lane alone); every lane is now: a dispatch of ANY task type, the poll and withdraw of any job, and the
// read of any rendered artifact need the fleet bearer on a node that has a fleet_auth_token, and a node that
// listens beyond loopback with none refuses them all. A loopback node with no token stays open (the trust
// boundary of the local MCP surface). Health and the jobs feed stay readable without it, with the private
// fields of each withheld (healthPayload.withoutPrivateFields, handleJobs). The guards themselves live at the
// decision points in server.go (authorizeGated, handleJob, handleMedia) because their PLACEMENT in the handler
// sequence is load-bearing and documented there; this file holds the shared credential check.

package fleetnode

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// fleetTokenRequired is the 403 refusal for a dispatch of any task type on a
// non-loopback listener with no fleet_auth_token configured — a loud
// misconfiguration signal. Callers key on the status (403), not the words.
const fleetTokenRequired = "fleet lanes require fleet_auth_token on a non-loopback listener"

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
