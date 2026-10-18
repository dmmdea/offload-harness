package helper

import "net/http"

// SetBearer puts the fleet bearer on a request, and PassOn hands the request to it.

func SetBearer(req *http.Request, key string) { req.Header.Set("Authorization", "Bearer "+key) }

func PassOn(req *http.Request, key string) { SetBearer(req, key) }
