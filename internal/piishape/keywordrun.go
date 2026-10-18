package piishape

// KeywordRunClass names the residual check in a list of classes that still match a text.
const KeywordRunClass = "credential-keyword-run"

// KeywordRunReach is how many characters after a credential keyword a long value may start.
const KeywordRunReach = 32

// KeywordRun reports whether s holds a credential keyword followed, within KeywordRunReach characters, by a long
// value run. (Not written yet.)
func KeywordRun(s string) bool { return false }
