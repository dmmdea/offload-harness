package shared

import "net/http"

// HTTPClient is a client that another package re-exports under a name of its own, and Note takes a request and sets
// nothing on it.
var HTTPClient = &http.Client{}

func Note(req *http.Request) { _ = req }
