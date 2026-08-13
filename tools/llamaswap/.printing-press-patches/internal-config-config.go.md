# Patch: `internal/config/config.go`

**Wave:** B (config surface) — a one-line change to a generated file

## What was changed

The default `BaseURL` in `Load()` is the IP literal:

```go
BaseURL: "http://127.0.0.1:11436",
```

One line, plus the comment above it explaining why.

**A regen WILL revert this.** `spec.yaml:26` declares
`base_url: http://localhost:11436`, so the generator emits the loopback
HOSTNAME every time. The durable fix is at the spec level — change that line to
`http://127.0.0.1:11436` — and this patch is the guard until someone does.

## Why

On a dual-stack Windows host the loopback hostname resolves `::1` first. The
llama-swap listener is IPv4-only, so every call eats the full IPv6 connect
timeout — roughly 21 seconds per request — before falling back. That stall has
been misread as "the server is down" more than once.

## What a regen must preserve

1. **The `127.0.0.1` literal.** Not `localhost`, not `[::1]`, not a hostname of
   any kind. This is the single most load-bearing one-line patch in the tree:
   without it every command in every wave is 21s slower on a cold connection.
2. The comment. The next person to "clean up" a hardcoded IP needs to read why
   it is there before deleting it.
3. `LLAMASWAP_BASE_URL` must remain the LAST override applied, after the config
   file. The global `--host` flag (`internal/cli/remotes.go`) is wired through
   that variable, so re-ordering the overrides silently disables named remotes.

## Related

Downstream helpers apply the same normalization defensively for a
config file that spells the host differently: `loopbackBaseURL`
(`config_root.go`), `spineBaseURL` (`sync_mirror.go`), `mcNormalizeHost`
(`measure_common.go`), and `normalizeLoopback` (`pkg/llamaswap/client.go`).
All four are resolution-based or literal-based and deliberately leave a
NON-loopback hostname — a MagicDNS name for a remote rig — untouched.
