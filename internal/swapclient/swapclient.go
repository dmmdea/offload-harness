// Package swapclient is the harness's SINGLE adapter onto the vendored
// llama-swap client (`tools/llamaswap`, package `llamaswap-pp-cli/pkg/llamaswap`).
//
// It exists because the harness had grown three independent readers of
// GET /v1/models with two different response schemas and three different URL
// normalizations, and they disagreed about the one thing that matters: llama-swap
// publishes CANONICAL model ids in `data[].id` and the names the harness actually
// binds — `offload-e4b`, `gemma4-26b-a4b`, `gemma4-e2b` — only inside
// `meta.llamaswap.aliases`. Two of the three readers matched ids alone, so
// `doctor`, `acceptance`, and `report` declared every correctly-served alias
// MISSING on a healthy box.
//
// Everything in this package is a thin pass-through. The alias rules, the
// keep-set-from-config rule, and the /upstream auto-start guard live in
// pkg/llamaswap and are documented there; nothing here re-implements them.
package swapclient

import (
	"context"
	"net/http"
	"strings"
	"time"

	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// DefaultTimeout is the per-request deadline used when a caller passes none.
// Deliberately short: every caller here is a probe on a health/status path, and
// a probe that outlives the command it informs is a hang, not a check.
const DefaultTimeout = 10 * time.Second

// BaseURL turns a harness `endpoint` (config.Config.Endpoint) into the llama-swap
// ROOT that pkg/llamaswap dials.
//
// The harness's own convention has always been that `endpoint` may or may not
// carry the trailing `/v1` — the chat client appends CompletionPath, while
// /running, /upstream, and /api/models all hang off the root. The three old
// readers each normalized this differently (one appended "/v1/models" blindly and
// would have produced "/v1/v1/models" for a /v1-suffixed endpoint, one appended
// "/v1" when absent, one cut at the first "/v1" occurrence). This is the one rule.
func BaseURL(endpoint string) string {
	b := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	return strings.TrimSuffix(b, "/v1")
}

// New builds a llama-swap client bound to the root behind endpoint.
//
// An EMPTY endpoint is a caller error, not a default: pkg/llamaswap would happily
// fall back to its own 127.0.0.1:11436 literal, which would make a probe report on
// a machine the caller never named. Callers guard for it and report "unchecked".
func New(endpoint string, timeout time.Duration) (*llamaswap.Client, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return llamaswap.New(BaseURL(endpoint), &llamaswap.Options{Timeout: timeout})
}

// NewGuarded is New with the tailnet DIAL gate installed — for probes whose
// endpoint is REMOTE and operator-configured (the cascade remote lanes), as
// opposed to this box's own llama-swap.
//
// It exists because the vendored client builds a plain &http.Client{} on the
// DEFAULT transport when no HTTPClient is supplied: ProxyFromEnvironment live,
// no dial-time address check. That is fine for a loopback probe and wrong for
// a configured remote, because netguard.TailnetURL admits a dotless MagicDNS
// short name on SHAPE alone (it deliberately touches no DNS at config time),
// so the only thing that can prove where that name actually resolves is the
// per-dial check in netguard.SafeTransport — which also strips the proxy, so
// nothing can dial on our behalf outside the gate. Without this, ADR 0023's
// "re-checked at every dial" was false for the lane roster probe.
//
// Timeout is carried on BOTH the Options and the supplied client: the vendored
// New honors a supplied client's own Timeout as-is, so leaving it zero would
// silently hand a remote probe an unbounded deadline.
func NewGuarded(endpoint string, timeout time.Duration) (*llamaswap.Client, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return llamaswap.New(BaseURL(endpoint), &llamaswap.Options{
		Timeout:    timeout,
		HTTPClient: &http.Client{Timeout: timeout, Transport: netguard.SafeTransport(nil)},
	})
}

// FetchRosterGuarded is FetchRoster for a REMOTE endpoint: same roster read,
// over NewGuarded's tailnet-gated client. Use it for any base the operator
// configured as remote; FetchRoster stays the right call for this node's own
// local llama-swap, which is loopback by construction and needs no gate.
func FetchRosterGuarded(ctx context.Context, endpoint string, timeout time.Duration) (Roster, error) {
	c, err := NewGuarded(endpoint, timeout)
	if err != nil {
		return Roster{}, err
	}
	models, err := c.Models(ctx)
	if err != nil {
		return Roster{}, err
	}
	return Roster{models: models}, nil
}

// Roster is one alias-aware snapshot of GET /v1/models.
//
// The zero value is an empty roster that serves nothing, which is why every
// caller must branch on the fetch error rather than on Len(): "the roster is
// empty" and "the roster could not be read" are different facts, and conflating
// them is how a health check goes green against a dead endpoint.
type Roster struct {
	models []llamaswap.Model
}

// FetchRoster reads the live roster from the llama-swap behind endpoint.
func FetchRoster(ctx context.Context, endpoint string, timeout time.Duration) (Roster, error) {
	c, err := New(endpoint, timeout)
	if err != nil {
		return Roster{}, err
	}
	models, err := c.Models(ctx)
	if err != nil {
		return Roster{}, err
	}
	return Roster{models: models}, nil
}

// NewRoster wraps an already-fetched model list. For callers that hold a client.
func NewRoster(models []llamaswap.Model) Roster { return Roster{models: models} }

// Serves reports whether nameOrAlias is served, matching canonical ids FIRST and
// then `meta.llamaswap.aliases` — case-insensitively, as pkg/llamaswap resolves.
// An empty name is never served.
func (r Roster) Serves(nameOrAlias string) bool {
	name := strings.TrimSpace(nameOrAlias)
	if name == "" {
		return false
	}
	for _, m := range r.models {
		if strings.EqualFold(m.ID, name) {
			return true
		}
	}
	for _, m := range r.models {
		for _, a := range m.Aliases {
			if strings.EqualFold(a, name) {
				return true
			}
		}
	}
	return false
}

// IDs returns the canonical model ids in roster order.
func (r Roster) IDs() []string {
	ids := make([]string, 0, len(r.models))
	for _, m := range r.models {
		ids = append(ids, m.ID)
	}
	return ids
}

// Len is the number of roster entries.
func (r Roster) Len() int { return len(r.models) }
