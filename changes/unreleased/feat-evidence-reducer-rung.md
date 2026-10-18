### Added - an optional evidence-preserving reducer for the agent's build and test results, default off

- **What it does.** With the config key `agent_evidence_reducer` set, a `run` or `run_shell` result that is a build or test
  invocation and is 4 KiB to 64 KiB is no longer carried into the transcript whole. The cascade seat the agent's own offload
  tools already use (`Build`'s `Offload`: the recordless offload, one named tier, the `extract` task, no new client and no ledger
  row) is asked for at most five claims, each with a `claim` and a one-line `quote`. Every quote is looked up in the output with
  `quotecheck.Locate`, the lookup behind the research digest's quote check (the output's bytes first, then case, spacing, quote
  marks and punctuation set aside, publishing the output's own span), and the transcript keeps a receipt in place of the log: an opening
  line `[evidence-receipt: original N chars, M lines; ...]`, the tool's `exit=` line copied by code and never written by the
  seat, each claim over its verified quote, and a closing line saying the rest is not kept and the command can be run again.
  Unset, the key is off and the loop is byte-identical to before.
- **When the raw output is kept, unchanged.** Output under 4 KiB or over 64 KiB; output with no `exit=` first line; any tool that
  is not `run` or `run_shell`; a command the producer gate (the log-collapse rung's `logsCallIsBuild`) does not read as a build or
  test invocation, which is what bypasses reads and searches (`read_file`, `search_files`, `list_dir`, `grep`, `cat`, `sed -n`,
  `git grep`, a chained command, a fetch, an unknown tool: the agent needs their exact text); a call that did not run or
  errored; a seat error, timeout (60 s), deferral or panic; an answer that is not the requested JSON, has no claims or more than
  eight; a claim with no text or a quote that is not one line of 8 to 200 characters; any quote that is not in the output as one contiguous span, an elided `first ... last` quote
  included (all or nothing); output the corpus vet's table says looks like a credential or an email address, checked before the seat is shown
  anything, and the receipt checked again; a receipt that is not strictly shorter than the output. A claim is collapsed to one line,
  so a seat cannot forge a receipt line (an exit status, a disclosure line) from inside a claim.
- **The credential check reuses an existing detector and moves it.** The only credential-shape detector in the tree was the
  compaction-eval corpus vet's class table (`piiPatterns`, `PIIClasses` in `internal/compeval`), and `internal/compeval` imports
  `internal/agent`, so the agent could not call it. The table and the question "which classes match this text" move, unchanged
  (same six classes, order and expressions), to the new `internal/piishape` (`Classes`, `Matching`). `compeval` derives its
  `piiPatterns` from it, `PIIClasses` delegates to `Matching`, and `VetPII`, the redactor and the residual gate behave as before
  (a test pins that the two tables are one). The reducer calls `piishape.Matching`. It flags an email address, so a log that prints
  one stays raw. The table was widened in the same change (next bullet).
- **The credential table is widened for both users.** `api-key-assignment` now also matches a key word that follows an underscore
  or other non-word character and may be followed by `access`/`key`/`secret`/`token` parts (`GITHUB_TOKEN=`, `OPENAI_API_KEY=`,
  `DB_PASSWORD=`, `AWS_SECRET_ACCESS_KEY=`), with the 12-character value floor kept; two classes are appended after the existing six,
  `provider-key` (bare `sk-` followed by 20 or more letters and digits, or `sk-proj-`, `sk-ant-`, `sk-svcacct-`, `sk-admin-` followed by 20
  or more key characters) and `jwt` (three base64url segments of 8 or more characters, the first starting `eyJ`). The corpus vet and the
  harvest redactor, which derive from the table, refuse and redact the same shapes. Settings and prose stay clean (`MAX_TOKENS=4096`,
  `token_count: 12`, "the password field is required", a git sha, a UUID, a base64 blob, `sk-learn`, `/tmp/secret_dir`), pinned by
  tests. Still not covered (stated, not detected): a camel-case name (`githubToken=`), a key name the table does not hold
  (`dbpassword=`, `sid=`), a command-line flag with a space (`--token value`, `curl -u user:pass`),
  URL userinfo (`scheme://user:pass@host`), an authorization scheme other than Bearer and Basic, a provider prefix the table does not list
  (`sk_live_`, `xoxb-`), an unsigned JWT, a high-entropy string with no key name or known prefix, a value that starts with `/`,
  `.`, `[` or `]`, the part of a value after a backslash inside it, an unquoted value after `:=`, `==` or `=>`, a value passed apart
  from its name (`fmt.Sprintf("password=%s", v)`), and a terminal control sequence other than escape, bracket, parameters and a letter.
- **A credential value is matched and redacted whole.** The first widening stopped a value at the first `.`, `/`, `+`, `=`, `@`, `~`
  or `:`, so the compaction-eval harvest redactor, which derives from the table, wrote `access_token=<jwt>` out as the key name, a
  placeholder and the JWT's payload and signature, `AWS_SECRET_ACCESS_KEY=<key>` with everything after its first slash, and a
  bearer value with a `/` or `+` the same way, and the residual gate saw nothing because a tail matches no class. The value of
  `api-key-assignment` and `bearer-token` now runs to the next whitespace, quote, comma, semicolon, ampersand or backslash (12 or more
  characters, 16 for a bearer token; a bearer value starts with a letter, digit, underscore or hyphen, an assignment value with any
  character except those and `/`, `.`, `[`, `]`, after an optional quote), so a value such as `ab/cdef...` or `!cdef...` after a secret name is recognised too, and
  the redactor replaces the value as a whole (the key name stays, as for every class). A value that starts with `/` or `.` (a path
  after `token_store=`) is not read as a secret; a relative path that starts with a letter is, the safe direction. Across the 3,959
  tracked files (3,736 read as text) the two classes matched 16 times before and 44 now (assignment 9 to 37, bearer 7 to 7); every new match is source
  code or a test that assigns something to a name containing `token` (a local variable or struct field whose name contains `token`) or a redaction
  test's own fixture, none in a corpus or testdata file.
- **The redactor substitutes the email class last.** An address-shaped piece inside an assignment's value used to be
  replaced first, leaving a placeholder where the credential class needs the value to start. The vet table keeps its order.
- **The residual gate is stronger than the vet.** `Redactor.Residual` (the trace harvest's and the trace corpus verb's exit gate)
  reports the vet classes that still match and `value-tail` when 12 or more consecutive bytes of any credential value the redactor
  replaced (assignment, bearer, bare provider keys, JWT; not email or the PEM header, whose pieces are ordinary text elsewhere)
  are still in the text. With whole-value replacement a tail cannot occur, which a 400-value property test pins, so the check is a
  second wall against the next widening that stops short; a mention of a replaced secret elsewhere in the same corpus, or ordinary code
  that reuses a piece of a value taken from `token = f(x)`, leaves that one trace out of the harvest with a note (the others are
  kept), the safe direction. In the trace corpus verb the same gate refuses the harvest.
- **A value stops at a backslash.** Tool-call arguments are JSON text. A value class that ran through a backslash took the closing
  quote or the next escaped command with it, so a redacted `Args` was no longer valid JSON and replay code that parses it lost the
  call (`export TOKEN=<value>\nls` became `export <placeholder> -la`). The value now ends at a backslash and may start with a
  quote or a JSON-escaped quote (with any number of backslashes in front of it, see below), and tests pin that the redacted `Args`
  stay valid JSON with the text after the value intact. A
  secret that itself holds a backslash leaves the part after it, which the limits above name.
- **A value may start with a symbol.** `DB_PASSWORD=!...`, `password: $...`, `secret=#...` and `token=(...` were neither
  redacted nor seen by the vet, because the value had to start with a letter, digit, underscore or hyphen. A value that starts with
  `/`, `.`, `[` or `]` is still not read as a secret (a path after a key name, an earlier placeholder).
- **A key word right after a JSON escape is recognised.** Tool-call arguments and JSON tool output are JSON text, so the newline
  before a line that starts with `PASSWORD=` is a backslash and an `n`, and a word boundary does not see one between them. Every
  class that starts with a word (assignment, bearer, AWS, GitHub, provider key, JWT) therefore missed a secret at the start of a
  line in an escaped write_file body or a heredoc: the redactor returned the Args unchanged, the residual gate and the corpus vet
  reported nothing, and the harvest wrote the password to the corpus. The classes now also accept a JSON escape (backslash and
  `n`, `r`, `t`, `b` or `f`, or backslash-`u` and four hex digits) in front of the first word, and the email class may start with one,
  so `\nbob@example.com` no longer loses the `n`. The match then carries the escape, so the redactor keeps the escape and puts the
  placeholder after it: redacted Args stay valid JSON and the lines around the secret are intact (pinned by a test that decodes
  the redacted Args and compares the lines, and by a harvest test through `HarvestTraces` and `WriteCorpus`). The reducer, which
  sees real newlines in tool output, was not affected on that path, but tool output that is itself JSON (a `go test -json` line)
  was.
- **More credential names.** The assignment class also reads `PGPASSWORD`, `passphrase`, `credential(s)`, `private_key`,
  `access_key`, `auth_key`, a plural `passwords`, a trailing digit (`password1=`) and a trailing `_value` or `_id` part
  (`client_secret_value=`, `secret_id=`), and `_PASS=` and `_PWD=` (`DB_PASS=`, `MYSQL_PWD=`). The short words need the
  underscore and an equals sign: bare `pass` would read the test runner's `PASS: TestName` line and bare `pwd` the shell's
  `PWD=`, and a test pins that those stay quiet along with `private_key_path=/etc/...` and `token_count:`.
- **Basic authorization, registry auth and cookies.** Three classes are appended after the existing eight: `basic-auth` (an
  `Authorization` or `Proxy-Authorization` header, or a quoted JSON key, with the Basic scheme and a value of 12 or more
  characters), `auth-field` (a quoted `auth` key with an encoded quoted value, the shape of a container registry's config) and
  `cookie-header` (a `Cookie` or `Set-Cookie` header holding a value of 12 or more characters, replaced as one whole span: see
  the cookie bullet below). Each match ends before
  the closing quote or backslash, so redacted text keeps its quoting and Args stay valid JSON, and the residual gate remembers the
  credential part of each (the encoded value, or every cookie value in the span, a quoted one whole, but not its attributes, not the header name). The words alone ("Basic
  authentication", `Authorization: Basic` with no value, `{"auth":"none"}`, `Cookie: theme=dark`) stay quiet. The corpus vet and
  the harvest redactor derive from the table, so both refuse and redact the new shapes.
- **A terminal control sequence before a key word is recognised.** The `m` that ends a color code is a word character, so a
  word boundary never saw the key word after it: `ESC[32mpassword=<value>`, `ESC[1;34mpassword: <value>`, a colored
  `Bearer`, `ghp_`, `sk-`, `Authorization: Basic` or `Cookie` line passed the table, the redactor, the residual gate and the
  reducer's seat check, and colored build and test output (`FORCE_COLOR`, `--color=always`, CI) is what the reducer targets. Every
  class that accepted a JSON escape before its first word now also accepts a control sequence (the escape byte, a bracket,
  parameters and a letter, as the raw byte or as its JSON escape `\u001b`); the assignment class also accepts one between the name
  and the colon (`DB_PASSWORD ESC[0m: <value>`) and after the operator, and a flag letter in front of a key word (`-Dpassword=`,
  `-DTOKEN=`). A value ends at the escape byte, so the color reset behind it is not eaten, and the redactor keeps the sequence
  that opens a match, like a JSON escape (`piishape.Lead` names it; the redactor no longer carries its own copy of the escape
  pattern). The cookie class does not stop at the escape byte: a color reset inside a cookie line goes with the rest of the line, as
  that class already takes the whole line. Quiet cases are pinned: `PASS: TestName`, `token_count=1234`, `MAX_TOKENS=4096` and the word `password` in colored prose.
- **A quote may carry any number of backslashes.** The assignment, Basic and registry auth classes took one optional
  backslash before a quote. JSON text inside a double-quoted shell string doubles them again, so `bash -c "export API_KEY=\"<value>\""`
  as Args has three backslashes in front of the value's quote and the value was neither redacted nor seen by the residual gate. The
  prefix is now any number of backslashes; the redacted Args stay valid JSON with the text after the value intact. The cookie class
  has no back-references to count with either, so it writes out the levels it reads (none, one, three, seven and fifteen), as its bullets below say.
- **More assignment shapes, quoted literals only.** The assignment class put nothing but `=` or `:` between the key word and a
  value, so Go's `password := "<value>"`, `const token: string = "<value>"`, `var token string = "<value>"`, `password: str = "<value>"`,
  `if password == "<value>"`, `password = b"<value>"` and `r'<value>'`, `os.environ["API_TOKEN"] = "<value>"`, `{'password' => '<value>'}` and a quoted
  JSON key `"token": "<value>"` matched nothing, and a type annotation such as `token: Record<string, string> = "<value>"` was
  matched in place of the value, which left the value in the text. A second alternative (tried first) takes `:=`, `==`, `!=`, `=>`
  (also as Go's encoder writes it, `=>`), a type annotation, a closing quote or bracket after the name and a `b`, `r`, `u` or `f` string
  prefix, and requires a QUOTED literal of 12 or more characters: an unquoted value after `:=` or `==` is an identifier or a call
  (`token := strings.TrimSpace(line)`), which would flood. A third takes a call that passes the name and the literal as two arguments
  right behind an opening paren (`t.Setenv("API_TOKEN", "<value>")`, `setProperty("password", "<value>")`); the match starts at the paren, and a
  list of strings (`["token", "client_identifier"]`) stays quiet. The harvest now remembers the literal itself and not the type
  annotation or call in front of it, so ordinary code that shares a long type name with a redacted line is not read as a leftover.
  Measured on the tracked tree: the assignment class matched 37 places before and 64 after (counted before this fragment was written); the
  27 added are quoted fake values in test files and one config example, none an identifier. A type annotation counts only when an `=`
  follows it, so prose such as `password required: "..."` stays quiet. Not covered and now named: `fmt.Sprintf("password=%s", v)`, where the value is a
  separate argument, and an unquoted value after the wider operators. The table costs time: about 1.5 s per MiB for all classes
  (three runs, 1.51, 1.54 and 1.54 s over 1 MiB that matches no class; the figure the first version of this note gave, about 1 s,
  understated it).
- **A Basic credential in a printed header or a call is recognised.** The class took a colon or an equals sign straight after the
  header name, so a Go `http.Header` dump (`map[Authorization:[Basic <value>]]`), `req.setHeader('Authorization', 'Basic <value>')`,
  `req.Header.Set("Authorization", "Basic <value>")`, `con.setRequestProperty(...)`, `headers["Authorization"] = "Basic <value>"`, a
  hash rocket, `http.Header{"Authorization": []string{"Basic <value>"}}` and `[Authorization] => Basic <value>` were not matched (the
  Bearer form of the same dump was). Between the name and the scheme the class now takes a run of up to eight punctuation characters
  that holds at least one of `: = , > [` (a JSON escape and Go's `[]string{` opener count as punctuation); a word in between, or the bare
  words with no separator, stay quiet. The value ends before a closing bracket, paren or brace, which base64 never holds, so
  the redactor does not eat the closers of a dump or a call.
- **A quoted credential literal runs to its closing quote.** A quoted value ended at the first space, comma, semicolon or
  ampersand inside it, so a quoted literal holding an ampersand, a comma or a space after the name (an assignment, a JSON field, a single-quoted shell value, an `export`)
  were total misses whenever each side was shorter than 12 characters, in the plain assignment, in the wider-operator forms and in the
  call form. A literal that starts with a quote now has its own body: it runs to the closing quote of the same kind, takes in the other
  kind of quote and Go's JSON escape for `&`, `<` and `>` (so an ampersand in Args does not end it), and never passes a backslash (an
  escaped quote in JSON text), a line break or the escape byte of a control sequence. The closing quote stays out of the match, and the
  first-character rule (no whitespace, none of `/ . [ ]`) holds inside the quote, so a quoted path after a key name stays quiet. An
  unterminated quote runs to the end of the line. The harvest redactor remembers the value the class took (`piishape.AssignmentValue`
  names it from the same pieces) and no longer cuts the match at its last space, comma, semicolon or ampersand, so the value-tail gate
  covers the whole quoted value. The cost is precision, measured on the tracked tree: the assignment class matched 67 places before and
  82 after (a scan of the tracked text files, counted before this fragment was updated). The 15 added are prose in quotes after a key
  name (a status map entry for `credentials`, a test string that starts with `secret = "the goal named`), placeholders with spaces in doc
  examples, and a Go string literal's closing quote read as an opening one (`... token: ", tailJWT(), ...`); none is a credential, and the
  cookie class matched nowhere in the tree. Quiet cases are pinned (a literal under 12 characters, a name that only contains the key word,
  a leading `/`, blank padding, a bare word run with no quote, a literal that continues on the next line).
- **The cookie header takes a whole span, not a cookie at a time.** The release lane's reading found seven defects of one design, three of
  them high: the class walked the line cookie by cookie, with a fine-grained rule for what a cookie value may hold, and every value
  outside that alphabet ended the match with the credentials behind it going to the seat and the corpus verbatim. A quoted value with a
  space, an apostrophe, an ampersand, a comma, a semicolon, a tab, a shell escape (an escaped dollar sign or backtick at the third level)
  or one of Go's HTML escapes for `&`, `<` and `>`; a Set-Cookie list whose first element was short or whose later elements held the
  credential; a header folded after the colon; and a cookie named like a credential (`refresh_token=`, `password=`) that the assignment
  class half-took first were each a way out. The design changes instead of a seventh patch. Once the header name and an opener fire (the
  Basic header's punctuation, a comma followed by a quote as in `req.Header.Set("Cookie", "...")`, a header fold on either side) and the
  span holds a cookie value of 12 or more characters, the match runs from the header name to the first of the end of the line (a line
  break, its JSON escape or the end of the text), the closing quote of the string the header sits in (a quote that opens no quoted
  cookie value: bare, or with 1, 3, 7 or 15 backslashes, the levels of escaping after none to four layers; a single quote ends it too, so
  a curl command's header ends before the shell's closing quote) and the closing bracket of a list or the closing brace of a Go slice
  literal (`"Set-Cookie": [ ... ]`, a printed Go header, the Go-syntax print of one, a Python list: one span however many elements and
  lines it has, a bracket inside a string element not ending it). The closing quote and the bracket stay out of the match, so the
  redacted Args stay valid JSON (a JSON document that held the header as a key does not, since the match starts at the key's name).
  Nothing inside the span is read for an alphabet. Over-redacting a cookie line is accepted by design: it is evidence of a header, not of
  the build. What starts a match stays precise: some value in the span has 12 characters with no white space from its start (an escape
  counts as one), so `http.Cookie{...}` literals, `Header.Get("Cookie")`, a Go string concatenation such as `"Cookie: sid=" + f(x)`,
  Django's short messages and the HTTP reference's example header stay quiet. The fine-grained value grammar now lives only in the harvest
  redactor, which remembers every value in the span for the value-tail gate (a quoted body whole, with its spaces, quotes, escapes, line
  breaks and the characters above, the closing quote found by the number of backslashes in front of it). The apostrophe gap stays and
  applies to an unquoted value only (`name=O'Brien` cannot be told from the quote that closes a header written in single quotes); quotes
  that are no level of escaping, a header folded again after its first cookie and a cookie passed apart from its header (`curl -b`) are
  named gaps. Measured on the first version of this class: a generated check of 17,057 lines (48 cookie shapes, 18 wrappings) broke the
  contract in all 18 wrappings with the old class and in none with the new one. The next bullet is the review of that version.
- **The whole-span cookie class after its review: seven findings fixed, red first.** A review of the whole-span class (verified one by
  one, each reproduced) found one high and six medium defects, every one a way a value of 12 or more characters reached the corpus with
  the residual gate clean, or the span ran into the next field. (1) The high one was the class order: run first, the cookie class took the
  key word of a quoted credential on the same line into its span (an unquoted header, then a credential whose value is quoted) and stopped
  in front of the quote, leaving the value with no key word for any class and no memory in the value-tail gate. A matrix of 8 cookie forms,
  12 credential forms and 12 layouts, as text and as Args, left 1,584 of 2,304 values in the text with the old order and none with the
  table's order; the redactor ran the cookie class in the table's order for a time (the next bullet says where it runs now). (2) A
  last cookie that ends in an equals sign (base64 padding, or no value) in front of the closing quote of the string made that quote the
  opener of a value: the span ran through the next field, key or argument, broke the Args JSON, and when the next key was a second header's
  name its value was left outside every match. A quote now opens a quoted value only when a value can start behind it (not white space, a
  separator, a closer or the escape of a tab, an ampersand or an angle bracket). (3) A header printed with the Go-syntax verb is a slice
  literal; the span stopped at its first element, or matched nothing when that was short: it is now a list that ends at the closing brace.
  (4) The quotes of JSON in a shell string in JSON carry seven backslashes (fifteen with a fourth layer); the class knew none, one and three:
  the levels are now 0, 1, 3, 7 and 15. (5) A line break inside a quote that is still open ended the span; the value now goes on to its
  closing quote when that is within three more line breaks and ends at its line when it is not. (6) A shell escape (an escaped dollar sign,
  backtick, space or semicolon) or a backslash path in an unquoted value ended the span or stopped any match, and a quoted value cut by the end
  of the text behind a long bare one was left; both are inside the span now. (7) A table of single-guard mutants showed guards no test
  reached; tests pin them and the dead alternatives (the hash rocket's greater-than sign in the openers of a list and of a slice literal (the one in the line opener was live and came back, see the next bullet), a lower bound that could not be
  reached, a SameSite entry that could not hold 12 characters) are deleted.
  Red first: 13 failing tests and a generator axis were committed before any fix and run against the previous class, then the guard tests (some red,
  some green by design and red against the mutant that removes the guard, each said so in its commit), then the fix. The generator grew from
  17,057 to 74,119 cases (60 cookie shapes, 46 wrappings: header last in its string, a document, a command followed by data, lists opened by
  an equals sign or a hash rocket, lists cut in their last element, the Go-syntax print of a header in four places, and 16 chains of two to
  four layers of JSON and shell escaping); against the previous class 34 of the 46 wrappings break the contract (29,271 lines with the wrong
  span, 28,024 leaking a value, 67 breaking the Args JSON) and none does with the new one. Measured with the new class: it matches nowhere in
  `internal/` and `docs/` (2,276 files, 27 MB), and in Go's `net/http` source (158 files, 2.8 MB) the same four places as before, each with a
  real cookie value in it; it scans 1 MiB of Go source in about 0.1 s, as before (the next bullet has the cost of text built to be slow for it and the size of the pattern). A mutation pass after the fixes (47 mutants of the class, 29 of the harvest scanner: each level, break limit,
  opening rule, escape bound, closer, list opener, count and stop character) killed all but one, which is equivalent (the index at which the
  scanner resumes after a closing quote, which is only re-read as a quote that is not followed by an equals sign). Named gaps after the review: a raw JSON value (a bare double quote inside an unquoted value) ends the span at the
  quote, and so does an apostrophe; a backslash and a lowercase n or r in an unquoted value is the JSON escape of a line break and ends the span (a raw path such as `dir=C:\new\x` ends it there); a quoted value that starts with white space or a separator is not a pair; quotes with two, four to six or
  eight to fourteen backslashes are no level; a Go slice literal with its type left out is read as a line.
- **The whole-span cookie class after its third review, red first.** A third review (every finding reproduced three times, most of them
  compared with the commit before the second review's fixes) found that those fixes had traded some defects for others, and some older ones. Most let
  a value of 12 or more characters through with the residual gate clean. (1) Order: with the credential classes run before the cookie class,
  a cookie named like a credential with a double-quoted value was taken by the assignment class, which leaves the closing quote behind its
  placeholder, and the span stopped at that stray quote, so every cookie behind it was written out; a JWT, a Bearer value or a key-word
  cookie inside an unquoted printed list (a header printed with the plain verb, a Java map) left a placeholder that holds the bracket that
  closes the list, and the span ended in it; and redacted Args lost their closing quote when an empty quoted cookie came ahead of a key-word
  cookie in such a list. The cookie class runs first again, so the span sees the text as written, and the reason it had moved is answered
  another way: when the cookie class has substituted anything, the credential classes also run over the text as it was written, and every
  credential value of 12 or more characters they find is substituted wherever it still stands (longest first, with its own placeholder,
  remembered for the value-tail gate); the 2,304-case matrix of an unquoted header followed by a quoted credential stays green. (2) A header
  printed with the Go-syntax verb holds its keys in sorted order, so a Cookie with short values stands in front of a Set-Cookie with long
  ones, and a string of a list that could stay open let the pairing of quotes shift by one: the match ran from the first key over the
  closing brace of its list to the long value of the second and on past the quote that closes an Args string. A JSON object of two lists, a
  Python dict and a Python list with a quoted value inside Args (the single-quoted string looked for the quote of the value at one level
  only) did the same. A string of a list is now closed in the middle of it and open only as its last element, and a single-quoted string
  looks for a quoted value at every level. (3) The count that starts a match took a JSON unicode escape as one character and nothing else,
  so a shell escape, an escaped slash or a backslash path inside the first 12 characters of the only long value left it whole; a capital N
  or R after a backslash (a Windows path) ended the span as if it were the escape of a line break. A bare value counts the escapes the span
  takes now, and only a lowercase n or r is the escape of a line break. (4) The greater-than sign right behind the header name (an XML
  element, an arrow) was live, and the second review had deleted it as dead; it is back. (5) Cost: a text that repeats a header over and
  over, each one opening a list that never closes, was scanned to its end once per match and took the square of its length (16 KiB, 0.6 s; 1
  MiB, hours), because a match cannot be reported while a thread of higher priority is alive. A header line now takes no opening bracket (a
  list opens behind the punctuation, a space or the name), the line alternative goes first, and a quoted value starts a match by the rule of
  a pair (a quote followed by a closer or a separator closes what came before). (6) Three guards that a mutant table found unreached are
  pinned: the bound on the backslash run in the value scanner (a text cut in a backslash run made the redactor read past its end and panic),
  the quoted value that starts with a backslash-u escape of U+0100 to U+0FFF (Python's json.dumps writes one for every non-ASCII letter;
  without its alternative the long cookie behind it was written out), and the resume of the value walk after a closing quote.
  Red first: the redactor tests of each defect and the generator axes (the bare escape inside the 12 characters; a two-key header in four
  wrappings, a JSON object of two lists in three, a Python dict in two and a Python list in Args; in the redactor generator the cookies
  behind a key-word cookie, a JWT and a Bearer value in three more wrappings that keep an earlier placeholder inside an unquoted list) were
  committed before any fix and run against the class as it stood, and so were the scan-time tests (4 KiB 41 ms, 16 KiB 594 ms, a factor of
  15 for 4 times the text); the guard tests are green by design and red against the mutant that removes the guard, each run as `go test -overlay` of the one test. The sweep of credential values is pinned by the 2,304-case matrix of an unquoted header followed by a quoted credential, which leaves 1,589 failing lines (values still in the text) when the sweep is removed. The generator grew from 77,417 to 145,862 cases (74 cookie shapes, 56 wrappings). Measured: the pattern is
  132,494 program instructions (54,527 before this review) and compiles in 26 ms; the class alone scans 1 MiB of Go source in 0.1 s, and 1 MiB of each text that reviews found to be slow takes 0.4 to 1.0 s and grows in step with its length; a full redaction (every class) of 1 MiB of those texts takes 1.4 to 2.1 s against 1.2 s for source text. A random search over 4,900 units of up to a dozen pieces (header
  names, quotes at two levels of escaping, backslash runs, brackets, equals signs) and a hill climb on the slowest eight, run on a gate
  runner against the class after the first cost fix, still found four families whose time grew with the square of the length (16 KiB, 0.3 to
  1.2 s); the line-first order and the pair rule for a long value remove them, and two searches with new seeds against the final class
  (9,800 units and 16 hill climbs) found none: the units that looked slow at 32 KiB scale linearly at 0.7 to 1.3 s per MiB from 8 KiB to 512
  KiB. A new named gap: a bare value with a shell escape inside four layers of escaping (more than sixteen backslashes) is left whole.
- **A Go module version is not an email address.** The email pattern read `toolchain@v0.0.1-go1.27.2.linux-amd64`, which every Go stack
  trace and module-cache path carries, as an address. Measured on 9.4 MB of real Go build and test logs cut into 4, 16 and 64 KiB pieces,
  it matched 67.6 % of the 4 KiB pieces (55.7 % of the 64 KiB ones), so the reducer would have kept most Go logs raw; a domain may no
  longer start with a lowercase `v` and a digit, and the same logs now match 0.3 % (all real addresses). No credential class matched
  any piece, before or after the widening. The coding-agent page now says what the check is for: defense in depth on the seat path, the
  only automatic credential layer on the corpus path, and a best-effort shape list in both.
- **The quote lookup moves as well.** `internal/agent` could not import `internal/research` to call `research.VerifyQuotes`:
  research imports `internal/delegate`, and delegate's own tests import the agent, so the import is a cycle in delegate's
  test build. The lookup (`locateQuote` with the fold code under it) moves, verbatim apart from exported names, to the new
  `internal/quotecheck` (`Locate`, `NewFolds`, `How`). `research.VerifyQuotes` keeps its signature, its report and its
  behaviour and calls it; research's tests, which exercise it through `VerifyQuotes` and `AnnotateWire`, are unchanged and
  green, and `quotecheck` has tests of its own. The reducer calls `quotecheck.Locate` directly, so the agent's dependency
  closure does not grow by research, delegate, config or the ledger.
- **Where it sits.** At the tool-result boundary in the loop, after the per-result cap and the environment rules and before the
  read budget, the effect record and the pager, so each sees the bytes the transcript really carries, and only for a call that ran.
  It is not a rung of the compaction ladder, which rewrites older bodies under budget pressure and is model-free by design: this
  runs once, when a result is first produced, and costs one seat call on the loop's critical path (seconds per qualifying result).
  The ladder, its options and its default bytes are untouched.
- **A repeat is honoured.** A receipt is not the output. With the reducer on, an exact repeat of a reduced call is re-run once
  (the receipt counts as a destroyed result for the repeat breaker, the same recovery path the compaction rungs use) and that
  result is pinned and not reduced; any further repeat is refused as before. With the reducer off the breaker is unchanged, even
  for a body that merely opens like a receipt. `IsEvidenceReceipt` recognises a receipt by its opening line and nothing that only
  mentions it; the trace harvest asks it, so a transcript that holds a receipt is refused like any other already-shortened body.
- **Config and doors.** `agent_evidence_reducer` (bool, `false`), documented in the operator guide and in `config.example.json`
  (regenerated). `BuildConfig.EvidenceReducer` installs `Loop.WithEvidenceReducer(Offload)`; with the key on and no seam, `Build`
  says so in its notes and installs nothing. The local agent CLI (its three `Build` calls), the MCP front door and the fleet-node
  agent task read it from the node's config; a source-level test requires every `BuildConfig` literal that takes the node's agent
  policy to hand over the key. The prompt-replay builder takes no node policy and is left alone, so a prompt A/B is not perturbed.
- **Tests.** Red first, in four red commits each followed by its green one: the shared credential table and the shared quote
  lookup against stubs, the reducer's
  properties against a no-op stub (the tests that need a receipt fail), then the key's wiring (a missing key in the example and
  five doors without it failed first). The properties are tested through the loop with a scripted planner and a scripted seat: the 4 KiB edge (4095
  stays, 4096 is reduced), the 64 KiB ceiling, the receipt's claims, quotes and verbatim exit line (including `exit=137 (killed by
  signal 9)` against a seat that claims success), reads and searches bypassed with the seat never asked, a missing exit line,
  an unverifiable quote, a credential shape (the seat not even called), a receipt that is not smaller, twelve ways the seat can
  fail, forged lines, a refused call, the repeat path with the reducer on and off, and the harvest refusal. A run of 22 mutations
  (each guard, threshold, the producer gate, the pinned bypass, the repeat recovery, the exit check, the credential checks, the
  quote shape, the claim cap, the panic guard) killed all 22; two initial survivors (a quote-shape check made redundant by the
  span check, and the exit-status post-condition) led to removing the redundancy and to a direct test of the check. The four
  fixes that followed the release lane's delta read (quoted values, cookie quoted pairs, cookie openers, cookie escapes) are red first as
  well, a red commit before each fix commit; the value-tail test for every cookie value landed with the harvest code of the first fix, and
  was run red against the older harvest file. The value-tail test for the session value behind a quoted cookie with escapes was already
  green at its red commit (the harvest finds that value once it is inside a match); the red one is the redactor test for the
  session cookie behind a quoted cookie with escapes. The whole-span change of the cookie class is red first as well, one red commit
  with the generator and the probes, run against the old class: all 18 wrappings broke the contract, with 10,130 of the generator's
  17,057 lines matched to the wrong span, 8,642 leaking a value and 326 matching with no long value in them; then the change. The
  redactor probe for a key-word cookie was red on its class-order assertion only; that assertion was given up in the review below, whose
  order finding showed it protected the accounting and not the values.
- **Not verified here.** Nothing in this change ran against a live seat. The acceptance measurement is a seat A/B on the 9B and
  27B agent seats through the gate's non-inferiority mode (tool-call reliability and pass rate), with forwarded prefill tokens as
  the efficiency metric and never seconds; until it passes the key stays off. Also open: how often real seats return a usable
  answer (the `extract` task's fixed 512-token completion budget holds about five claims with short quotes, and an answer cut by
  it falls back to the raw output, which would make the step nearly inert); whether a seat quotes single verbatim lines as asked;
  the receipt's effect on the agent's later steps (the thing the A/B exists to find out); the seat-call latency on the critical
  path; and the planner-seat render of the cascade call (the offload closure renders on the planner seat without thinking when
  the planner is not the workhorse). Fall-back reasons are logged per attempt (`agent evidence reducer: kept the raw output of ...`)
  and not yet counted on the run result.
  The whole-span cookie class was tested on fixtures built from pieces, on this repository's tracked text and on the Go standard
  library's `net/http` source; no real harvested trace went through it, and the review's fixes were not run against a live seat either.
  The opening rule (a quote followed by white space or a separator opens no value) and the three-line-break bound are design choices
  pinned by tests, not measured against real traffic; the timings are one series on a noisy shared machine. The widened value class and the tail gate were tested on fixtures only; no real harvested trace was re-run through the redactor here.
  The same holds for the control-sequence, quoted-literal and header shapes: the fixtures are built from pieces, and no real colored
  build log or real harvested trace was run through them. The scan cost (about 1.5 s per MiB; 1.51, 1.54 and 1.54 s) is one series of three runs on a noisy shared machine, and so are the timings of the third review's slow texts (a desk machine at two threads, and a gate runner for the random searches), and the 67-to-82 count of assignment matches is one scan of this repository's tracked text.
