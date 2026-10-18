package redfixture

import (
	"net/http"
	"net/url"
	"strings"
)

// The same shorthands on a client value: a parameter, a field, a package variable, the default client and a client built
// on the spot. None of them can carry a header.

type fleetCaller struct{ client *http.Client }

var sharedClient = &http.Client{}

func RedClientGet(c *http.Client, base string) (*http.Response, error) {
	return c.Get(base + "/fleet/health")
}

func RedClientPost(c *http.Client, base string) (*http.Response, error) {
	return c.Post(base+"/fleet/dispatch", "application/json", strings.NewReader("{}"))
}

func RedFieldClientHead(f *fleetCaller, base string) (*http.Response, error) {
	return f.client.Head(base + "/fleet/health")
}

func RedSharedClientPostForm(base string) (*http.Response, error) {
	return sharedClient.PostForm(base+"/fleet/dispatch", url.Values{})
}

func RedDefaultClientGet(base string) (*http.Response, error) {
	return http.DefaultClient.Get(base + "/fleet/health")
}

func RedClientBuiltOnTheSpotGet(base string) (*http.Response, error) {
	return (&http.Client{}).Get(base + "/fleet/health")
}
