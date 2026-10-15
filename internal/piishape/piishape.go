// Package piishape holds the credential- and identity-shaped patterns that the
// compaction-eval corpus vet refuses (the "vet classes"). It exists so a package
// that cannot import internal/compeval (compeval imports internal/agent) can
// still ask the same question compeval asks: does this text look like it carries
// a secret.
package piishape

import (
	"regexp"
	"sort"
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

// openQuote is a quote with any number of backslashes in front of it: JSON text doubles them, and JSON
// text inside a double-quoted shell string doubles them again.
const openQuote = `\\*['"]`

// afterName is what may stand between a name and the operator that follows it: the closing quote of a
// quoted name and a closing bracket or paren (os.environ["API_TOKEN"] = , {'password' => , "token": ).
const afterName = `(?:` + openQuote + `)?[\])]?`

// typeAnno is a type written between a name and its value (": string", " string", ": Optional[str]",
// ": Record<string, string>").
const (
	typeName = `[A-Za-z_](?:[\w.*\[\]<>|?]|` + htmlEscape + `)*`
	typeAnno = `(?:\s*:\s*|\s+)` + typeName + `(?:,\s*` + typeName + `)*`
)

// assignPlain is the assignment the class always matched: a key word, = or :, then a value that may start
// with a quote. Or else _PASS= or _PWD=: the short words need the underscore and the equals sign, so a test
// runner's "PASS: TestName" and the shell's PWD stay quiet.
const assignPlain = `(?:` + leadUnderscore + credName + `(?:\s*` + ansi + `)*\s*[=:]|_(?:pass|pwd)\s*=)\s*(?:` + ansi + `\s*)*(?:` + openQuote + `)?` + valueBody

// assignQuoted is an assignment whose value is a quoted literal, with a wider operator between the name and
// the literal: Go's :=, a comparison (== or !=), a hash rocket (=>), a type annotation in front of the =
// (const token: string = "..."), a closing quote or bracket after the name, and a b, r, u or f prefix on the
// string. A quote is required: an unquoted value after := or == is an identifier or a call (token :=
// strings.TrimSpace(x)), and Go source assigns to token-named variables all day.
const assignQuoted = leadUnderscore + credName + afterName + `(?:` + typeAnno + `(?:\s*` + ansi + `)*\s*=|(?:\s*` + ansi + `)*\s*(?::=|[=!]==?|=(?:>|` + htmlEscape + `)|[=:]))\s*(?:` + ansi + `\s*)*[bruf]{0,2}` + openQuote + valueBody

// assignCall is a call that takes the name and the literal as two arguments: Setenv("API_TOKEN", "...").
// The comma only counts right behind an opening paren and a quoted name, so a list of strings
// (["token", "client_identifier"]) stays quiet. The match starts at the paren.
const assignCall = `\(\s*` + openQuote + `[A-Za-z0-9_.-]*` + credName + openQuote + `\s*,\s*[bruf]{0,2}` + openQuote + valueBody

// emailDomainStart is the first character of an address's domain, which may not be a lowercase v
// followed by a digit: that is a Go module version (golang.org/toolchain@v0.0.1-go1.27.2.linux-amd64),
// which every stack trace and module-cache path in a Go log carries and which otherwise reads as an
// address ending in ".linux".
const emailDomainStart = `(?:[A-Za-uw-z0-9-]|v[A-Za-z.-])`

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
//   - cookie-header: a Cookie or Set-Cookie header that holds a value of 12 or more
//     characters; the match runs to the end of the line or the closing quote, so
//     the other cookies on the line go with it.
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
// backslash inside it, an UNQUOTED value after :=, == or => (an identifier or a
// call, by design), a value passed apart from its name (fmt.Sprintf("password=%s", v)),
// a terminal control sequence other than escape-bracket-letter (a charset switch,
// an operating-system command, the one-byte form of the bracket), and a Basic
// header whose name and scheme are more than eight punctuation characters apart or
// have a word between them.
var Classes = []Class{
	{"email", regexp.MustCompile(`(?:` + jsonEscape + `)?[A-Za-z0-9._%+-]+@` + emailDomainStart + `[A-Za-z0-9.-]*\.[A-Za-z]{2,}`)},
	{"private-key-block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"bearer-token", regexp.MustCompile(`(?i)` + lead + `bearer\s+[A-Za-z0-9_\-][^` + stop + `]{15,}`)},
	{"api-key-assignment", regexp.MustCompile(`(?i)(?:` + assignQuoted + `|` + assignPlain + `|` + assignCall + `)`)},
	{"aws-access-key", regexp.MustCompile(lead + `AKIA[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(lead + `gh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"provider-key", regexp.MustCompile(lead + `sk-(?:(?:ant|proj|svcacct|admin)-[A-Za-z0-9_\-]{20,}|[A-Za-z0-9]{20,})`)},
	{"jwt", regexp.MustCompile(lead + `eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	{"basic-auth", regexp.MustCompile(`(?i)` + lead + `authorization` + headerGap + `{0,8}?[:=,>\[]` + headerGap + `{0,8}?basic\s+[A-Za-z0-9+/=_\-][^` + stop + `\])}]{11,}`)},
	{"auth-field", regexp.MustCompile(`(?i)` + lead + `auth\\*['"]\s*:\s*\\*['"][A-Za-z0-9+/=_\-][^` + stop + `]{11,}`)},
	{"cookie-header", regexp.MustCompile(`(?i)` + lead + `(?:set-)?cookie(?:\\*['"])?\s*[:=]\s*(?:\\*['"])?[^'"\\\r\n]*?=[^\s'",;&\\]{12,}[^'"\\\r\n]*`)},
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
