package greenfixture

import (
	"net/http"

	"github.com/dmmdea/offload-harness/testdata/fleetbearerlint/green/helper"
)

// More requests that carry the bearer: through a helper of another package, from a client that a function hands back,
// and from a local that copies a field. A header copied to a local is not a client.

func newGreenClient() *http.Client { return &http.Client{} }

func GreenUsesAHelperOfAnotherPackage(c *http.Client, base, key string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	helper.SetBearer(req, key)
	_, err = c.Do(req)
	return err
}

func GreenUsesAHelperOfAnotherPackageThatPassesTheRequestOn(c *http.Client, base, key string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/jobs", nil)
	if err != nil {
		return err
	}
	helper.PassOn(req, key)
	_, err = c.Do(req)
	return err
}

func GreenConstructedClientWithTheBearer(base, key string) error {
	c := newGreenClient()
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	_, err = c.Do(req)
	return err
}

func GreenLocalCopyOfAFieldClientWithTheBearer(c *caller, base string) error {
	hc := c.client
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	auth(c.cfg, req)
	_, err = hc.Do(req)
	return err
}

func GreenACopyOfAHeaderIsNotAClient(h http.Header) string {
	hh := h
	return hh.Get("X-Fleet-Node")
}
