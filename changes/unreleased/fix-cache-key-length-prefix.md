### Fixed - the result-cache key can no longer be forged from request text: every part is length-prefixed, callers pass each ingredient as its own part, and every existing cache key changes once

- **One-time effect on upgrade: every existing result-cache key changes, so the cache starts cold.** The first call of each kind after the upgrade misses and
  runs the model, then stores its result under the new key. Nothing else changes: the cache is a transparent accelerator, a miss does what the request would have
  done with no cache at all, and there is nothing to configure or migrate. Entries stored under the old keys are never looked up again, and the cache has no
  eviction, so they stay in the cache file as dead weight until the file is removed. (The ask lane's own cache lives in memory for the life of one server process,
  so it has nothing stored to orphan.)
- **The defect (issue #4).** `cache.Key` joined its parts with a NUL byte and hashed the result, so `Key("a", "b")` and `Key("a\x00b")` were one key. One part of a
  key is arbitrary request text, and text that carries a NUL can forge the boundary between two parts, so a request could be served a result that was stored for a
  different request. The empty cases collided as well: `Key()` and `Key("")`, and `Key("", "")` and `Key("\x00")`, were each one key. `TestKeySeparatorCollision` had
  pinned the collision as known behaviour, with a `t.Skip` that was to fire once the encoding was fixed.
- **The encoding (`internal/cache`).** The preimage is a version tag, `offload/cache.Key/v2`, then for each part its byte length as 8 bytes big-endian and its
  bytes; the key is the hex SHA-256 of that. A length prefix is self-delimiting, so no byte inside a part can be read as the boundary between two parts, and the
  empty list, one empty part and two empty parts are three keys. The doc comment on `Key` states the encoding, why a separator cannot work for text that may hold
  any byte, and the rule for callers: pass each ingredient as its own part and never glue fields into one part with a separator of your own. The same rule is in
  `docs/systems/offload-pipeline.md`, under "How a result-cache entry is keyed".
- **The same defect one level up (`internal/pipeline`).** Length-prefixing cannot repair a part that a caller built by gluing fields, because the encoding cannot
  see inside a part. Five key ingredients were built that way, and each is now separate parts or goes through `cache.Key`, so the whole key has one framing and not
  a second one:
  - `runVisionGen` keyed `req.Input + "|" + the media identity` as one part, and the identity is the only carrier of the source file's content, so `("a|b", "c")` and
    `("a", "b|c")` named one entry and one file's cached description could be served for another. They are two parts now (`visionKeyFor` in `mediagate_test.go`, the
    test mirror, follows).
  - `cacheKeyForTierRender` folded the render into the model name (`model#render`); the render is a part of its own, so a model named `x#y` and the model `x` rendered
    `y` no longer share an entry.
  - `exemplarCacheTag` ended each exemplar field with a NUL, and an exemplar's input and output are past request and answer text that can hold one, so
    `(Input "a\x00b", Output "c")` and `(Input "a", Output "b\x00c")` had one tag while the prompts they inject differ.
  - `templateCacheTag` joined the system prompt, the grammar and the user template the same way. Both tags are now `partsHash`, which is `cache.Key` over the fields.
  - The transcription key is a named function, `transcribeCacheKey`, taking the content digest, model, language and protocol as parts, with the model once (the old
    key carried it twice). The joined `digest|model=...|lang=...|proto=...` string stays only as the input of the on-disk media stem, so no output file is renamed.
    This seam was not reachable end to end: the old key also carried the model as a part of its own, and the digest is a hash of the file. It is hardening, and its
    test says so.
  - `askcache.Key` keeps its own per-field length prefix as a second layer, so its keys do not rest on `cache.Key` alone. Only its comments, and the comment in its
    test that said `cache.Key` joins with a NUL, changed.
- **The params string (`internal/tasks`).** `StableParamsKey` renders `<key>=<value>;` per entry with nothing to tell a structural `=` or `;` from one inside a key or
  a value, so `{"a": "1;b=2"}` and `{"a": "1", "b": "2"}` both rendered `a=1;b=2;`. The string is an ingredient of the cache key, it names the render-, video-, animate-,
  inpaint-, upscale-, edit- and audio output files, and it is recorded as `params_key` on the persisted exemplar rows. Each key and each rendered value now has `%`, `;`
  and `=` written as `%25`, `%3B` and `%3D`, in one pass (`%` is the escape character, so it is escaped too; that is what keeps a value that spells `%3B` apart from the
  value `;`). Backslashes are left alone. **Every top-level entry is now unambiguous; nested and non-string values keep Go's `%v` rendering**, which is type-erased and
  space-separated, so `{"seed": "5"}` and `{"seed": 5}`, or a slice holding `"a b"` and a slice holding `"a"` and `"b"`, still render alike. That limit is documented and pinned by a
  test, because lifting it would give every params map with such a value a new file name. The escaping does not make output file names injective either: a stem is a hash of the
  prompt followed directly by this string, with no boundary between the two, so a prompt and a params string can still run together. That predates this change and is out of its
  scope; a stem collision only reuses or overwrites a file under a truncated 32-bit name. **The output is byte-identical to the old format for every params map whose keys and
  rendered values contain none of `%`, `;` and `=`** (prompts, numbers, bools, paths of either slash direction, nested maps and slices of those), so the output file names and
  recorded keys of ordinary params do not move, and neither do those of the image, edit and upscale params that carry Windows paths. A key or value that does contain one of the
  three, such as a negative prompt with a semicolon or an equals sign, a URL with a query string (`?a=b`), or a percent-encoded URL, renders differently from before: its default
  output file name changes once (an earlier file under the old name is not reused), and its `params_key` on newly written exemplar rows differs. Nothing reads `params_key` back for
  matching today.
- **Tests, red first.** Each new test was written before its fix and run against the old code, where it failed.
  - `internal/cache`: `TestKeySeparatorCollision` no longer skips. It asserts 24 named part lists give 24 distinct keys (the NUL cases, every empty-part case, a part that
    spells out a length header, a part that spells out the frames of another list), names the headline pairs as separate checks, and checks that its colliding pairs
    really did collide under the old `strings.Join(parts, "\x00")`, so the table cannot go stale. `TestKeyIsInjectiveOverSmallLists` checks all 2,380 lists of up to
    three short parts over `{a, NUL, \x01}`. `TestKeyEncodingIsPinned` holds four keys to values computed outside the package, so a change to the tag or the length width is
    a visible edit.
  - `internal/pipeline`: `TestRunVisionGenKeepsInputAndIdentityApart` drives `runVisionGen` itself with inputs whose old glued strings are equal; the second call must
    miss, the model must run twice, two entries must be stored, the answers must differ, and a genuine repeat of either must still hit.
    `TestTierKeyKeepsModelAndRenderApart`, `TestExemplarCacheTagKeepsFieldsApart` (tag and key level) and `TestTemplateCacheTagKeepsFieldsApart` (tag level, and key level for the case where
    the grammar agrees) cover the render and the two tags. `TestTranscribeCacheKeyKeepsFieldsApart` has three cases; two fail on the old key, and the third (model text imitating the digest
    boundary) passes on it because the old key repeated the model, which is what the case now holds to the framing.
  - `internal/tasks`: `TestStableParamsKeyKeepsEntriesApart` (three pairs, each checked against a copy of the old rendering), `TestStableParamsKeyEscapesWhatItMustEscape`
    and `TestStableParamsKeyIsInjectiveOverASmallAlphabet` (all 93,051 params maps of up to two entries over `{a, %, =, ;}`, strings up to two characters) fail on the old
    format. `TestStableParamsKeyIsInjectiveOverEscapeSpellings` covers what that alphabet cannot reach, a value that spells an escape: all 160,000 single-entry maps with
    keys and values of up to three characters over `{a, %, ;, =, 3, B, D}`. `TestStableParamsKeyOrdinaryParamsAreUnchanged` holds a table of ordinary maps, Windows and
    UNC paths and values with backslashes among them, to hard-coded expected strings written from the format and not computed by the function; it passes on the old
    format and the new, which is the promise that file names and recorded keys did not move. `TestStableParamsKeyNestedAndNonStringValuesKeepGoRendering` pins the documented
    limit above (three pairs that still render equal); it goes red if the nested rendering changes, so that change has to be deliberate.
  - `TestRunTier_NoCacheWrite` probed a hand-written five-part key while the key `Run` really uses has seven parts, so its "no entry under the default-model key" check could
    not fail whatever `RunTier` did. The probe now calls `cacheKeyFor` and the test checks that `Run`'s own entry is found under it. With `RunTier` made to write under `Run`'s
    key, the old probe stayed silent and the new one fails.
- **Mutations run.** With each of these put back in turn, the named tests fail and the code is restored byte for byte afterwards: the old NUL join in `Key`; `Key`
  without the version tag (only `TestKeyEncodingIsPinned` fails); an 8-byte length cut to 4 (only `TestKeyEncodingIsPinned`); the length missing on the last part; the glued
  vision key; the folded render; the NUL-terminated exemplar fields; the NUL-joined template tag; the glued transcription identity; the transcription key without the
  model; in `StableParamsKey`, the keys not escaped, the values not escaped, `%` not escaped, `=` not escaped, `;` not escaped, the trailing `;` changed, the backslash
  escaped as well, the escape applied twice, and the nested rendering changed to `%#v`. Removing `field()` from `askcache.Key` on its own no longer turns a test red, because `cache.Key` keeps those fields apart;
  its test says so.
- **Proved.** Remote Linux runs of this branch before its last review round: a gate over `./internal/cache`, `./internal/askcache`, `./internal/tasks`, `./internal/mcpserver`,
  `./internal/changelog`, `./internal/pipeline` and the root package passed (7 of 7), and `./internal/pipeline` passed again on a second remote Linux runner. On Windows,
  `go test -p 1 -count=1` passes on `./internal/cache/`, `./internal/askcache/` and `./internal/tasks/`; on `./internal/pipeline/` with `-run` over the key, cache,
  vision, tier, exemplar, template, transcribe, gate, media, image-batch, render, video and legacy-file-name tests; and on `./internal/mcpserver/` with `-run TestAsk`.
  `go vet` is clean on the four packages and `go run ./cmd/changelog check` passes. The other callers of `StableParamsKey` (the output-file stems in `internal/pipeline`, the cache keys
  and the exemplar append) were read, and the tests that reach them pass, among them the legacy file-name test in `imagebatch_family_test.go`. That test computes its expectation with
  `StableParamsKey` itself, so it cannot see a change in the output; the hard-coded table above is what holds the bytes.
- **Not verified here.** No live cache file, seat or fleet node was used, so the cold start after an upgrade is read from the code (a key that is not in the store is a miss) and not
  measured. A Windows run of the full suite was not made, and the leak-gate tool (it needs the operator's name list) was not run. The review round's own changes (the pinned-limit
  test, the `runtier_test.go` probe, the comments, the documentation paragraph and this entry) were run as targeted packages and `go run ./cmd/changelog check` here; the remote gate
  has not been repeated on them.
