# Vendored llama-swap config schema

| | |
|---|---|
| **File** | `config-schema.json` |
| **Upstream** | <https://raw.githubusercontent.com/mostlygeek/llama-swap/main/config-schema.json> |
| **Retrieved** | 2026-08-13 |
| **Against llama-swap** | **v249** (`/api/version` on the reference deployment reported `{"version":"v249","commit":"f94c94a","build_date":"2026-08-10T06:22:31Z"}` on the retrieval date) |
| **Draft** | JSON Schema draft-07 (`$schema: https://json-schema.org/draft-07/schema#`) |
| **sha256** | `85c0101dbc8a4461bd4c751bc98b3441651a83a8ba855d0473ef6c8f4c2c5666` |
| **Bytes** | 40192 |

## Why it is vendored

`config validate` must work on a box with no network and against a config file
that is not the live one. Fetching the schema per invocation would make
validation a network-dependent operation and would silently change behavior
when upstream edits `main`. The file is embedded with `go:embed` (see
`../schema.go`) so a given CLI build always validates against a known schema.

## Downloaded once, on purpose

This copy was fetched **once**, by hand, and committed. There is no auto-update
path and there should not be one: a schema that changes under the operator is
indistinguishable from a config that broke.

## Refreshing it

When llama-swap ships config keys this copy does not know about, refresh
deliberately:

```bash
curl -fsSL https://raw.githubusercontent.com/mostlygeek/llama-swap/main/config-schema.json \
  -o internal/lsconfig/schema/config-schema.json
sha256sum internal/lsconfig/schema/config-schema.json   # update the table above
```

Then update the **Retrieved** date, the **Against llama-swap** version (read it
from `/api/version` on a deployment running the new build), the sha256, and the
byte count, and re-run `go test ./internal/lsconfig/... -count=1` — the schema
tests assert the embedded document parses and that the known-good reference
config still validates.

## Known limitation this package compensates for

The upstream schema does **not** set `additionalProperties: false` at the top
level, so an unknown or misspelled top-level key (`macro:` for `macros:`,
`startport:` for `startPort:`) validates clean and is then silently ignored by
llama-swap at boot. `config validate` therefore layers its own
unknown-top-level-key check with nearest-key suggestions on top of schema
validation — see `../validate.go`.
