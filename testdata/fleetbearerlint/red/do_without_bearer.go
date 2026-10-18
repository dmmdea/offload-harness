package redfixture

import "net/http"

// A request that is built and sent with no bearer call on it, and one that is built by a helper and sent here.

func RedDoOnARequestBuiltWithoutBearer(c *http.Client, base string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// RedBuildFleetRequest hands back a request nothing has set the bearer on.
func RedBuildFleetRequest(base string) (*http.Request, error) {
	return http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
}

func RedDoOnARequestFromAHelper(c *http.Client, base string) (*http.Response, error) {
	req, err := RedBuildFleetRequest(base)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}
