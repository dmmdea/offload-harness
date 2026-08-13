# Patch: `internal/cli/keepset.go`

**Waves:** A (spine), D (final glue — `Example` + `pp:parent-group`)

## What was changed

- The generated TODO scaffold was replaced with the real `keepset` parent.
- `keepset status` was added as a subcommand, attached here rather than in
  `root.go`.
- Wave D added the `Example` block and the `pp:parent-group` annotation.

## Why

The keep-set is the structural protection standing between an `unload --all` and
a memory-stack outage, so it needs a first-class command surface. Attaching
`status` here keeps generated `root.go` untouched.

## What a regen must preserve

1. The whole body, plus the `addNovelCommandIfAbsent(cmd, newNovelKeepsetStatusCmd(flags))`
   and `...AuditCmd(flags)` lines.
2. **The binding source rule, stated in the Long text and enforced in
   `internal/mirror/keepset.go`: the keep-set is read from the llama-swap YAML
   and this CLI's config, NEVER from the server's `ttl` field.** GET /running
   reports `ttl:0` for a seat configured `ttl:-1` on current builds. A regen that
   "simplifies" this to read the API rebuilds the outage.
3. Membership matching must stay alias-aware.
