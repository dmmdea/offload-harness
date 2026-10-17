// Package piishape holds the credential- and identity-shaped patterns that the
// compaction-eval corpus vet refuses (the "vet classes"). It exists so a package
// that cannot import internal/compeval (compeval imports internal/agent) can
// still ask the same question compeval asks: does this text look like it carries
// a secret.
package piishape

import (
	"regexp"
	"sort"
	"strings"
)

// jsonEscape is a JSON string escape that can stand right before a word: a backslash and one of n, r,
// t, b, f, or a backslash-u and four hex digits. Tool-call arguments and JSON tool output are JSON
// text, so the newline in front of a line that starts with a key word is those two characters, and
// there is no word boundary between the 'n' of the escape and the key word's first letter. Escapes
// are lower case, so the alternative stays case-sensitive inside a case-insensitive class (a Windows
// path such as C:\Tokens\x is not an escape).
const jsonEscape = `\\(?-i:[nrtbf]|u[0-9a-fA-F]{4})`

// ansi is a terminal control sequence: the escape byte, '[', parameters and a final letter, such as the
// color code ESC[32m. Colored build and test output (FORCE_COLOR, --color=always, CI) puts one right in
// front of the next word, and its final letter is a word character, so a word boundary does not see the
// word after it. The escape byte is the raw byte in plain output and the six characters backslash, u, 0,
// 0, 1, b in JSON text.
const ansi = `(?:\x1b|(?-i:\\u001[bB]))\[[0-9;:]*[A-Za-z]`

// lead is what may stand in front of a class's first word: a word boundary, a JSON escape or a terminal
// control sequence.
const lead = `(?:\b|` + jsonEscape + `|` + ansi + `)`

// leadUnderscore also allows an underscore, so a key word inside a name (GITHUB_TOKEN) is found, and a
// flag letter, so the Java system property form (-Dpassword=) is found.
const leadUnderscore = `(?:\b|_|-[A-Za-z]|` + jsonEscape + `|` + ansi + `)`

// stop is what ends a credential value (the inside of a character class): whitespace, a quote, a comma,
// a semicolon, an ampersand, a backslash (an escape in JSON text) and the escape byte of a control
// sequence.
const stop = `\s'",;&\\\x1b`

// headerGap is one piece of what may stand between an Authorization header's name and its scheme: a
// non-word character other than a newline (a colon, a bracket, a quote, a comma, a paren, a space), a
// JSON escape, or the opener of a Go string slice literal. The pieces cover a header line, a printed
// http.Header, a call that takes the name and the value as two arguments, a subscript and a hash.
const headerGap = `(?:[^\w\r\n]|` + jsonEscape + `|\[\]string\{)`

// leadRe finds the escape a match begins with.
var leadRe = regexp.MustCompile(`^(?:` + ansi + `|` + jsonEscape + `)`)

// Lead returns the escape that opens a match, if any: a JSON string escape or a terminal control
// sequence that stood in front of the class's first word. It is not part of the credential, so a
// redactor keeps it and puts its placeholder after it; replacing it would break the JSON of the text it
// sits in.
func Lead(m string) string { return leadRe.FindString(m) }

// keyWords are the words that make an assignment a credential: api key, private, access and auth key,
// secret, token, passwd, password (plural too), PGPASSWORD (the one name that carries the word with no
// boundary before it), passphrase and credential(s). The short forms pass and pwd are a separate,
// underscore-only alternative of the class (DB_PASS=, MYSQL_PWD=): bare, they would read the test
// runner's "PASS: TestName" and the shell's PWD as credentials.
const keyWords = `(?:api[_-]?key|(?:private|access|auth)[_-]?key|secret|token|passw(?:or)?ds?|pgpassword|passphrase|credentials?)`

// credName is a key word with the parts a name may add after it (AWS_SECRET_ACCESS_KEY, client_secret_value,
// secret_id) and digits (password1).
const credName = keyWords + `(?:[_-](?:access|key|secret|token|value|id))*\d*`

// valueBody is a credential value of 12 or more characters. It starts with anything but a stop, '/', '.',
// '[' or ']' (a path after a key name is meant to stay quiet), may start with a symbol, and runs to the
// next stop, so it takes in '.', '/', '+', '=', '@', '~' and ':'. The match is the WHOLE value on purpose:
// the harvest redactor is derived from this table, so a value the pattern stops short of would be written
// out with its tail.
const valueBody = `[^` + stop + `/.\[\]][^` + stop + `]{11,}`

// htmlEscape is the JSON escape that Go's encoder writes for '<' and '>' (backslash, u, 003c or 003e),
// so a type argument or a hash rocket in Args that were marshalled by it carries the escape.
const htmlEscape = `\\u003[cCeE]`

// htmlChar is the JSON escape that Go's encoder writes for '&', '<' and '>': a quoted value with one of
// them, marshalled into Args, carries the six characters where the text had the one.
const htmlChar = `\\u00(?:26|3[cCeE])`

// openQuote is a quote with any number of backslashes in front of it: JSON text doubles them, and JSON
// text inside a double-quoted shell string doubles them again.
const openQuote = `\\*['"]`

// quotedBody is the inside of a quoted literal of 12 or more characters, for the quote kind q. Inside
// quotes a space, a comma, a semicolon and an ampersand are part of the value, so it runs to the closing
// quote, not to the next stop. It never runs past a line break, a backslash (an escaped quote in JSON
// text, the same boundary the unquoted value has) or the escape byte of a control sequence; the other
// kind of quote and Go's JSON escape for '&', '<' and '>' are part of it. The first character follows the
// unquoted value's rule: no whitespace, and none of '/', '.', '[' and ']', so a path in quotes stays quiet.
// The closing quote is not part of the match.
func quotedBody(q string) string {
	return `[^\s` + q + `\\/.\[\]\x1b](?:[^` + q + `\\\r\n\x1b]|` + htmlChar + `){11,}`
}

// quotedLiteral is an opening quote (with any backslashes in front of it) and the body that follows. With
// capture the body is a capturing group, one per quote kind, so a redactor can ask for the value alone.
func quotedLiteral(capture bool) string {
	dq, sq := quotedBody(`"`), quotedBody(`'`)
	if capture {
		dq, sq = `(`+dq+`)`, `(`+sq+`)`
	}
	return `\\*(?:"` + dq + `|'` + sq + `)`
}

// plainValue is the unquoted value, behind an optional opening quote; with capture it is a group.
func plainValue(capture bool) string {
	v := valueBody
	if capture {
		v = `(` + v + `)`
	}
	return `(?:` + openQuote + `)?` + v
}

// afterName is what may stand between a name and the operator that follows it: the closing quote of a
// quoted name and a closing bracket or paren (os.environ["API_TOKEN"] = , {'password' => , "token": ).
const afterName = `(?:` + openQuote + `)?[\])]?`

// typeAnno is a type written between a name and its value (": string", " string", ": Optional[str]",
// ": Record<string, string>").
const (
	typeName = `[A-Za-z_](?:[\w.*\[\]<>|?]|` + htmlEscape + `)*`
	typeAnno = `(?:\s*:\s*|\s+)` + typeName + `(?:,\s*` + typeName + `)*`
)

// assignment builds the api-key-assignment pattern. With capture the value of the match (the quoted body
// or the unquoted value) is a capturing group, so AssignmentValue can name it; the class itself has none.
//
//   - plain is the assignment the class always matched: a key word, = or :, then a value that may start
//     with a quote. Or else _PASS= or _PWD=: the short words need the underscore and the equals sign, so a
//     test runner's "PASS: TestName" and the shell's PWD stay quiet. A quoted literal comes first, so it
//     runs to its closing quote; the unquoted value runs to the next stop.
//   - quoted is an assignment whose value is a quoted literal, with a wider operator between the name and
//     the literal: Go's :=, a comparison (== or !=), a hash rocket (=>), a type annotation in front of the
//     = (const token: string = "..."), a closing quote or bracket after the name, and a b, r, u or f prefix
//     on the string. A quote is required: an unquoted value after := or == is an identifier or a call
//     (token := strings.TrimSpace(x)), and Go source assigns to token-named variables all day.
//   - call is a call that takes the name and the literal as two arguments: Setenv("API_TOKEN", "...").
//     The comma only counts right behind an opening paren and a quoted name, so a list of strings
//     (["token", "client_identifier"]) stays quiet. The match starts at the paren.
func assignment(capture bool) string {
	plain := `(?:` + leadUnderscore + credName + `(?:\s*` + ansi + `)*\s*[=:]|_(?:pass|pwd)\s*=)\s*(?:` + ansi + `\s*)*(?:` + quotedLiteral(capture) + `|` + plainValue(capture) + `)`
	quoted := leadUnderscore + credName + afterName + `(?:` + typeAnno + `(?:\s*` + ansi + `)*\s*=|(?:\s*` + ansi + `)*\s*(?::=|[=!]==?|=(?:>|` + htmlEscape + `)|[=:]))\s*(?:` + ansi + `\s*)*[bruf]{0,2}` + quotedLiteral(capture)
	call := `\(\s*` + openQuote + `[A-Za-z0-9_.-]*` + credName + openQuote + `\s*,\s*[bruf]{0,2}` + quotedLiteral(capture)
	return `(?:` + quoted + `|` + plain + `|` + call + `)`
}

// assignValueRe finds the value inside a whole match of the api-key-assignment class.
var assignValueRe = regexp.MustCompile(`(?i)\A` + assignment(true) + `\z`)

// AssignmentValue returns the credential value of a match of the api-key-assignment class: the quoted
// literal's body, or the unquoted value, without the key name, the operator or the opening quote. It is ""
// when m is not a whole match of the class (strip Lead first). A redactor remembers exactly this part,
// because the name, a type annotation or a call in front of a literal is ordinary text elsewhere.
func AssignmentValue(m string) string {
	sub := assignValueRe.FindStringSubmatch(m)
	for i := 1; i < len(sub); i++ {
		if sub[i] != "" {
			return sub[i]
		}
	}
	return ""
}

// emailDomainStart is the first character of an address's domain, which may not be a lowercase v
// followed by a digit: that is a Go module version (golang.org/toolchain@v0.0.1-go1.27.2.linux-amd64),
// which every stack trace and module-cache path in a Go log carries and which otherwise reads as an
// address ending in ".linux".
const emailDomainStart = `(?:[A-Za-uw-z0-9-]|v[A-Za-z.-])`

// cookieOpener is what stands between a cookie header's name and the cookies: the punctuation the Basic
// header takes (up to eight non-word characters, then a colon, an equals sign, a hash rocket or an opening
// bracket, then up to eight more), so a header line, a list, a subscript and a hash are covered; or else a
// comma behind which a quote opens the string that holds the cookies, the form of a call that takes the name
// and the value as two arguments (Header.Set("Cookie", "...")). A bare comma stays quiet: "the cookie,
// path=/some/long/path" is prose.
const cookieOpener = `(?:` + headerGap + `{0,8}?[:=>\[]|` + headerGap + `{0,8}?,\s*\\*['"])` + headerGap + `{0,8}?`

// cookieQuoteLevels are the numbers of backslashes in front of a double quote that opens or closes a quoted
// cookie, by how many times the text has been escaped: none on a header line, one in JSON text and in the
// double-quoted string of a shell command or of code, three when that string is itself JSON text. The
// pattern has no back-references, so it cannot ask that the closing quote carry as many backslashes as the
// opening one; each level is written out instead. The level also fixes which backslash-and-quote is a quote
// inside the value (twice the backslashes plus one) and which one closes it.
var cookieQuoteLevels = []int{0, 1, 3}

// backslashes is a pattern for n literal backslashes.
func backslashes(n int) string { return strings.Repeat(`\\`, n) }

// cookieQuotedUnit is one piece of a quoted cookie's body at a level (n backslashes in front of its quotes),
// the way Python's http.cookies writes it, which is how Django writes its messages cookie: a character from
// plain; a quote inside the value (2n+1 backslashes and a quote); a backslash inside the value (2n+2
// backslashes); an escape such as the octal one for a comma, a semicolon or a space (n+1 backslashes and a
// character); and, in JSON text, an escape of the JSON layer itself (\u0026, \t, \/: one backslash and a
// character other than n or r, which are line breaks). The backslash and quote that close the value are
// none of these, so a body stops there.
func cookieQuotedUnit(n int, plain string) string {
	u := plain + `|` + backslashes(2*n+1) + `"|` + backslashes(2*n+2) + `|` + backslashes(n+1) + `[^"\\\r\n]`
	if n > 0 {
		u += `|` + backslashes(1) + `[^"\\\r\nnr]`
	}
	return u
}

// cookieQuotedPair is a cookie whose value is a double-quoted string, from the equals sign to the closing
// quote, at any level. A line break ends it.
var cookieQuotedPair = func() string {
	var alts []string
	for _, n := range cookieQuoteLevels {
		q := backslashes(n) + `"`
		alts = append(alts, `=`+q+`(?:`+cookieQuotedUnit(n, `[^"\\\r\n]`)+`)*`+q)
	}
	return `(?:` + strings.Join(alts, `|`) + `)`
}()

// cookieStep is one step over text that is not the value: a quoted pair, or any character that is not a
// quote, a backslash or a line break. The quoted pair comes first, so the quote that opens it is not where
// the step stops. A quote that is not part of a pair still ends the walk, because in a command or in JSON
// text it closes the string the header sits in.
var cookieStep = `(?:` + cookieQuotedPair + `|[^'"\\\r\n])`

// cookieValue is a cookie value of 12 or more characters that is a credential: in double quotes, taken
// whole, escapes included (12 or more units, then the closing quote when it is there), or bare (it ends
// before a quote, so a header in a curl command ends before the quote that closes the string).
var cookieValue = func() string {
	var alts []string
	for _, n := range cookieQuoteLevels {
		q := backslashes(n) + `"`
		alts = append(alts, q+`(?:`+cookieQuotedUnit(n, `[^\s'",;&\\]`)+`){12,}(?:`+q+`)?`)
	}
	return `(?:` + strings.Join(alts, `|`) + `|[^\s'",;&\\]{12,})`
}()

// cookieBody walks to the first long cookie value and then takes the rest of the line, stepping over quoted
// pairs on the way in and on the way out. The other cookies on the line go with the value: the safe
// direction. A single quote is a gap: an apostrophe inside an unquoted value (name=O'Brien) cannot be told
// from the quote that closes a header written in single quotes, so the match ends there.
var cookieBody = cookieStep + `*?=` + cookieValue + cookieStep + `*`

// Class is one refusal class: a name and the pattern that recognizes it.
type Class struct {
	Name string
	Re   *regexp.Regexp
}

// Classes are the DETERMINISTIC refusal classes for corpus vetting: a corpus is a
// committed/replayed artifact, so credential- or identity-shaped content refuses
// the whole corpus rather than being silently scrubbed (scrubbing would change
// the bytes the hash pins). The table moved here from internal/compeval with six
// classes; later classes are appended at the end, never reordered.
//
// What the classes recognise:
//
//   - api-key-assignment: a key word (api key, private/access/auth key, secret,
//     token, passwd, password or passwords, PGPASSWORD, passphrase, credential or
//     credentials), which may follow an underscore or any non-word character
//     (GITHUB_TOKEN=, OPENAI_API_KEY=, DB_PASSWORD=), a flag letter (-Dpassword=) or a
//     terminal control sequence (a color code), and may be followed by
//     access/key/secret/token/value/id parts (AWS_SECRET_ACCESS_KEY=,
//     client_secret_value=, secret_id=) and digits (password1=), then = or :
//     (a control sequence may stand between the name and the colon), or
//     else _PASS= or _PWD= (DB_PASS=, MYSQL_PWD=; the short words need the
//     underscore and the equals sign, so a test runner's "PASS: TestName" and the
//     shell's PWD stay quiet), then a value of 12 or more
//     characters. The value may start with a quote (any number of backslashes may
//     stand before it: JSON text inside a shell string doubles them) and
//     then any character except whitespace, a quote, a comma, a semicolon, an
//     ampersand, a backslash, the escape byte, '/', '.', '[' or ']'; it runs to the next
//     whitespace, quote, comma, semicolon, ampersand, backslash or escape byte, so it takes in '.', '/', '+',
//     '=', '@', '~' and ':' (a JWT after access_token=, a slash-bearing secret key)
//     and a leading symbol (DB_PASSWORD=!...). A backslash ends the value because
//     tool-call arguments are JSON text and a backslash there is an escape: running
//     through it would take the closing quote or the next command with it. The
//     match is the WHOLE value on purpose: the
//     harvest redactor is derived from this table, so a value the pattern stops
//     short of would be written out with its tail. A name that only contains the
//     word (MAX_TOKENS, token_count, tokenizer) does not match. A path after a
//     key-ish name (token_store=/var/lib/x) starts with '/', so it does not
//     match; token_store=var/lib/xyzzy does, which is the safe direction.
//     A literal that starts with a quote is a QUOTED literal: its value runs to the closing
//     quote of the same kind, so a space, a comma, a semicolon, an ampersand and the other
//     kind of quote are part of it (a shell export of a key with a space in its value, an
//     env file line with a single-quoted value, a JSON value with a comma), and so is Go's
//     JSON escape for &, < and >. It stops at a backslash (an escaped quote in JSON text),
//     a line break and the escape byte of a control sequence, and the closing quote is not
//     part of the match; an unterminated quote runs to the end of the line. The
//     first-character rule above holds inside the quote, and the 12 characters are counted
//     inside it. A quoted prose value after a key name matches too, which is the safe
//     direction.
//     A QUOTED literal of 12 or more characters is also matched behind a wider
//     operator: Go's := (password := "..."), a comparison (== or !=), a hash
//     rocket (=>), a type annotation in front of the = (const token: string =
//     "...", password: str = "..."), a closing quote or bracket after the name
//     (os.environ["API_TOKEN"] = "...", {'password' => '...'}, a quoted JSON key
//     "token": "..."), and a b, r, u or f prefix on the string (password = b"...").
//     The quote is required there: after := or == an unquoted value is an
//     identifier or a call (token := strings.TrimSpace(x)), which would flood.
//     A call that passes the name and the literal as two arguments is matched
//     when the name is quoted right behind the opening paren
//     (t.Setenv("API_TOKEN", "...")); the match then starts at the paren. A list
//     of strings (["token", "client_identifier"]) stays quiet.
//   - bearer-token: the word bearer, whitespace, then a value of 16 or more
//     characters that starts with a letter, digit, underscore or hyphen and runs
//     to the next whitespace, quote, comma, semicolon, ampersand, backslash or escape byte
//     (whole value, for the same reason).
//   - provider-key: a bare provider key, sk- followed by 20 or more letters and
//     digits (OpenAI style), or sk-ant-, sk-proj-, sk-svcacct-, sk-admin- followed
//     by 20 or more key characters. sk-learn and similar short names do not match.
//   - jwt: three base64url segments of 8 or more characters, the first starting eyJ.
//   - basic-auth: an Authorization header (also Proxy-Authorization, or a quoted
//     JSON key) with the Basic scheme and a value of 12 or more characters. A short run
//     of punctuation (at least one of : = , > [) may stand between the name and the
//     scheme, so a printed Go header (map[Authorization:[Basic ...]]), a call
//     (setHeader('Authorization', 'Basic ...'), Header.Set("Authorization", "Basic ...")),
//     a subscript and a hash literal are matched; the value ends before a closing
//     bracket, paren or brace, which base64 never holds.
//   - auth-field: a quoted auth key whose quoted value is an encoded string of 12
//     or more characters (a container registry's config file).
//   - cookie-header: a Cookie or Set-Cookie header (the punctuation the Basic header takes
//     may stand between the name and the cookies, so a JSON list, a subscript, a hash and
//     a printed Go header match; a comma counts only when a quote follows it, as in
//     Header.Set("Cookie", "..."), so prose stays quiet) that holds a value of 12 or more
//     characters; the match runs to the end of the line or to the quote that closes the
//     string the header sits in, so the other cookies on the line go with it. A
//     double-quoted pair (messages="ok") is stepped over before and after the first long
//     value, and a double-quoted long value is matched whole, with its closing quote. Inside
//     the quotes a quote with a backslash in front of it, a doubled backslash and an octal
//     escape (\054) belong to the value, which is how Python's http.cookies writes Django's
//     messages cookie; a line break ends the pair. The quotes carry no backslash on a header
//     line, one in JSON text and in the double-quoted string of a command or of code, and three
//     in JSON text inside such a string; each of the three is written out, and the escapes
//     inside the value shift with them. A single quote ends the match: an apostrophe in an
//     unquoted value (name=O'Brien) cannot be told from the quote that closes a header
//     written in single quotes, so a cookie behind one is not covered.
//
// Every class that starts with a word also accepts, right before the word, a JSON string escape
// (a backslash and n, r, t, b or f, or backslash-u and four hex digits) or a terminal control
// sequence (the escape byte, '[', parameters and a letter; as raw output or as the JSON escape
// of the byte): in tool-call arguments the newline before a line that starts with
// PASSWORD= is those two characters, in colored output a color code stands there, and a word
// boundary sees neither. The match then carries the escape or sequence; Lead names it and the
// redactor keeps it.
//
// Not covered (the redactor does not scrub these either; see the reducer's honest
// limits and docs/systems/coding-agent.md): a key name written in camel case
// (githubToken=), a key name the list above does not hold (dbpassword=, sid=,
// session= outside a cookie header), a command-line flag with a space
// (--token value, curl -u user:pass), URL userinfo
// (scheme://user:pass@host), an authorization scheme other than Bearer and Basic
// (Digest, NTLM), a provider prefix the table does not list (sk_live_, xoxb-), an
// unsigned JWT, a high-entropy string with no key name or known prefix, a value
// that starts with '/', '.', '[' or ']', the part of a value after a
// backslash inside it (a double-quoted cookie keeps its escapes, above), an UNQUOTED value after :=, == or => (an identifier or a
// call, by design), a value passed apart from its name (fmt.Sprintf("password=%s", v)),
// a terminal control sequence other than escape-bracket-letter (a charset switch,
// an operating-system command, the one-byte form of the bracket), and a Basic
// header whose name and scheme (or a cookie header whose name and cookies) are more than
// eight punctuation characters apart or have a word between them, a cookie behind a
// single quote or an apostrophe, a quoted cookie whose quotes carry two backslashes or four
// or more, and a cookie passed apart from its header name (curl -b, a jar).
var Classes = []Class{
	{"email", regexp.MustCompile(`(?:` + jsonEscape + `)?[A-Za-z0-9._%+-]+@` + emailDomainStart + `[A-Za-z0-9.-]*\.[A-Za-z]{2,}`)},
	{"private-key-block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"bearer-token", regexp.MustCompile(`(?i)` + lead + `bearer\s+[A-Za-z0-9_\-][^` + stop + `]{15,}`)},
	{"api-key-assignment", regexp.MustCompile(`(?i)` + assignment(false))},
	{"aws-access-key", regexp.MustCompile(lead + `AKIA[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(lead + `gh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"provider-key", regexp.MustCompile(lead + `sk-(?:(?:ant|proj|svcacct|admin)-[A-Za-z0-9_\-]{20,}|[A-Za-z0-9]{20,})`)},
	{"jwt", regexp.MustCompile(lead + `eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	{"basic-auth", regexp.MustCompile(`(?i)` + lead + `authorization` + headerGap + `{0,8}?[:=,>\[]` + headerGap + `{0,8}?basic\s+[A-Za-z0-9+/=_\-][^` + stop + `\])}]{11,}`)},
	{"auth-field", regexp.MustCompile(`(?i)` + lead + `auth\\*['"]\s*:\s*\\*['"][A-Za-z0-9+/=_\-][^` + stop + `]{11,}`)},
	{"cookie-header", regexp.MustCompile(`(?i)` + lead + `(?:set-)?cookie` + cookieOpener + cookieBody)},
}

// Matching names the classes that match s, sorted. Nil when none does.
func Matching(s string) []string {
	var out []string
	for _, c := range Classes {
		if c.Re.MatchString(s) {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}
