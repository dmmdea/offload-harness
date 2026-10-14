package main

import "regexp"

// Seam: the helpers the speed-term tests call, with no behaviour yet. The
// tests follow in the next commit and fail on their assertions; the bodies
// follow after that.

const speedTermPattern = `(?i)\b(slower|faster|speed-?up)\b|tok/s\s*[<>]=?|wall\s*<=?\s*[0-9.]+\s*x|<=\s*[0-9.]+x\s+the|[0-9.]+x\s+the\s+k=1|\b(near|within|under|below|at most)\s+~?[0-9][0-9.,]*\s*(s|sec|secs|seconds|min|mins|minutes)\b`

var speedTermRe = regexp.MustCompile(speedTermPattern)

type acceptanceFinding struct {
	File  string
	Path  string
	Check string
	Why   string
}

func deescapeRegexArg(s string) string { return s }

func speedTermIn(check string) (term string, ok bool) { return "", false }

func scanAcceptanceDir(dir string) (findings []acceptanceFinding, files []string, strs int, err error) {
	return nil, nil, 0, nil
}

func acceptanceVerbsInSource(src []byte) (consts, cases []string, err error) { return nil, nil, nil }
