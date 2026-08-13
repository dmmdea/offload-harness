# Patch: `internal/cli/upstream_props.go`

**Wave:** E (live-dogfood repair — Class 2, live fixture)

## What was changed

Two lines in the command constructor:

- `pp:happy-args: "--model;embeddinggemma"` was added to the `Annotations` map
  (merged alongside the existing `pp:endpoint` / `pp:method` / `pp:path` /
  `mcp:read-only` entries).
- The `Example` placeholder `--model example-value` was replaced with
  `--model embeddinggemma`, which removed the generated
  `// TODO: replace placeholder example values before relying on this for live dogfood.`
  comment above it.

Nothing else in the file changed: /upstream/{model}/props returns JSON, so the generated read path
is correct here.

## Why

The generator had no real value for the `model` path parameter and emitted the
literal string `example-value`. Live dogfood 2026-08-13 therefore ran
`upstream props --model example-value`, which llama-swap correctly refused with
HTTP 404 `{"error":"model not found"}` and exit 3. The command was right; the
fixture was fake. The dogfood runner honors `pp:happy-args` (semicolon-separated
tokens) as the live invocation, so naming a model that exists on this
deployment is the whole fix.

`embeddinggemma` specifically: it is the always-resident mem0 embedder
(`ttl: -1`, keep-set member), the call is read-only, and a resident seat can
never be auto-started or evicted by being read. Seat properties (build_info, model_path, chat_template, total_slots). Live-verified 2026-08-13: embeddinggemma answers 200. NOTE: this command's --model help text says "auto-starts the model if idle" — embeddinggemma is a keep-set resident (ttl: -1), so the fixture can never trigger that auto-start.

## What a regen must preserve

1. `pp:happy-args: "--model;embeddinggemma"`. Without it the generator's
   `example-value` fixture returns and the live dogfood fails again.
2. The placeholder-free `Example`.

Both are guarded by `TestUpstreamListCommandsCarryLiveHappyArgs`
(`internal/cli/rawtext_test.go`), which asserts the annotation across all five
upstream read commands and fails on any surviving `example-value`.
