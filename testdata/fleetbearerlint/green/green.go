package greenfixture

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
)

// Every request here carries the bearer, and the calls that look like a client's shorthands are not on a client. The
// lint must report none of it.

type fleetConfig struct{ FleetAuthToken string }

// auth is the package's bearer helper: it sets the header on the request it is given.
func auth(cfg fleetConfig, req *http.Request) {
	if cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
	}
}

type caller struct {
	client *http.Client
	cfg    fleetConfig
}

// setBearer reaches the header through the helper above.
func (c *caller) setBearer(req *http.Request) { auth(c.cfg, req) }

func GreenSetsTheHeaderItself(ctx context.Context, c *http.Client, base, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	_, err = c.Do(req)
	return err
}

func GreenAddsTheHeader(c *http.Client, base, key string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", key))
	_, err = c.Do(req)
	return err
}

func GreenUsesTheHelper(c *caller, base string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/jobs", nil)
	if err != nil {
		return err
	}
	auth(c.cfg, req)
	_, err = c.client.Do(req)
	return err
}

func GreenUsesTheMethodThatUsesTheHelper(c *caller, base string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/jobs", nil)
	if err != nil {
		return err
	}
	c.setBearer(req)
	_, err = c.client.Do(req)
	return err
}

// GreenTwoRequestsOneName authenticates each of two requests that share a name.
func GreenTwoRequestsOneName(c *caller, base string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	auth(c.cfg, req)
	if _, err := c.client.Do(req); err != nil {
		return err
	}
	req, err = http.NewRequest(http.MethodDelete, base+"/fleet/jobs/j1", nil)
	if err != nil {
		return err
	}
	auth(c.cfg, req)
	_, err = c.client.Do(req)
	return err
}

// GreenSendsARequestItWasGiven does not build the request, so the builder is the one judged.
func GreenSendsARequestItWasGiven(c *http.Client, req *http.Request) (*http.Response, error) {
	return c.Do(req)
}

// GreenLooksLikeAClientCall has calls named Get and Do on things that are not an HTTP client.
func GreenLooksLikeAClientCall(h http.Header, q url.Values, once *sync.Once, store map[string]string) string {
	once.Do(func() {})
	return h.Get("X-Fleet-Node") + q.Get("/fleet/health") + store["/fleet/jobs"]
}
