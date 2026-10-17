package main

// The list of clock_lint_test.go. Regenerate the keys with CLOCK_LINT_PRINT=1 (the command is in that file), then write each
// reason after reading the code: the reason is the review.

// clockBoundAllowlist: every upper bound on a measured elapsed time under one second in a test file, as "file:elapsed>C" with
// its count, and its reason: what the bound rules out and how wide the margin to it is.
var clockBoundAllowlist = map[string]clockAllow{}
