### Fixed - the result-cache key can no longer be forged from request text: every part is length-prefixed, callers pass each ingredient as its own part, the params part is typed, and every existing cache key changes once

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
  `docs/systems/offload-pipeline.md`, under "How a result-cache entry is keyed". `Cache.Put` refuses an empty key (`ErrEmptyKey`): no real key is empty, and the
  pipeline uses `""` for "this call has no key and must not be cached".
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
    This seam was not reachable end to end: the old key also carried the model as a part of its own, and the digest is a hash of the file. It is hardening.
  - `askcache.Key` keeps its own per-field length prefix as a second layer, so its keys do not rest on `cache.Key` alone. Its comments changed (and the comment in its
    test that said `cache.Key` joins with a NUL), and its test gained one assertion: `Key("ab", "c")` against `Key("a", "bc")`.
  - One glued part is left on purpose: the video media identity (`vid:<digest>|fps=...|n=...|w=...|frames=...`, and the per-window `vidw:` form) is built from the file
    digest and numbers and passed as one part. None of it is caller text, so it cannot forge a boundary; the documentation names it as the exception to "every ingredient
    is its own part".
- **The params part is typed, and params that cannot be keyed skip the cache (`internal/tasks`, `internal/pipeline`).** The result-cache key carried the params as
  `StableParamsKey` printed them, and that prints nested and non-string values with Go's `%v`, which is type-erased and space-separated. Two extract requests whose
  schemas differ only where `%v` cannot see it, a `required` list naming `"a b"` instead of `"a"` and `"b"`, or a `minimum` that is the string `"1"` instead of the
  number 1, had one key, so the second was served the first's cached answer before its own schema was ever checked (a cached reply is validated only on a miss). The
  key now carries `tasks.CacheParamsKey`: the params as JSON with the keys of every map sorted at every depth, typed, so `"5"` and 5, `["a","b"]` and `["a b"]`, a string
  and the bool it spells all differ, while an int 5 and the float64 5 the MCP door decodes, or a `[]string` and a `[]any` holding the same strings, still agree. It is used
  for the result cache only: Run, RunTier, the vision path and the transcription key. **It fails closed.** Params with no faithful typed form (NaN or an infinity, a
  function or channel, a struct, a byte slice, a type with its own JSON form, a string that is not valid UTF-8, a self-referencing value) make that call neither read nor
  write the result cache: it runs the model every time, logs one line, and records `cache_bypass` ("params: ...") on its ledger row. There is no fallback to a `%v`
  rendering or to `StableParamsKey`, because that would hand the call a key a different request can reach.
- **The params string for file names (`internal/tasks`).** `StableParamsKey` renders `<key>=<value>;` per entry with nothing to tell a structural `=` or `;` from one
  inside a key or a value, so `{"a": "1;b=2"}` and `{"a": "1", "b": "2"}` both rendered `a=1;b=2;`. It names the render-, video-, animate-, inpaint-, upscale-, edit- and
  audio output files and is recorded as `params_key` on the persisted exemplar rows, so it stays, with the escaping. Each key and each rendered value now has `%`, `;`
  and `=` written as `%25`, `%3B` and `%3D`, in one pass (`%` is the escape character, so it is escaped too; that is what keeps a value that spells `%3B` apart from the
  value `;`). Backslashes are left alone. **Every top-level entry is now unambiguous; nested and non-string values keep Go's `%v` rendering**, so `{"seed": "5"}` and
  `{"seed": 5}`, or a slice holding `"a b"` and a slice holding `"a"` and `"b"`, still render alike. That limit is documented and pinned by a test, because lifting it would
  give every params map with such a value a new file name; it now matters only for file stems and exemplar rows, since the result cache no longer uses this string. The
  escaping does not make output file names injective either: a stem is a hash of the prompt followed directly by this string, with no boundary between the two, so a prompt
  and a params string can still run together. That predates this change; a stem collision only reuses or overwrites a file under a truncated 32-bit name.
  **The output is byte-identical to the old format for every params map whose keys and rendered values contain none of `%`, `;` and `=`** (prompts, numbers, bools, paths
  of either slash direction, nested maps and slices of those), so the output file names and recorded keys of ordinary params do not move, and neither do those of the
  image, edit and upscale params that carry Windows paths. A key or value that does contain one of the three, such as a negative prompt with a semicolon or an equals sign,
  a URL with a query string (`?a=b`), or a percent-encoded URL, renders differently from before: its default output file name changes once (an earlier file under the old
  name is not reused), and its `params_key` on newly written exemplar rows differs. Nothing reads `params_key` back for matching today.
- **Tests, red first.** A test that pins a defect was written before its fix and failed on the old code; a test that pins behaviour the code already has (the
  ordinary-params table, the documented limit, the encoding values, the digit-part entries, the empty-key refusal) is shown red by a mutation instead, listed below.
  - `internal/cache`: `TestKeySeparatorCollision` no longer skips. It asserts 26 named part lists give 26 distinct keys (the NUL cases, every empty-part case, a part that
    spells out a length header, a part that spells out the frames of another list, and two digit lists that a decimal length with no delimiter would read as one), names the
    headline pairs as separate checks, and checks that its colliding pairs really did collide under the old `strings.Join(parts, "\x00")`, so the table cannot go stale.
    `TestKeyIsInjectiveOverSmallLists` checks all 2,380 lists of up to three SHORT parts over `{a, NUL, \x01}` (what it cannot reach, long digit parts, is the named digit
    entries above). `TestKeyEncodingIsPinned` holds four keys to values computed outside the package, so a change to the tag or the length width is a visible edit.
    `TestEmptyKeyNamesNoEntry` holds the empty-key refusal.
  - `internal/pipeline`: `TestRunVisionGenKeepsInputAndIdentityApart` drives `runVisionGen` itself with inputs whose old glued strings are equal; the second call must
    miss, the model must run twice, two entries must be stored, the answers must differ, and a genuine repeat of either must still hit.
    `TestTierKeyKeepsModelAndRenderApart`, `TestExemplarCacheTagKeepsFieldsApart` (tag and key level) and `TestTemplateCacheTagKeepsFieldsApart` (tag level, and key level for the case where
    the grammar agrees) cover the render and the two tags. `TestTranscribeCacheKeyKeepsFieldsApart` has three cases; two fail on the old key, and the third (model text imitating the digest
    boundary) passes on it because the old key repeated the model, which is what the case now holds to the framing. `TestRunTranscribeStoresUnderTranscribeCacheKey` drives
    `runTranscribe` and ties the entry it stores to `transcribeCacheKey`; that is what makes the call site a pinned seam (the function's own test cannot see which key the call
    site builds: with the glued key put back at the call site every other transcription test stayed green).
  - Typed params, driven through `Run` (`internal/pipeline`): `TestRunKeepsTypeErasedParamsApart` stores one request and asks the other for both release-review pairs
    (`required` and `minimum`); the asking request must miss and the model must run again. It fails on the old code with "A was served B's cached answer". The
    fail-closed behaviour is pinned the same way for `Run` (NaN, an infinity, a function), `RunTier`, the vision path and the transcribe path: two identical calls both run the model,
    no entry is stored, and `cache_bypass` says why; the same request with keyable params is cached and hit, so the tests are not vacuous. They fail on the old code, where the
    second call is a cache hit.
  - `internal/tasks`: `TestCacheParamsKeyKeepsTypesAndStructureApart` (eight pairs, each checked against the old `%v` rendering first),
    `TestCacheParamsKeyIsStableAcrossRepresentations`, `TestCacheParamsKeyIsPinned` (exact strings) and `TestCacheParamsKeyFailsClosed` (twenty unkeyable values, each an error that
    names a kind and never a value). For `StableParamsKey`: `TestStableParamsKeyKeepsEntriesApart` (three pairs, each checked against a copy of the old rendering),
    `TestStableParamsKeyEscapesWhatItMustEscape` and `TestStableParamsKeyIsInjectiveOverASmallAlphabet` (all 93,051 params maps of up to two entries over `{a, %, =, ;}`, strings
    up to two characters) fail on the old format. `TestStableParamsKeyIsInjectiveOverEscapeSpellings` covers what that alphabet cannot reach, a value that spells an escape: all
    160,000 single-entry maps with keys and values of up to three characters over `{a, %, ;, =, 3, B, D}`. `TestStableParamsKeyOrdinaryParamsAreUnchanged` holds a table of
    ordinary maps, Windows and UNC paths and values with backslashes among them, to hard-coded expected strings written from the format and not computed by the function; it
    passes on the old format and the new, which is the promise that file names and recorded keys did not move. `TestStableParamsKeyNestedAndNonStringValuesKeepGoRendering`
    pins the documented limit above (three pairs that still render equal); it goes red if the nested rendering changes, so that change has to be deliberate.
  - `TestRunTier_NoCacheWrite` probed a hand-written five-part key while the key `Run` really uses has seven parts, so its "no entry under the default-model key" check could
    not fail whatever `RunTier` did. The probe now calls `cacheKeyFor` and the test checks that `Run`'s own entry is found under it. With `RunTier` made to write under `Run`'s
    key, the old probe stayed silent and the new one fails.
- **Mutations run.** With each of these put back in turn, the named tests fail and the code is restored byte for byte afterwards. `cache.Key`: the old NUL join; no version tag
  (only `TestKeyEncodingIsPinned`); an 8-byte length cut to 4 (only `TestKeyEncodingIsPinned`); the length missing on the last part; a decimal length with no delimiter
  (`TestKeySeparatorCollision` and `TestKeyEncodingIsPinned`; before the digit entries only the pinned test failed). `Cache.Put` without the empty-key refusal. Call sites:
  the glued vision key; the folded render; the NUL-terminated exemplar fields; the NUL-joined template tag; the glued transcription identity, inside `transcribeCacheKey` and
  at the `runTranscribe` call site (the latter fails only `TestRunTranscribeStoresUnderTranscribeCacheKey`); the transcription key without the model; `StableParamsKey` back in
  the key at the `Run`, vision, `RunTier` and transcribe call sites. Fail-closed handling: a fallback from an unkeyable value to `StableParamsKey`; the bypass not recorded;
  the unkeyable flag ignored at the `Run`, `RunTier`, vision and transcribe call sites. `CacheParamsKey`: it returning `StableParamsKey`; no walk over the values; byte slices,
  non-UTF-8 strings or types with their own JSON form allowed; a `fmt` fallback for an unkeyable value (it crashes on a self-referencing map, which is the danger of a
  fallback). `StableParamsKey`: the keys not escaped, the values not escaped, `%` not escaped, `=` not escaped, `;` not escaped, the trailing `;` changed, the backslash
  escaped as well, the escape applied twice, and the nested rendering changed to `%#v`. Removing `field()` from `askcache.Key` on its own no longer turns a test red, because
  `cache.Key` keeps those fields apart; its test says so.
- **Not fixed here (pre-existing, out of scope).** D-136: a cached result is not validated against the requesting call's schema on a hit (validation runs only on a miss), so
  a key collision of any kind would still be served; the typed key closes the known ones. D-137: the result cache has no bound, TTL, compaction or operator lever. D-138:
  output-file stems that glue fields (`composevideo.go:343` hashes a `fmt.Sprint` of its fields with no separator at all; the render, video and edit stems hash the prompt
  and the params string with no boundary). Left alone on purpose, no row: `promptPrefixFingerprint` (`internal/pipeline/identity.go`) and the research retry-cap key
  (`pageKeyFor`, `internal/delegate/processgate.go`) join their fields with a NUL, but they are telemetry and a retry cap, not result-cache keys.
- **Proved.** Remote Linux runs at commit `dca2e015` (the typed-params round; `babdd239` before it passed the same): a gate over `./internal/cache`, `./internal/askcache`, `./internal/tasks`, `./internal/mcpserver`,
  `./internal/changelog`, `./internal/pipeline` and the root package passed (7 of 7), and `./internal/pipeline` passed again on a second remote Linux runner. On Windows, for the
  typed-params round, `go test -p 1 -count=1` passes on `./internal/cache/`, `./internal/askcache/`, `./internal/tasks/` and `./internal/mcpserver/` with `-run TestAsk`, and on
  `./internal/pipeline/` with `-run` over the key, cache, vision, tier, exemplar, template, transcribe, gate, media, image-batch, render, video and legacy-file-name tests
  (skipping one lease-timing test, see below); `go vet` is clean on the touched packages and `go run ./cmd/changelog check` passes. The other callers of `StableParamsKey` (the output-file stems in `internal/pipeline` and
  the exemplar append) were read, and the tests that reach them pass, among them the legacy file-name test in `imagebatch_family_test.go`. That test computes its expectation with
  `StableParamsKey` itself, so it cannot see a change in the output; the hard-coded table above is what holds the bytes.
- **Not verified here.** No live cache file, seat or fleet node was used, so the cold start after an upgrade is read from the code (a key that is not in the store is a miss) and not
  measured. The Windows pipeline run skips `TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere`: a lease-window timing test (it spawns real runner processes) that failed
  intermittently under load on this machine, always as "want one live lease while the render runs" within about 50 ms and on a different subtest each time, and passed three runs in a
  row on its own; it has no cache code in its path, and the remote Linux runs of the whole package included it. A Windows run of the full suite was not made, and the leak-gate tool (it needs the operator's name list) was not run.
