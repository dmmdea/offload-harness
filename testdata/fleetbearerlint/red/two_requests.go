package redfixture

import "net/http"

// A function that sets the bearer on one request and not on a second is judged request by request: the second goes
// without it.

func RedTwoRequestsOneAuthed(c *http.Client, base, key string) error {
	first, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	first.Header.Set("Authorization", "Bearer "+key)
	if _, err := c.Do(first); err != nil {
		return err
	}
	second, err := http.NewRequest(http.MethodGet, base+"/fleet/jobs", nil)
	if err != nil {
		return err
	}
	_, err = c.Do(second)
	return err
}

// RedOneNameTwoRequests reuses the name: the bearer set on the first request does not carry to the second.
func RedOneNameTwoRequests(c *http.Client, base, key string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if _, err := c.Do(req); err != nil {
		return err
	}
	req, err = http.NewRequest(http.MethodGet, base+"/fleet/jobs", nil)
	if err != nil {
		return err
	}
	_, err = c.Do(req)
	return err
}
