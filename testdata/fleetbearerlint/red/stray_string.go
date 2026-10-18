package redfixture

import (
	"log"
	"net/http"
)

// Text that says "Authorization", and a header set on some other request, do not put the bearer on this one.

func RedAStrayAuthorizationString(c *http.Client, base string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	log.Print("the Authorization header is left to the caller")
	_, err = c.Do(req)
	return err
}

func RedAnAuthorizationOnAnotherRequest(c *http.Client, base, key string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	other, err := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
	if err != nil {
		return err
	}
	other.Header.Set("Authorization", "Bearer "+key)
	if _, err := c.Do(other); err != nil {
		return err
	}
	_, err = c.Do(req)
	return err
}

func RedAnAuthorizationThatIsNotABearer(c *http.Client, base, key string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Basic "+key)
	_, err = c.Do(req)
	return err
}
