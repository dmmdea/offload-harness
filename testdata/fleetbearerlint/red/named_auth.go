package redfixture

import "net/http"

// auth is called the way a client's bearer helper is, and sets nothing on the request it is given.
func auth(key string, req *http.Request) {
	_ = key
	_ = req
}

func RedACallNamedAuth(c *http.Client, base, key string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	auth(key, req)
	_, err = c.Do(req)
	return err
}
