package fleetnode

// The node half of the caller-scope policy (2026-11-09; delegator half: internal/config scope.go). A node whose
// config sets fleet_admit_scopes takes work only from askers that declare one of those scopes in the
// X-Offload-Scope header, and refuses everything else with a typed 403 before it reads a body or creates a job.
// It is a backstop: the delegator's own filter keeps such a node out of every list for a caller it does not
// admit, so this door only fires for a delegator that is stale, misconfigured or resolves no scope at all.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dmmdea/offload-harness/internal/callscope"
)

// scopeRefusalReason is the refusal-log reason of a request this node refused for its asker's scope.
const scopeRefusalReason = "scope"

// scopeFreeLabels are the doors that create no work and read no caller data: fetching a finished job's output
// file belongs to a job the node already admitted.
var scopeFreeLabels = map[string]bool{"media-file": true}

// admitsScope reports whether the request's asker may be given work on this node, and writes the 403 itself when
// it may not. With fleet_admit_scopes unset every asker is admitted, exactly as before the key.
func (s *Server) admitsScope(w http.ResponseWriter, r *http.Request, label string) bool {
	admit := s.opts.Cfg.FleetAdmitScopes
	if len(admit) == 0 || scopeFreeLabels[label] {
		return true
	}
	asker := strings.ToLower(strings.TrimSpace(r.Header.Get(callscope.Header)))
	if asker == callscope.Business || asker == callscope.Personal {
		for _, a := range admit {
			if strings.ToLower(strings.TrimSpace(a)) == asker {
				return true
			}
		}
	} else {
		asker = callscope.Unknown
	}
	s.refusals.note(r, label, scopeRefusalReason)
	writeJSON(w, http.StatusForbidden, map[string]any{
		"status":       "error",
		"error":        fmt.Sprintf("scope refused: this node admits only %s callers (fleet_admit_scopes) and the asker's scope is %s", strings.Join(admit, ", "), asker),
		"refusal":      "scope_not_admitted",
		"admit_scopes": admit,
	})
	return false
}
