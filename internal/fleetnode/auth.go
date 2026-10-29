// Fleet bearer auth. The agent lane was the first lane gated (multi-node delegation, Task 3, whose v1 scope was
// that lane alone); every lane is now: a dispatch of ANY task type, the poll and withdraw of any job, and the
// read of any rendered artifact need the fleet bearer on a node that has a fleet_auth_token, and a node that
// listens beyond loopback with none refuses them all. A loopback node with no token stays open (the trust
// boundary of the local MCP surface). Health and the jobs feed stay readable without it, with the private
// fields of each withheld (healthPayload.withoutPrivateFields, handleJobs); a bearer presented there that is not the node's
// is answered the same way and is journaled like any other (holdsBearer). The guards themselves live at the
// decision points in server.go (authorizeGated, handleJob, handleMedia) because their PLACEMENT in the handler
// sequence is load-bearing and documented there; this file holds the shared credential check and the one way a door
// refuses a credential: a 401 or 403 and one journal line (refuseBearer, refuseNoToken, authorizeLane).

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

// The words the journal uses for the doors that refuse a credential themselves. The lanes that go through
// authorizeGated are named by their task type, and the dispatch door by dispatchRefusalLabel. A label is a fixed word
// for the door, never text the caller supplies.
const (
	chatRefusalLabel     = "chat"
	kvslotRefusalLabel   = "kvslot"
	pollRefusalLabel     = "poll"
	withdrawRefusalLabel = "withdraw"
	queueRefusalLabel    = "fleet-queue"
	// The two routes that are readable without the bearer, for a caller that presents one that is not the node's.
	healthRefusalLabel   = "health"
	jobsFeedRefusalLabel = "jobs-feed"
)

// Why a request was refused, as the journal says it. The reason is half of the refusal throttle's key, so there are
// three and the caller cannot add a fourth.
const (
	refusedNoBearer    = "no fleet bearer presented"
	refusedWrongBearer = "the fleet bearer presented is not this node's"
	refusedNoToken     = "no fleet_auth_token is set on this non-loopback listener"
)

// bearerRefusalReason names why r failed the bearer check: it presented nothing, or something that is not this
// node's. The value presented is never read into the reason.
func bearerRefusalReason(r *http.Request) string {
	if r.Header.Get("Authorization") != "" {
		return refusedWrongBearer
	}
	return refusedNoBearer
}

// refuseBearer answers a request that failed the bearer check with 401 and the standard error envelope, and writes
// its journal line (throttled, see refusalLog). Every door that checks the bearer itself ends here, so no door can
// refuse a credential and leave the journal silent.
func (s *Server) refuseBearer(w http.ResponseWriter, r *http.Request, label string) {
	s.refusals.note(r, label, bearerRefusalReason(r))
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

// holdsBearer is the check the routes that stay readable without the bearer (health, the jobs feed) apply to decide
// which view to answer: true when the node has no token (a loopback node, whose caller is this box) or the request carries
// the bearer. A caller that sends none is simply reading what is public and writes nothing. A caller that presents a bearer
// which is not the node's is trying a credential, so it gets the throttled journal line every other door writes for that,
// and is still answered the view a caller without the bearer gets.
func (s *Server) holdsBearer(r *http.Request, label string) bool {
	tok := s.opts.Cfg.FleetAuthToken
	if tok == "" || bearerOK(r, tok) {
		return true
	}
	if r.Header.Get("Authorization") != "" {
		s.refusals.note(r, label, refusedWrongBearer)
	}
	return false
}

// refuseNoToken answers a request to a node that has no fleet_auth_token and listens beyond loopback with 403 and the
// door's own message, and writes its journal line.
func (s *Server) refuseNoToken(w http.ResponseWriter, r *http.Request, label, message string) {
	s.refusals.note(r, label, refusedNoToken)
	writeError(w, http.StatusForbidden, message)
}

// authorizeLane is the rule every lane applies, in one place: with a fleet_auth_token the bearer is required (401
// without it); with none, a loopback listener is open and any other is refused with 403 and the door's own message.
// It writes the refusal and its journal line itself and reports whether the request may go on.
//
// The reachability condition is CONSULTED, never re-derived: AgentLaneSafelyReachable is the same expression
// AgentLaneAdmissible applies to the same resolved listener, so this refusal can never disagree with what health
// advertised. (With no token configured it reduces to "is the listener loopback?" — a tokenless lane beyond
// loopback runs renders, installs node packs and drives a coding-agent loop for whoever can reach the port.
// Misconfiguration must fail here, visibly, at ack time; fleet-serve refuses to start in this state.)
func (s *Server) authorizeLane(w http.ResponseWriter, r *http.Request, label, noTokenMessage string) bool {
	if s.opts.Cfg.FleetAuthToken == "" {
		if !AgentLaneSafelyReachable(s.opts.Cfg, s.opts.LoopbackListener) {
			s.refuseNoToken(w, r, label, noTokenMessage)
			return false
		}
		// Loopback + no token: the lanes are reachable only from this box — the same trust boundary as the
		// local MCP surface.
		return true
	}
	if !bearerOK(r, s.opts.Cfg.FleetAuthToken) {
		s.refuseBearer(w, r, label)
		return false
	}
	return true
}

// bearerOK reports whether r carries `Authorization: Bearer <token>`. Both
// sides are SHA-256 hashed before subtle.ConstantTimeCompare: the hash makes
// the comparison length-independent (ConstantTimeCompare alone returns early
// on unequal lengths, leaking the token's length), and the compare itself is
// constant-time over the digests. The scheme name is matched case-
// insensitively per RFC 7235; the token bytes are exact.
func bearerOK(r *http.Request, token string) bool {
	// No configured token means no bearer can be right: "Bearer " with nothing after it hashes equal to the
	// empty string, so without this line an empty token would ACCEPT an empty bearer, and every door that calls
	// this directly would depend on its own admissibility check having run first. The tokenless-loopback doors
	// never reach here (holdsBearer and authorizeLane short-circuit on an empty token).
	if token == "" {
		return false
	}
	const scheme = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) < len(scheme) || !strings.EqualFold(auth[:len(scheme)], scheme) {
		return false
	}
	presented := sha256.Sum256([]byte(auth[len(scheme):]))
	want := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(presented[:], want[:]) == 1
}
