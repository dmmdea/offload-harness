### Fixed - a manifest model is checked against what its publisher publishes, and a download never leaves a partial file

- **The gap.** A run-graph manifest's `models[].sha256` was supplied by the caller. It pinned one build and authenticated
  nothing against the publisher, a present file with no hash was trusted as it lay, and a download was streamed straight onto
  its final path with no size check, so an interrupted download left a truncated file that every later run trusted.
- **The publisher's digest.** `render/manifest-satisfy.mjs` reads it at run time (`publisherDigest`): for a Hugging Face
  `resolve` URL the `X-Linked-ETag` and `X-Linked-Size` of an unfollowed HEAD, else the tree API's `lfs.oid` and size; for a
  GitHub release asset the release API's `digest` and size. A Hugging Face file outside LFS carries a size only; any other
  host publishes nothing the node reads. What cannot be checked against a publisher digest is listed in `unverified_models`.
- **Downloads.** A model downloads to `<file>.part`, is checked for size and sha256, and is renamed onto the destination
  only then. A mismatch deletes the `.part`, retries once, and then defers `MODEL_SHA_MISMATCH` naming both digests. A
  download that ends short of the server's stated length fails instead of being kept. A missing model whose digest could not be
  read (publisher unreachable, no caller assertion) defers `MODEL_DOWNLOAD_FAILED` (retryable) rather than fetching a file
  nothing can check.
- **Files already on disk.** `<file>.sha-ok` is now JSON `{oid, size, mtime}`. A file whose size and modification time are the
  recorded ones and whose oid is the publisher's is kept without being read; a changed size or time, or a publisher that moved
  to a newer build, hashes it once and replaces it when it differs. The replacement lands where the file was found (an extra
  model root stays on its own drive), and the previous file is kept as `<file>.prev`, one generation, never deleted. A record in the older format (a bare hash) reads as none,
  so each such file is hashed once more. With the publisher unreachable a file with a fitting record is kept and the result
  carries a warning; one with no record is listed unverified and never deleted.
- **The caller's `sha256`** is an optional assertion: when it differs from a published digest the entry defers naming both,
  and it never overrides the publisher. `manifestHash` now keys a model on `path` and `source_url` only, so the first request
  after the upgrade provisions (and so verifies) every manifest once.
- **Docs.** The comments in `render/node-packs.mjs` and `internal/mediacap/manifestmodels.go`, the README, the operator guide,
  `docs/systems/mcp-server.md` and `docs/flows/run-graph-manifest-satisfaction.md` say the same.
