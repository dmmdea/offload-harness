package redfixture

import (
	"net/http"
	"strings"

	"github.com/dmmdea/offload-harness/testdata/fleetbearerlint/red/shared"
)

// A client reached through another name for it: a package variable that re-exports another package's client, and a
// local that copies a field, a package variable or a dereferenced client. The shorthands on any of them still leave
// nowhere to set a header.

var ReExportedClient = shared.HTTPClient

func RedReExportedClientGet(base string) (*http.Response, error) {
	return ReExportedClient.Get(base + "/fleet/health")
}

func RedLocalCopyOfAFieldClientPost(f *fleetCaller, base string) (*http.Response, error) {
	hc := f.client
	return hc.Post(base+"/fleet/dispatch", "application/json", strings.NewReader("{}"))
}

func RedLocalCopyOfAPackageClientGet(base string) (*http.Response, error) {
	hc := sharedClient
	return hc.Get(base + "/fleet/health")
}

func RedLocalCopyOfADereferencedClientHead(c *http.Client, base string) (*http.Response, error) {
	cp := *c
	cp.Timeout = 0
	hc := &cp
	return hc.Head(base + "/fleet/health")
}

// A helper of another package that is handed the request and sets nothing on it does not count as a bearer call.
func RedAnImportedHelperThatSetsNothing(c *http.Client, base string) error {
	req, err := http.NewRequest(http.MethodGet, base+"/fleet/health", nil)
	if err != nil {
		return err
	}
	shared.Note(req)
	_, err = c.Do(req)
	return err
}
