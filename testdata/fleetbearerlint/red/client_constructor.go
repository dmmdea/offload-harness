package redfixture

import (
	"net/http"
	"net/url"
	"strings"
)

// A client that a function hands back, and a request that is built in the argument of the Do that sends it.

func newFleetClient() *http.Client { return &http.Client{} }

func RedConstructedClientGet(base string) (*http.Response, error) {
	return newFleetClient().Get(base + "/fleet/health")
}

func RedKeptConstructedClientPost(base string) (*http.Response, error) {
	c := newFleetClient()
	return c.Post(base+"/fleet/dispatch", "application/json", strings.NewReader("{}"))
}

func RedDoOnARequestBuiltInline(c *http.Client, u *url.URL) (*http.Response, error) {
	return c.Do(&http.Request{Method: http.MethodGet, URL: u})
}
