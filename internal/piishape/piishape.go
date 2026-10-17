// Package piishape holds the credential- and identity-shaped patterns that the
// compaction-eval corpus vet refuses (the "vet classes"). It exists so a package
// that cannot import internal/compeval (compeval imports internal/agent) can
// still ask the same question compeval asks: does this text look like it carries
// a secret.
package piishape

import (
	"regexp"
	"sort"
	"strconv"
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

// The cookie-header class takes a whole span, and not the cookies one at a time. Once the name of a Cookie or
// Set-Cookie header and an opener fire (below), and the span holds a cookie value of 12 or more characters, the
// match runs from the header name to the first of
//
//   - the end of the line (a carriage return, a line feed, a JSON-escaped one, or the end of the text),
//   - the closing quote of the string the header sits in, which is a quote that opens no quoted cookie value: a
//     bare quote, or a backslash run of 1, 3, 7 or 15 and a quote (the levels of escaping that occur: a header
//     line, JSON text or the double-quoted string of a shell command, and each further layer of either; a
//     level is 2^k-1 backslashes after k layers), and
//   - the closing bracket of a list, when the opener is a list ("Set-Cookie": [ ... ], map[Cookie:[ ... ]]), or
//     the closing brace of a slice literal ([]string{ ... }, the Go-syntax print of a header); a list is one
//     span to its closing bracket, however many elements it has, across lines too,
//
// whichever comes first. What stands between is never inspected for an alphabet: a quoted value with a space,
// an apostrophe, an ampersand, a comma, a semicolon, a tab, an escaped quote, a shell dollar sign or backtick, or
// an HTML escape (backslash-u and four hex digits, which Go's encoder writes for &, < and >) is inside the span
// like everything else, and so is a bare value with a shell escape or a backslash in it. Over-redacting a cookie
// line is the accepted price: it is evidence of a header, not of the build. The only fine-grained rules are the
// one that starts a match, some cookie value on the line has 12 or more characters, so the header of a line of
// short cookies, the code that handles cookies and the prose about them stay quiet, and the two that tell the
// quote that opens a quoted value from the one that closes the string the header sits in: a quote opens a value
// only when what follows it can start one (not white space and not a separator, so base64 padding or an empty
// last cookie in front of the closing quote does not make that quote an opener), and a quoted value that is
// open at a line break goes on to its closing quote when that is within three more line breaks, and ends at the
// line when it is not.

// cookieFold is a header fold: a line break, which the gaps do not take (they take no line break), followed by
// the spaces or tabs of the continuation line, which they do take, as they take the JSON escapes of a line break
// and of a tab in tool-call arguments. A header can be folded between its name and the opener and between the
// opener and the cookies.
const cookieFold = `\r?\n`

// cookieGapPiece is one piece of what may stand next to the punctuation of an opener: a non-word character other
// than a line break, or a JSON escape. (The Basic header's gap also takes the word of a Go slice literal, so that
// its scheme can follow a slice's opener; a cookie header handles the slice literal as a list of its own.)
const cookieGapPiece = `(?:[^\w\r\n]|` + jsonEscape + `)`

// cookieGap is up to eight of those pieces, as few as possible.
const cookieGap = cookieGapPiece + `{0,8}?`

// cookieOpener is what stands between a cookie header's name and the cookies: the punctuation the Basic header
// takes (up to eight non-word characters, then a colon, an equals sign or an opening bracket, then up to eight
// more; a hash rocket is an equals sign and a greater-than sign, the second of which the gap takes), so a header
// line, a subscript and a hash are covered; or else a comma behind which a quote opens the string that holds the
// cookies, the form of a call that takes the name and the value as two arguments (Header.Set("Cookie", "...")).
// A bare comma stays quiet: "the cookie, path=/some/long/path" is prose. A header fold may stand before the
// punctuation and after it.
const cookieOpener = `(?:` + cookieFold + `)?(?:` + cookieGap + `[:=\[]|` + cookieGap + `,\s*\\*['"])` + cookieGap + `(?:` + cookieFold + cookieGap + `)?`

// cookieListOpener is the opener of a list: the punctuation, which a line break may precede or follow, then an
// opening bracket. The white space behind the bracket, a pretty-printed list's line breaks and indentation
// included, is taken by the steps of the list.
const cookieListOpener = `(?:` + cookieFold + `)?` + cookieGap + `[:=](?:` + cookieGapPiece + `|[\r\n]){0,8}?\[`

// cookieGoListOpener is the opener of a Go slice literal, the way the Go-syntax print of a header writes it
// (http.Header{"Set-Cookie":[]string{"a=b", "c=d"}}): the punctuation, then the type, then the brace. The list ends
// at the closing brace.
const cookieGoListOpener = `(?:` + cookieFold + `)?` + cookieGap + `[:=](?:` + cookieGapPiece + `|[\r\n]){0,8}?\[\]string\{`

// cookieQuoteLevels are the numbers of backslashes in front of a double quote that opens or closes a quoted
// cookie, by how many times the text has been escaped: none on a header line, one in JSON text and in the
// double-quoted string of a shell command or of code, and 2q+1 for each further layer (three when that string
// is itself JSON text, seven for JSON in a shell string in JSON, fifteen for a fourth layer). The pattern has no
// back-references, so it cannot ask that the closing quote carry as many backslashes as the opening one; each
// level is written out instead. The level also fixes which backslash-and-quote is a quote inside the value
// (twice the backslashes plus one) and which one closes it.
var cookieQuoteLevels = []int{0, 1, 3, 7, 15}

// cookieBreakLimit is how many line breaks a quoted cookie value that is open at a line break may run over to find
// its closing quote.
const cookieBreakLimit = 3

// backslashes is a pattern for n literal backslashes.
func backslashes(n int) string { return strings.Repeat(`\\`, n) }

// cookieQuote is the quote of a level.
func cookieQuote(n int) string { return backslashes(n) + `"` }

// cookieBreakRun is the most backslashes that stand in front of the n of a JSON-escaped line break at a level n:
// every layer of escaping doubles the backslashes of the line break (one after the first, two after the second, four
// after the third, eight after the fourth), and the quotes of that level are 2^k-1 of them. A line break that was
// escaped fewer times carries fewer.
func cookieBreakRun(n int) int { return (n + 1) / 2 }

// cookieUnitOf is one piece of a quoted cookie's body at a level n, as generous as it can be: single, which is a
// class of single characters (any that is not a quote, a backslash or a line break, in the plain unit); a quote
// inside the value (2n+1 backslashes and a quote); a backslash inside the value (2n+2 backslashes); and an escape,
// a backslash run and a character that is neither a quote nor a line break (an octal escape such as the one for
// a comma, a semicolon or a space, a JSON escape such as a tab or a unicode escape, a shell's escaped dollar sign
// and backtick). The backslash and quote that close the value are none of these, and in escaped text neither is the
// escape of a line break: a run of up to cookieBreakRun(n) backslashes and an n or an r. short is the pattern of the
// escape behind a run of up to that many backslashes (cookieShortEscape); a longer run takes any character that is
// not a quote, a backslash or a line break.
func cookieUnitOf(n int, single, short string) string {
	u := single + `|` + backslashes(2*n+1) + `"|` + backslashes(2*n+2) + `|` + short
	if n == 0 {
		return u
	}
	return u + `|\\{` + strconv.Itoa(cookieBreakRun(n)+1) + `,` + strconv.Itoa(2*n+1) + `}[^"\\\r\n]`
}

// cookieOpeningHex is what may follow a backslash-u at the start of a quoted value: anything but the four digits of
// the escapes Go's encoder writes for an ampersand, a less-than sign and a greater-than sign (0026, 003c, 003e; the
// class is case-insensitive, like the rest of the pattern, so 003C and 003E are out too).
const cookieOpeningHex = `[^0"\\\r\n]|0[^0"\\\r\n]|00[^23"\\\r\n]|002[^6"\\\r\n]|003[^ce"\\\r\n]`

// cookieShortEscape is the escape behind a backslash run that is too short to be an escaped backslash and a character
// (up to cookieBreakRun(n) backslashes, one at the plain level): any character but a quote, a backslash, a line break
// and, in escaped text, an n or an r (the escape of a line break). At the start of a quoted value (opening) it is
// also not the escape of a tab, and a backslash-u is not the escape of an ampersand or an angle bracket: they are
// white space and separators, which no value starts with.
func cookieShortEscape(n int, opening bool) string {
	run, not := `\\`, `"\\\r\n`
	if n > 0 {
		run, not = `\\{1,`+strconv.Itoa(cookieBreakRun(n))+`}`, not+`nr`
	}
	if !opening {
		return run + `[^` + not + `]`
	}
	return run + `u(?:` + cookieOpeningHex + `)|` + run + `[^` + not + `tu]`
}

// cookieSpanUnit is one piece of a quoted cookie's body at a level n.
func cookieSpanUnit(n int) string { return cookieUnitOf(n, `[^"\\\r\n]`, cookieShortEscape(n, false)) }

// cookieOpeningUnit is the first piece of a quoted value: the same, except that a single character may be neither
// white space nor a separator or a closer (a comma, a semicolon, a colon, a closing bracket, brace or paren, a pipe, an
// ampersand, an angle bracket). A quote that is followed by one of them is the quote that closes the string the header
// sits in, as in a last cookie that ends in an equals sign (base64 padding, or no value at all) in front of it; it
// does not open a value.
func cookieOpeningUnit(n int) string {
	return cookieUnitOf(n, `[^"\\\s,;:)}\]|&<>]`, cookieShortEscape(n, true))
}

// cookieBreak is a line break inside a quoted cookie at a level n: a raw one (a header wrapped by a terminal, a fold)
// and, in escaped text, the JSON escape of one (a carriage return and a line feed make one break).
func cookieBreak(n int) string {
	if n == 0 {
		return `\r?\n`
	}
	return `\r?\n|(?:\\{1,` + strconv.Itoa(cookieBreakRun(n)) + `}[nr]){1,2}`
}

// cookieBody is the body of a quoted cookie at a level n that is closed within cookieBreakLimit line breaks: units, and
// up to that many line breaks with units after each.
func cookieBody(n int) string {
	u := `(?:` + cookieSpanUnit(n) + `)*`
	return u + `(?:(?:` + cookieBreak(n) + `)` + u + `){0,` + strconv.Itoa(cookieBreakLimit) + `}`
}

// cookieCountUnit is one character of a quoted value for the count that starts a match: 12 of them, with no
// white space among them, from the opening quote (the rule a bare value follows, so a Go string concatenation
// that happens to follow an equals sign, `"Cookie: sid=" + f(x) + "..."`, is not a value). It is the units of
// the span, except that an escape counts as one character, so Go's backslash-u0026 for an ampersand is one and
// "Tom&Jerry" is nine characters in JSON text as it is in a header. A backslash-u that is not followed by four
// hex digits counts for nothing; the span takes it all the same.
func cookieCountUnit(n int) string {
	k := strconv.Itoa(2*n + 1)
	u := `[^\s"\\]|\\{1,` + cookieRunMax + `}u[0-9a-fA-F]{4}|\\{1,` + k + `}[0-7]{3}|` + backslashes(2*n+1) + `"|` + backslashes(2*n+2)
	if n == 0 {
		return u + `|\\[^"\\\r\nu0-7]`
	}
	h := strconv.Itoa(cookieBreakRun(n))
	return u + `|\\{1,` + h + `}[^"\\\r\nnru0-7]|\\{` + strconv.Itoa(cookieBreakRun(n)+1) + `,` + k + `}[^"\\\r\nu0-7]`
}

// cookieRunMax is the most backslashes in a run that is the escape of a bare value: a JSON unicode escape in text that
// has been escaped four times carries sixteen (one, doubled by each layer).
const cookieRunMax = `16`

// cookieBareLong is a bare cookie value of 12 or more characters (it ends before a quote, a space, a comma, a
// semicolon, an ampersand or a closing bracket, so a header in a curl command ends before the quote that closes
// the string and the value in a printed Go header before the bracket that closes its list); a JSON unicode
// escape counts as one character.
const cookieBareLong = `(?:[^\s'",;&\\\]]|\\{1,` + cookieRunMax + `}u[0-9a-fA-F]{4}){12,}`

// cookieQuotedLong is a quoted cookie value at a level whose first 12 characters are counted, taken whole: the rest
// of the body, then the closing quote when it is there. With cross, a body that is open at a line break goes on
// to its closing quote when that is within cookieBreakLimit line breaks, and ends at its line when it is not; a
// list's strings never run over a line break.
func cookieQuotedLong(n int, cross bool) string {
	q := cookieQuote(n)
	head := q + `(?:` + cookieCountUnit(n) + `){12}`
	units := `(?:` + cookieSpanUnit(n) + `)*`
	if !cross {
		return head + units + `(?:` + q + `)?`
	}
	return head + `(?:` + cookieBody(n) + q + `|` + units + `)`
}

// cookieLongOf is the equals sign and a cookie value of 12 or more characters, quoted at any level or bare as given.
func cookieLongOf(bare string) string {
	var alts []string
	for _, n := range cookieQuoteLevels {
		alts = append(alts, cookieQuotedLong(n, true))
	}
	return `=(?:` + strings.Join(alts, `|`) + `|` + bare + `)`
}

// cookieLong is a long cookie value on a line.
var cookieLong = cookieLongOf(cookieBareLong)

// cookiePairAt is a cookie whose value is a double-quoted string at a level n, from the equals sign to the closing
// quote: an empty value; a value that starts with a unit that can start one and runs to its closing quote, over up
// to cookieBreakLimit line breaks; or, when there is no closing quote in reach, the rest of the line (a line that
// the text was cut in). It is how the walk tells the quotes of a value from the quote that closes the string the
// header sits in: a quote that is not the closing quote of a pair ends the span, and a quote that nothing can
// follow as a value, white space, a separator or a closer, opens no pair.
func cookiePairAt(n int) string {
	q := cookieQuote(n)
	first := `(?:` + cookieOpeningUnit(n) + `)`
	return `=` + q + `(?:` + q + `|` + first + cookieBody(n) + q + `|` + first + `(?:` + cookieSpanUnit(n) + `)*)`
}

// cookiePair is a pair at any level.
var cookiePair = func() string {
	var alts []string
	for _, n := range cookieQuoteLevels {
		alts = append(alts, cookiePairAt(n))
	}
	return strings.Join(alts, `|`)
}()

// cookieBareEscape is a backslash run (a shell escape of a dollar sign, a backtick, a space or a semicolon, a path
// separator, a JSON escape or a doubled backslash of either) and the character it escapes. Not a quote, which closes
// the string the header sits in, and not a line break (the JSON escape of one is an n or an r).
const cookieBareEscape = `\\{1,` + cookieRunMax + `}[^"'\\\r\nnr]`

// cookieStep is one step over the span of a header line: a quoted pair, or any character that is not a quote, a
// backslash or a line break, or an escape that is not a quote or a line break. The quoted pair comes first, so the
// quote that opens it is not where the step stops. A single quote ends the span: an apostrophe in an unquoted
// value (name=O'Brien) cannot be told from the quote that closes a header written in single quotes.
var cookieStep = `(?:` + cookiePair + `|[^'"\\\r\n]|` + cookieBareEscape + `)`

// cookieLineBody is the span of a header on a line: steps, a long value, then steps to the end.
var cookieLineBody = cookieStep + `*?` + cookieLong + cookieStep + `*`

// cookieString is a string of a list at a level, with its quotes: the body, then the closing quote when it is
// there. Inside it a quote of the next level is a quote of a cookie value. A string that is open at a line break
// goes on to its closing quote when that is within cookieBreakLimit line breaks (a printed header holds the quoted
// value of a cookie as a string of the list, and that value may hold a line break).
func cookieString(n int) string {
	q := cookieQuote(n)
	return q + `(?:` + cookieBody(n) + q + `|(?:` + cookieSpanUnit(n) + `)*)`
}

// cookieSingleString is a single-quoted string of a list (Python's repr of a header list).
const cookieSingleString = `'(?:[^'\\\r\n]|\\[^\r\n])*'?`

// cookieListStepClosing is one step over a list that ends at the closer (the class of the characters a bracket or a
// brace is): a string at any level, a single-quoted string, or anything else that is not the closer, a quote or a
// backslash, and the JSON escapes. A closer inside a string is part of the string (Django's messages cookie holds
// two closing brackets).
func cookieListStepClosing(closer string) string {
	var alts []string
	for _, n := range cookieQuoteLevels {
		alts = append(alts, cookieString(n))
	}
	return `(?:` + strings.Join(alts, `|`) + `|` + cookieSingleString + `|[^` + closer + `"'\\]|\\{1,` + cookieRunMax + `}[^"'\\\r\n])`
}

// cookieListLong is where a list holds its long value: after an equals sign outside a string (a printed Go
// header), inside a string at a level (the value bare, or quoted at the next level), or inside a single-quoted
// string.
var cookieListLong = func() string {
	alts := []string{cookieLong}
	deepest := cookieQuoteLevels[len(cookieQuoteLevels)-1]
	for _, n := range cookieQuoteLevels {
		// A quote of a cookie value inside a string at level n carries 2n+1 backslashes.
		inner := cookieBareLong
		if 2*n+1 <= deepest {
			inner = cookieQuotedLong(2*n+1, false) + `|` + cookieBareLong
		}
		span := `(?:` + cookieSpanUnit(n) + `)`
		alts = append(alts, cookieQuote(n)+span+`*?=(?:`+inner+`)`+span+`*(?:`+cookieQuote(n)+`)?`)
	}
	alts = append(alts, `'(?:[^'\\\r\n]|\\[^\r\n])*?=(?:`+cookieQuotedLong(0, false)+`|`+cookieBareLong+`)(?:[^'\\\r\n]|\\[^\r\n])*'?`)
	return `(?:` + strings.Join(alts, `|`) + `)`
}()

// cookieListBodyClosing is the span of a list that ends at the closer: steps, a long value, then steps to the closer.
func cookieListBodyClosing(closer string) string {
	step := cookieListStepClosing(closer)
	return step + `*?` + cookieListLong + step + `*`
}

// cookieHeader is the cookie-header class: a bracketed list, a Go slice literal, or a line.
var cookieHeader = `(?i)` + lead + `(?:set-)?cookie(?:` +
	cookieListOpener + cookieListBodyClosing(`\]`) + `|` +
	cookieGoListOpener + cookieListBodyClosing(`}`) + `|` +
	cookieOpener + cookieLineBody + `)`

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
//   - cookie-header: a Cookie or Set-Cookie header, taken as one WHOLE SPAN and not a cookie at a time. The
//     name and an opener fire (the punctuation the Basic header takes may stand between them, so a JSON list, a
//     subscript, a hash and a printed Go header match; a comma counts only when a quote follows it, as in
//     Header.Set("Cookie", "..."), so prose stays quiet; a header fold, a line break, may stand before the
//     punctuation and after it) and the span holds a cookie value of 12 or more characters: 12 characters with no
//     white space from the start of a bare value or from the opening quote of a quoted one, a JSON unicode escape
//     counting as one. The match then runs from the header name to the first of the end of the line (a line break,
//     its JSON escape or the end of the text), the closing quote of the string the header sits in (a quote that
//     opens no quoted cookie value: a bare quote, or a backslash run of 1, 3, 7 or 15 and a quote, which are the
//     levels of escaping of a header line or a shell command, of JSON text, and of each further layer; a single
//     quote ends it too, so a curl command's header ends before the shell's closing quote) and the closing bracket
//     of a list or the closing brace of a Go slice literal (a JSON list, a printed Go header, the Go-syntax print
//     of one, a Python list: one span however many elements and lines it has, a bracket inside a string element
//     not ending it). The closing quote and the bracket are not part of the match, so redacted text keeps its
//     quoting and Args stay valid JSON. Nothing inside the span is read for an alphabet: a quoted value with a
//     space, an apostrophe, an ampersand, a comma, a semicolon, a tab, an escaped quote, a doubled backslash, an
//     octal escape, a shell escape (an escaped dollar sign or backtick) or one of Go's HTML escapes
//     (backslash-u0026, 003c, 003e) is inside it, a bare value with a shell escape or a backslash path is inside
//     it, and so is every other cookie and attribute on the line. Over-redacting a cookie line is accepted by
//     design: it is evidence of a header, not of the build. Two rules tell a quote that opens a value from the
//     one that closes the string the header sits in. A quote after an equals sign opens a value only when a
//     value can start behind it (not white space, not a separator or a closer, so base64 padding or an empty
//     last cookie in front of the closing quote does not make it an opener; an empty value is still a value),
//     and a quoted value that is open at a line break goes on to its closing quote when that is within three more
//     line breaks, and ends at its line when it is not (a truncated line takes the rest of its line and no
//     more). An apostrophe in an unquoted value ends the span (name=O'Brien cannot be told from the quote that
//     closes a header written in single quotes), so a cookie behind one is not covered, and neither is one
//     behind a bare double quote inside a value (a raw JSON value). The fine-grained grammar of a cookie value
//     lives only in the harvest redactor, which remembers the values of a span for its value-tail gate.
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
// single quote or an apostrophe in an unquoted value, a cookie behind a bare double quote inside an unquoted value,
// a quoted cookie whose quotes carry a number of backslashes that is no level of escaping (not 0, 1, 3, 7 or 15),
// a quoted value that starts with white space or a separator, a Go slice literal whose type is left out, a header
// folded again after its first cookie, and a cookie passed apart from its header name (curl -b, a jar).
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
	{"cookie-header", regexp.MustCompile(cookieHeader)},
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
