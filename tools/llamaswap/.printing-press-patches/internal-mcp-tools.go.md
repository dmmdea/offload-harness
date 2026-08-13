# Patch: `internal/mcp/tools.go`

**Wave:** LS-1 (perfection wave — MCP outputSchema, backlog row 4)

## What was changed

One line appended to the end of `RegisterTools`, after the existing
`cobratree.RegisterAll(...)`:

```go
registerOutputSchemas(s)
```

## Why

Every tool this server exposed advertised its result as TEXT. An agent
receiving `{"schema_version":1,"header":{...}}` had to discover that shape by
calling the tool and reading what came back, with no way to know whether a
field was optional, an integer, or renamed last week. MCP has carried
`outputSchema` on the tool definition and `structuredContent` on the result
since the 2025-06-18 revision, and mcp-go v0.57 supports both; neither appeared
anywhere in this tree.

`registerOutputSchemas` (hand-authored, `internal/mcp/output_schemas.go`) reads
each already-registered tool back, attaches the schema, wraps the handler to
publish `structuredContent`, and re-adds it. Decorating after the fact — rather
than threading a schema option through every registration — keeps the tool
definitions in exactly one place and means a reprint that adds a tool gets
decorated automatically if it has a typed envelope.

It must run LAST so that everything (typed endpoint tools, the registry tools,
and the runtime Cobra-tree mirror) is already registered.

## What a regen must preserve

1. The `registerOutputSchemas(s)` call, positioned after every other
   registration in `RegisterTools`. Guarded by
   `TestRegisterOutputSchemasAttachesSchemaAndStructuredContent`.
2. The schemas themselves are reflected from Go structs and committed under
   `testdata/schema/`; `TestOutputSchemaGoldenFilesAreCurrent` fails on any
   unreviewed envelope change, so a regen that alters a result struct is
   caught rather than silently republished.
