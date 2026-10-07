package rosterprobe

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// A roster entry is a base URL, but nothing stops one being pasted with a token in its query string
// or a user:password in front of its host (a node behind a reverse proxy, say). Everything this
// package words about an entry goes through the two functions below, and so does the doctor, so
// the lanes' "probed ..." lines and `doctor` cannot disagree about what is safe to print.

// Shown is the member's base as it may be printed: netguard.RedactBase(Base). Base itself stays
// raw, because it is the dial target and the cache key.
func (m Member) Shown() string { return netguard.RedactBase(m.Base) }

// Scrub is err as it may be printed for the entry base. A dial or guard error quotes the URL it was
// given (a net/http error quotes the request URL, and the health reader quotes the URL it built),
// so the raw base in its text is replaced by the redacted one; any other URL left in the text has
// its userinfo, query and fragment masked too, because the HTTP client re-serializes the URL it
// dials (userinfo password replaced by ***, escapes untouched) and a replace of the configured
// string alone would miss that spelling. The cause and the host stay: only the secrets go.
func Scrub(base string, err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	base = strings.TrimSpace(base)
	if base != "" {
		if shown := netguard.RedactBase(base); shown != base {
			text = strings.ReplaceAll(text, base, shown)
			// A %q of the base (strconv.Quote) escapes quotes and backslashes in it.
			if q := strings.Trim(strconv.Quote(base), `"`); q != base {
				text = strings.ReplaceAll(text, q, strings.Trim(strconv.Quote(shown), `"`))
			}
		}
	}
	return maskURLSecrets(text)
}

var (
	// userinfo of any http(s) URL in a message: everything between the scheme's "//" and the "@".
	urlUserinfo = regexp.MustCompile(`(?i)(https?://)[^/\s"'@?#]*@`)
	// query and fragment of any http(s) URL in a message, up to the end of the URL.
	urlQuery    = regexp.MustCompile(`(?i)(https?://[^\s"'?#]*)\?[^\s"']*`)
	urlFragment = regexp.MustCompile(`(?i)(https?://[^\s"'#]*)#[^\s"']*`)
)

// maskURLSecrets masks the userinfo, query and fragment of every http(s) URL in text.
func maskURLSecrets(text string) string {
	text = urlUserinfo.ReplaceAllString(text, "${1}<redacted>@")
	text = urlQuery.ReplaceAllString(text, "${1}?<redacted>")
	return urlFragment.ReplaceAllString(text, "${1}#<redacted>")
}

// scrubbedError is an error whose text has been through Scrub, and which still unwraps to the
// original so errors.Is / errors.As (a *url.Error, a timeout) keep working for the caller.
type scrubbedError struct {
	text string
	err  error
}

func (e *scrubbedError) Error() string { return e.text }
func (e *scrubbedError) Unwrap() error { return e.err }

// Scrubbed wraps err so that printing it can never show base's secrets. A nil err stays nil, and
// an error with nothing to hide is returned as it is.
func Scrubbed(base string, err error) error {
	if err == nil {
		return nil
	}
	if text := Scrub(base, err); text != err.Error() {
		return &scrubbedError{text: text, err: err}
	}
	return err
}
