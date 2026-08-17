// endpoints.go — per-model seat endpoint overrides (Phase A of multi-node
// delegation, spec §S1): any model seat can resolve to a REMOTE
// OpenAI-compatible base over the tailnet, with zero job machinery — the
// cascade or the agent planner uses a Lenovo-served model as a lane simply by
// naming it. Overridden requests ride netguard.SafeTransport, so the
// never-cloud boundary (ADR 0001) holds at DIAL time on every request:
// config-load validation alone cannot survive a DNS answer that drifts to a
// public address after the config was checked.
package llamaclient

import (
	"net/http"
	"strings"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// WithSeatEndpoints installs per-model base overrides and returns the client
// (chainable at construction: New(...).WithSeatEndpoints(cfg.SeatEndpoints)).
// The map key is the exact model id or llama-swap alias a call names; the
// value is the base URL that seat's requests target instead of the default
// base. An empty/nil map installs NOTHING — no override table, no second HTTP
// client — so a config without seat_endpoints yields a client byte-identical
// to a pre-seat build (pinned by test). The overrides get their own
// tailnet-guarded HTTP client, built on the same split-budget transport shape
// New uses, so a cold remote swap keeps the same connect/total budget split
// as a cold local one.
func (c *Client) WithSeatEndpoints(endpoints map[string]string) *Client {
	if len(endpoints) == 0 {
		return c
	}
	m := make(map[string]string, len(endpoints))
	for model, base := range endpoints {
		m[model] = strings.TrimRight(base, "/") // same normalization New applies to base
	}
	c.seatEndpoints = m
	c.safeHTTP = &http.Client{Timeout: c.http.Timeout, Transport: netguard.SafeTransport(newTransport())}
	return c
}

// BaseFor resolves the base URL a request for model targets: the seat
// override on an exact model-id/alias key match, else the default base. ""
// means the client's default model — mirroring Generate's own resolution —
// so the default seat itself can be remoted by keying its model id.
func (c *Client) BaseFor(model string) string {
	if model == "" {
		model = c.model
	}
	if base, ok := c.seatEndpoints[model]; ok {
		return base
	}
	return c.base
}

// httpFor pairs with BaseFor: an overridden seat rides the tailnet-guarded
// client (dial-time refusal of any non-tailnet resolution), every other seat
// rides the original client untouched — the absent-key path never gains a
// guard it did not have.
func (c *Client) httpFor(model string) *http.Client {
	if model == "" {
		model = c.model
	}
	if _, ok := c.seatEndpoints[model]; ok {
		return c.safeHTTP
	}
	return c.http
}
