package piishape

import (
	"regexp"
	"strings"
)

// The class table finds a credential by the shape of the text around it, and every shape it does not know (a YAML list
// item, a backtick string, a HAR name and value, an XML attribute, a scheme other than Basic and Bearer, a header that a
// redactor took part of) leaves the value in the text with nothing to compare it to. KeywordRun does not read shapes. It
// is the check a text has to pass after redaction, or before it is shown to a model, and it fails closed: a credential
// header keyword with a long value run a few characters behind it is not safe to keep, whatever stands between them.
//
// The keywords are the names of the headers that carry a credential: cookie (also Set-Cookie, HTTP_COOKIE), authorization,
// api key (X-Api-Key, API_KEY, apikey), auth token (X-Auth-Token) and security token (X-Amz-Security-Token), in any
// case, and inside any longer word or name: the word boundary a class asks for hides HTTP_COOKIE, a backtick, a bytes
// prefix and a quote. The value run is the length the classes ask of a value, 12 characters with no white space, quote,
// comma, semicolon or backslash in them, and it has to start within KeywordRunReach characters behind the keyword. The
// reach covers what the classes' openers allow (eight punctuation characters, a header fold and its indentation, eight
// more) and the ways a document puts a name and a value apart: a closing quote, a colon, a space, an opening bracket and a
// quote (`": ["`, 5 characters), a JSON key and its sibling (`","value":"`, 11), a YAML dash and its indentation after a
// line break (`:\n      - '`, 11), an XML attribute (`" value="`, 9) and the same inside text that was escaped once, twice
// or three times (the quotes and the colon carry the backslashes: 24 at the third layer). 32 holds the deepest of them
// and leaves little room for the next word: the longer the reach the more text that merely follows the keyword is read as
// its value.
//
// Three rules keep ordinary text from tripping it:
//
//   - A placeholder a redactor wrote ([redacted-class-N]) carries the keyword in its name and is a long run itself, so it
//     is not read, and it ends the reach of a keyword in front of it: it stands in the value's place, and what follows
//     belongs to the next field (the next key of a JSON object, the next header line).
//   - A line break ends the reach of a keyword once a word has been read since it: the keyword's own line holds its value,
//     and the next line is the next header. When only punctuation and white space stand between the keyword and the line
//     break, and the last of it is a colon, an equals sign, an opening bracket or brace, a comma or a dash
//     (`Set-Cookie:`, `"Authorization": [`, `"name": "Cookie",`), the value is on the next line, as a YAML list and a
//     pretty-printed JSON document put it, and the reach goes on; a keyword at the end of a closed call or of a line of
//     prose has no value on the next line. A JSON escape of a line break (a backslash run and an n or an r) is a line
//     break.
//   - A run of 11 or fewer characters, or one that starts beyond the reach, is no value.
//
// The check is wide on purpose. A test or an identifier whose name starts with a keyword (TestCookieJarKeepsTheValues,
// cookieSessionManager) is a long run right behind it, and a text that holds one fails. That is the price of failing
// closed: the callers either leave one trace out of a corpus or keep a command's output raw, and the other choice is a
// credential in a corpus.

// KeywordRunClass names the residual check in a list of classes that still match a text.
const KeywordRunClass = "credential-keyword-run"

// KeywordRunReach is how many characters after a credential keyword a long value may start.
const KeywordRunReach = 32

// keywordRunLength is the number of characters of a long value run, the length the classes ask of a value.
const keywordRunLength = 12

// redactionMarkerRe finds a placeholder a redactor wrote.
var redactionMarkerRe = regexp.MustCompile(redactedMarker)

// listContinuationRe finds a list that goes on behind the placeholder of a cookie header: a YAML block list is redacted up
// to the quote that closes its first item, and the header name goes with the item, so the items behind it (a line break,
// indentation, a dash, an optional quote and a long run, the line break raw or as a JSON escape) have no keyword in front
// of them any more.
var listContinuationRe = regexp.MustCompile(`\[redacted-cookie-header-[0-9]+\]['"]?[ \t]*(?:\r?\n|\\+n)[ \t]*-[ \t]+['"]?[^\s'",;\\]{12}`)

// KeywordRun reports whether s holds a credential keyword followed, within KeywordRunReach characters, by a long value
// run (see above), or a list that goes on behind the placeholder of a cookie header. A placeholder is not read and ends
// the reach of the keyword in front of it.
func KeywordRun(s string) bool {
	if strings.Contains(s, "[redacted-") {
		if listContinuationRe.MatchString(s) {
			return true
		}
		s = redactionMarkerRe.ReplaceAllString(s, "\x00")
	}
	spans := keywordSpans(asciiLower(s))
	starts := make(map[int]int, len(spans))
	for _, sp := range spans {
		if sp[1] > starts[sp[0]] {
			starts[sp[0]] = sp[1]
		}
	}
	for _, sp := range spans {
		if valueRunNear(s, sp[1], starts) {
			return true
		}
	}
	return false
}

// asciiLower is s with its ASCII capitals made small, byte for byte (strings.ToLower changes the length of some
// characters, and the indexes of the keywords are used in s).
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; c >= 'A' && c <= 'Z' {
					b[j] = c + 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// keywordSpans are the start and the end of each keyword in lower (a text in small letters): cookie, authorization,
// api key (api, an optional hyphen or underscore, key), auth token and security token (the same with token).
func keywordSpans(lower string) [][2]int {
	var out [][2]int
	each := func(word string, after func(i int) int) {
		for from := 0; ; {
			i := strings.Index(lower[from:], word)
			if i < 0 {
				return
			}
			i += from
			if end := after(i); end >= 0 {
				out = append(out, [2]int{i, end})
			}
			from = i + 1
		}
	}
	plain := func(word string) func(i int) int {
		return func(i int) int { return i + len(word) }
	}
	// word, an optional hyphen or underscore, then tail.
	joined := func(tail string) func(i, n int) int {
		return func(i, n int) int {
			j := i + n
			if j < len(lower) && (lower[j] == '-' || lower[j] == '_') {
				j++
			}
			if strings.HasPrefix(lower[j:], tail) {
				return j + len(tail)
			}
			return -1
		}
	}
	each("cookie", plain("cookie"))
	each("authorization", plain("authorization"))
	apiKey := joined("key")
	each("api", func(i int) int { return apiKey(i, len("api")) })
	authToken, securityToken := joined("token"), joined("token")
	each("auth", func(i int) int {
		if strings.HasPrefix(lower[i:], "authorization") {
			return -1
		}
		return authToken(i, len("auth"))
	})
	each("security", func(i int) int { return securityToken(i, len("security")) })
	return out
}

// valueRunNear reports whether a long value run starts within KeywordRunReach characters behind end in s, under the rules
// above: the run holds no white space, quote, comma, semicolon or backslash; a placeholder (the byte 0) ends the reach;
// a line break ends it once a word has been read. Another keyword (starts maps the start of each to its end) is no part
// of a run: Authorization is 13 characters, and "the Cookie and Authorization headers" has no value in it.
func valueRunNear(s string, end int, starts map[int]int) bool {
	run, word := 0, false
	var last byte // the last byte since the keyword that is not white space or a quote
	limit := end + KeywordRunReach + keywordRunLength
	for i := end; i < len(s) && i < limit; i++ {
		if e, ok := starts[i]; ok {
			run, word = 0, true
			i = e - 1
			continue
		}
		switch c := s[i]; {
		case c == 0:
			return false
		case c == '\\':
			run = 0
			j := i
			for j < len(s) && j < limit && s[j] == '\\' {
				j++
			}
			if j < len(s) && (s[j] == 'n' || s[j] == 'r') {
				if word || !opensAValue(last) {
					return false
				}
				i = j
			} else {
				last = c
			}
		case c == '\n' || c == '\r':
			if word || !opensAValue(last) {
				return false
			}
			run = 0
		case c == ' ' || c == '\t' || c == '\f' || c == '\v' || c == '\'' || c == '"':
			run = 0
		case c == ',' || c == ';':
			run, last = 0, c
		default:
			last = c
			run++
			if c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80 {
				word = true
			}
			if run >= keywordRunLength {
				return true
			}
		}
	}
	return false
}

// opensAValue reports whether the byte before a line break says the value is on the next line: a colon, an equals sign,
// an opening bracket or brace, a comma or a dash (a YAML key, a pretty-printed list or object, the name of a pair, a call
// split over lines). A keyword at the end of a line of prose, or of a call that closes (Header.Get("Authorization")),
// has no value on the next line.
func opensAValue(last byte) bool {
	switch last {
	case ':', '=', '[', '{', ',', '-', '>':
		return true
	}
	return false
}
