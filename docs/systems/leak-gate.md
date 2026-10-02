# Leak gate

## Purpose

Keep the real names of the operator's machines, people and brands out of this public repository, and keep
them out after the sweep that removed them. A test fails the build when any tracked file, or any tracked
file name, holds a denied name; the list of denied names is a secret, so the repository carries only keyed
hashes of it.

This is a guard against accident and drift. It is not a defence against a collaborator with write access
(see [What it does not cover](#what-it-does-not-cover)).

## The shape of it

| piece | where | what it is |
|---|---|---|
| the list | outside the repository | one entry per line, `mode<TAB>text`. The real asset; backed up by the operator |
| the key | outside the repository | 32 random bytes as 64 hex characters. A random MAC key, not a password: it can be re-minted at any time |
| the digest file | `testdata/leak-gate-digests.json` | the list as HMAC-SHA256 digests, plus a MAC over the whole file and the exempt rows. Reveals the count and lengths of entries, never a name |
| the library | `internal/leakgate/` | the matcher, the fold, the PNG walk, the tree scan with its fail-closed rules, the shape rules, key resolution. Pure: it reads no environment variable and starts no process |
| the generator and local scanner | `cmd/leakdigest/` | `genkey`, `gen`, `check`, `scan`, `patterns` |
| the tree gate | `leak_gate_test.go` | five tests, below |
| the CI wiring | the `Test` step of `.github/workflows/ci.yml` | the key from an Actions secret, and whether the gate is required |

The digest file is tamper-evident: adding or removing an entry, an allow row or an exempt row without the
key fails the MAC check. A canary entry is always appended to the list, and every scan must flag it, so a
blind scanner fails loudly even where the tree gate skips.

## The tests

| test | needs the key | what it does |
|---|---|---|
| `TestTrackedTreeCarriesNoDeniedNames` | yes | every tracked file, and every tracked file name, against the digests. Skips visibly without a key unless the gate is required |
| `TestTrackedTreeCarriesNoShapedIdentifiers` | no | the shape rules over the tracked tree: a GPU id, a WSL UNC path naming a distro that is not public |
| `TestLeakGateKeyResolution` | no | the behaviour table of the key rows below, including the empty values an unset Actions secret renders as |
| `TestLeakGateScannerBehaviour` | no | a synthetic key and synthetic names: the canary is flagged, a scanner built with the wrong key is blind and says so |
| `TestLeakGateDigestFileIsWellFormed` | no | the committed file parses strictly, is in the generator's canonical form, and respects the ratchets |

The full behaviour matrix of the matcher (about a hundred cases: camel and digit glue, escapes, accents,
zero-width characters, percent forms, phrases, drive letters) is in `internal/leakgate`, with synthetic names.

## Entry modes

The list holds five kinds of entry. Each is hashed with its own code, so the same word in two modes has two
digests.

| mode | a finding is |
|---|---|
| `sub` | any window of one alphanumeric run, compared in lower case |
| `word` | the same, with letter boundaries: a camel cut, a digit or a non-letter ends the word, a plural or a longer word does not match |
| `exact` | the whole run equals the entry (numbers and model codes) |
| `subcs` | any window of a run, compared as written: for words whose case separates two classes |
| `phrase` | two or three consecutive runs on one line |

Before matching, the text is folded: JSON `\u` escapes are decoded, zero-width characters are removed,
accented Latin letters lose their marks, full-width forms become ASCII. A second pass blanks backslash
escapes (`\n`, `\t`, ...), `\x` escapes and `%XX` forms in place, because an escape letter glues onto the next
word. File names are scanned too, as one line each, under their own key, so a name finding never shares an
allow key with a body finding.

## What fails closed

A finding is never the only way to fail. Each row below is a failure that names the path and the reason, never
matched text, unless an exempt row bound to the exact path and git blob id says otherwise.

| input | rule |
|---|---|
| a tracked file name | scanned always, whatever the content |
| a text file | scanned; a UTF-8 mark is stripped; UTF-16 with a mark is decoded first, and its raw bytes are scanned too (a plain ASCII file with a stray mark decodes to other scripts and would hide its names) |
| a file with a UTF-16 mark that is not valid UTF-16 (an odd payload, an unpaired surrogate) | fails like an unknown binary |
| a PNG | every chunk except pixel data is scanned, compressed text chunks are inflated, bytes after the end marker are scanned; a bad signature or a missing end marker fails |
| an unknown binary | fails (a NUL in the first 8000 bytes) |
| a file over 16 MiB | fails |
| a run of more than 96 alphanumeric characters | fails, naming the line |
| a symlink or a submodule | fails |
| a read error other than "does not exist" | fails |
| a path in the index but not in the work tree | counted; fails when the gate is required |
| an empty file set, or `git ls-files` failing while required | fails: the gate went blind |

There are no skip globs, and the allow list is empty and ratcheted to zero in the test source: a finding is
fixed in the file, never allowed. The exempt list is ratcheted to three rows (today the three font files,
exempt as binaries); raising either number is a reviewed change to `leak_gate_test.go`.

## Behaviour: the key, the requirement, the result

The inputs are a key (K), whether the gate is required (R, `OFFLOAD_LEAK_GATE_REQUIRED` exactly `1`), and a
valid digest file (D). Every key input is trimmed of surrounding whitespace and an **empty value means
unset**: an unset Actions secret reaches a step as an empty string.

| state | result |
|---|---|
| K and a valid D | enforce: a finding fails the test; clean passes and logs `leak gate: enforced (N files, M entries)` |
| K, digest file missing, unparseable or without entries | fails |
| K, MAC mismatch (wrong key, or the list edited without the key) | fails: "digest file does not match this key" |
| no K, required | fails: "key missing where secrets exist" |
| K non-empty and not 64 hex characters | fails: "malformed gate key" (nothing about its length) |
| no K, not required (a fork's pull request, a contributor's machine, a source tarball) | skips, with the reason, and writes `leak gate: skipped (reason)` to the step summary |
| K valid, previous key empty (every normal CI run) | the previous key is ignored |
| K valid, previous key non-empty and not 64 hex | fails: "malformed previous gate key" |
| no K, previous key present | the previous key never counts as the key |
| key file variable empty | not consulted; set but absent, unreadable or malformed: fails |
| the canary not flagged | fails, wherever a scan runs |
| `OFFLOAD_LEAK_GATE_REQUIRED` neither empty nor `1` | fails: "unrecognized OFFLOAD_LEAK_GATE_REQUIRED value" |

Key resolution order: `OFFLOAD_LEAK_GATE_KEY`; else `OFFLOAD_LEAK_GATE_KEY_FILE`; else the default file
`leak-gate.key` in `offload-harness/` under the user config directory. `OFFLOAD_LEAK_GATE_KEY_PREV` is read
only when a key was found and lets one digest file verify under either key during a rotation.

Nothing prints the key, its length, a hash of it or the expected MAC. A finding prints
`path:line:col id=<first four bytes of the entry digest> mode`, never the matched text, because CI logs of a
public repository are public.

## In CI

The `Test` step of the existing workflow carries the key as an environment variable and sets the requirement:
`1` on a push and on a pull request from this repository, empty on a pull request from a fork (GitHub withholds
secrets there, so the test skips visibly and the run summary says `skipped`). The workflow declares
`permissions: contents: read`. No workflow, job or trigger exists for the gate; it is one more test in
`go test ./...`.

A fork's pull request therefore runs without the keyed test. The push run on `main` after the merge is the
backstop, and a maintainer runs the gate on the fork's checkout before merging.

## Running it locally

A maintainer holds the key file and the list. With the key file:

```sh
OFFLOAD_LEAK_GATE_KEY_FILE=<key file> go test -count=1 -run 'TestTrackedTreeCarries|TestLeakGate' .
```

`-count=1` matters: a key file is outside the module, so Go's test cache cannot see it change. The gate opens
every directory that holds a tracked file so that adding or removing a file busts the cache.

A contributor without the key runs `go test ./...` as usual: the keyed test skips, and the keyless tests
(including the shape rules) still run.

Setting `OFFLOAD_LEAK_GATE_PLAIN=<list>` makes a failure print, beside each finding, the entry its id stands
for. That prints names: it is for the maintainer's own terminal and is never set in CI.

### Regenerating the digest file

After the list or the set of exempt files changes (or the key is re-minted):

```sh
go run ./cmd/leakdigest gen -plain <list> -key-file <key file> -exempt <exempt file> -out testdata/leak-gate-digests.json
go run ./cmd/leakdigest check -plain <list> -key-file <key file> -exempt <exempt file>
```

The exempt file is one `reason<TAB>path` row per line; the blob ids come from the git index. `genkey -out
<file>` mints a key (mode 0600, nothing printed). `scan -plain <list> [-dir DIR | -ref REV] [-files FILE]
[-digest FILE]` is the local acceptance scanner: it runs the same matcher, fold, fail-closed rules and shape
rules as the gate, over a work tree or the blobs of a commit, and prints plaintext names, so it is never run
in CI. `patterns -plain <list>` emits the same list as ERE lines for a local pre-push hook.

### Rotating the key

Mint a new key, regenerate the digest file with it, set `OFFLOAD_LEAK_GATE_KEY_PREV` to the old key and
`OFFLOAD_LEAK_GATE_KEY` to the new one **before** merging, and delete the previous-key secret after the merge.
While both exist the test accepts a digest file whose MAC verifies under either. If the key is lost, re-mint and
regenerate from the list. If the list is lost, the gate cannot be regenerated, which is why it is backed up.

## Fixing a finding

Rewrite the file with the repository's vocabulary: a node letter or a role phrase, never the real name. The
vocabulary rules are in [STYLE.md](../STYLE.md#privacy) and the terms in the [glossary](../glossary.md). Do not
ask for an allow row. A test, fixture or document that needs to show a denied name (to feed it to the gate, or
to assert it is absent) builds it from parts at run time, split inside the word, because the gate scans test
sources like every other file.

## What it does not cover

- **History.** Old commits, tags, releases, forks, pull request titles and bodies and issue text are outside
  the tracked tree. The tip plus the gate is the control.
- **Commit metadata.** Author and committer fields are set outside the repository; the controls are the
  maintainer's local git identity, the host's commit-email privacy and a commit-message check in a local hook.
- **Encoded or compressed content.** Base64, gzip, `data:` URIs, pixel data inside PNGs, homoglyphs from other
  scripts, a phrase wrapped over a newline or split by markup.
- **A collaborator with write access.** Anyone who can push a branch to this repository can read the Actions
  secret through a workflow or test edit. Fork pull requests never receive it.
- **Short GPU id forms.** The shape rule flags full ids, short pins of eight or more digits, and truncated ids
  with an ellipsis or two more groups; a bare four-to-seven-digit head with no ellipsis passes, because model
  numbers must.

## Source map

| path | role |
|---|---|
| `leak_gate_test.go` | the five tests, the key-and-requirement decision (`resolveLeakGate`), the step summary lines, the ratchets |
| `testdata/leak-gate-digests.json` | the committed digests, the MAC and the exempt rows |
| `internal/leakgate/leakgate.go` | entries, the list format, validation, findings |
| `internal/leakgate/match.go`, `fold.go`, `foldtable.go` | the tokenizer, the matcher and the fold |
| `internal/leakgate/scan.go`, `png.go` | the tree scan and its fail-closed rules; the PNG chunk walk |
| `internal/leakgate/shape.go` | the keyless shape rules |
| `internal/leakgate/digest.go`, `key.go` | the digest file and its MAC; key resolution and the required/skip decision |
| `internal/leakgate/filelist.go`, `patterns.go` | the `-files` grammar of the local scanner; the hook patterns |
| `cmd/leakdigest/` | the command |
| `.github/workflows/ci.yml` | the `Test` step's environment and the workflow permissions |
| `identity_lint_test.go` | the older, shape-only identity lint; unchanged |

## Related

- [STYLE.md](../STYLE.md#privacy): the vocabulary rules the gate enforces.
- [CONTRIBUTING.md](../../CONTRIBUTING.md#the-leak-gate): what a contributor needs to know.
- [glossary.md](../glossary.md): Leak gate, Reference box.
