# Vendored llama-swap config schema

| | |
|---|---|
| **File** | `config-schema.json` |
| **Upstream** | <https://raw.githubusercontent.com/mostlygeek/llama-swap/main/config-schema.json> |
| **Retrieved / carries** | recorded in `snapshot.json` beside it (retrieval date, and the newest llama-swap release at that moment) |
| **Draft** | JSON Schema draft-07 (`$schema: https://json-schema.org/draft-07/schema#`) |

## Why it is vendored

`config validate` must work on a box with no network and against a config file
that is not the live one. Fetching the schema per invocation would make
validation a network-dependent operation and would silently change behavior
when upstream edits `main`. The file is embedded with `go:embed` (see
`../schema.go`) so a given CLI build always validates against a known schema.

## A refreshable snapshot

The embedded copy is a snapshot of upstream, not a frozen document. Nothing
refreshes it behind the operator's back (a schema that changes under a running
CLI is indistinguishable from a config that broke); it is refreshed on purpose,
and the refresh is one command from the repository root:

```bash
bash scripts/refresh-llamaswap-schema.sh
```

The script fetches the newest `config-schema.json` from the default branch of the
llama-swap repository, records the retrieval date and the newest release tag in
`snapshot.json`, and prints the version the snapshot now carries. Then re-run
`cd tools/llamaswap && go test ./internal/lsconfig/... ./internal/cli/ -count=1`:
the schema tests assert the embedded document parses and that the known-good
reference config still validates.

`version drift` reads the snapshot version as a **floor**: only a live server
OLDER than the snapshot is drift; a newer server is fine.

## Known limitation this package compensates for

The upstream schema does **not** set `additionalProperties: false` at the top
level, so an unknown or misspelled top-level key (`macro:` for `macros:`,
`startport:` for `startPort:`) validates clean and is then silently ignored by
llama-swap at boot. `config validate` therefore layers its own
unknown-top-level-key check with nearest-key suggestions on top of schema
validation — see `../validate.go`.
