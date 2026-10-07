package netguard

import (
	"errors"
	"net/url"
	"strings"
)

// RedactBase is a configured base URL as it may be PRINTED: unchanged unless it carries what a
// URL can carry that is a secret (userinfo, a query string, a fragment), in which case those are
// dropped and the scheme, host, port and path remain. A value that does not parse is shown as it
// is (a typo has to be shown to be fixed), except that whatever sits before the last "@" of its
// authority is masked and anything from a "?" or "#" on is cut, so a mistyped base carrying
// credentials is not echoed whole. An ordinary base prints byte-identically, so no message about
// it changes.
//
// It lives here, below internal/config, because the tailnet guard's own refusal quotes the value
// it refused: a roster entry is a base URL, but nothing stops one being pasted with a token in its
// query string or a user:password in front of its host, and every message that names an entry must
// be safe to paste into a chat. internal/config.RedactBase is this function.
func RedactBase(raw string) string {
	v := strings.TrimSpace(raw)
	u, err := url.Parse(v)
	if err != nil {
		return maskUnparsed(v)
	}
	// "user:pw@node:18811" has no "//", so net/url reads it as scheme "user" with the opaque
	// remainder "pw@node:18811": nothing is in u.User to drop, and re-serializing would print the
	// password. Mask it like a value that did not parse.
	if strings.Contains(u.Opaque, "@") {
		return maskUnparsed(v)
	}
	if u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" {
		return v
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment = nil, "", false, ""
	return u.String()
}

// maskUnparsed redacts a value net/url could not take apart: the authority's userinfo (everything
// between "://", or the start of a scheme-less value, and the last "@") is masked, and a query or
// fragment is cut off, the two places a pasted secret lives.
func maskUnparsed(v string) string {
	start := 0
	if i := strings.Index(v, "://"); i >= 0 {
		start = i + 3
	}
	if at := strings.LastIndex(v, "@"); at > start {
		v = v[:start] + "<redacted>@" + v[at+1:]
	}
	if q := strings.IndexAny(v, "?#"); q >= 0 {
		v = v[:q] + "<redacted>"
	}
	return v
}

// parseFailure is why a URL did not parse, in words that never quote the value: a url.Error quotes
// the WHOLE input, credentials and all, and an EscapeError quotes three bytes of it. The callers
// already print the value, redacted.
func parseFailure(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	var esc url.EscapeError
	if errors.As(err, &esc) {
		return "invalid percent-escape"
	}
	return err.Error()
}
