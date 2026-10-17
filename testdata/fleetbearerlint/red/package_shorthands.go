package redfixture

import (
	"net/http"
	"net/url"
	"strings"
)

// The package-level shorthands build and send a request in one call, so there is no request value to set a header on:
// a call to a fleet node through one of them goes without the bearer, whatever the function does around it.

func RedPackageGet(base string) (*http.Response, error) {
	return http.Get(base + "/fleet/health")
}

func RedPackagePost(base string) (*http.Response, error) {
	return http.Post(base+"/fleet/dispatch", "application/json", strings.NewReader("{}"))
}

func RedPackageHead(base string) (*http.Response, error) {
	return http.Head(base + "/fleet/health")
}

func RedPackagePostForm(base string) (*http.Response, error) {
	return http.PostForm(base+"/fleet/dispatch", url.Values{})
}
