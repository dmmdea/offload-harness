// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

// MCP output schemas for the code-orchestration trio.
//
// A tool that declares `outputSchema` must return `structuredContent` that
// conforms to it, so a schema is only worth declaring where this CLI controls
// the payload end to end. That is true of comfyui_search and comfyui_get,
// whose payloads are built entirely from the local endpoint registry, and of
// comfyui_execute, whose envelope is ours even though the `body` it carries is
// whatever the ComfyUI server returned — hence `body` is deliberately
// unconstrained rather than pinned to a shape ComfyUI does not promise.
//
// The committed copies under testdata/schema/ are the fixture these constants
// are checked against (see output_schema_test.go), so renaming a property here
// without renaming it there fails the build rather than silently shipping a
// schema that no longer describes the payload.
//
// Not covered: the Cobra command-mirror tools. cobratree's shell-out handler
// returns the companion CLI's stdout as free text whose shape depends on the
// caller's own --json/--agent flags, and it emits no structuredContent at all,
// so declaring an outputSchema for those tools would advertise a contract the
// handler cannot keep.

package mcp

// codeOrchSearchOutputSchema describes comfyui_search's ranked endpoint list.
// `score` is the naive keyword score handleCodeOrchSearch computes; results are
// already sorted by it, descending.
const codeOrchSearchOutputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "count",
    "results"
  ],
  "properties": {
    "count": {
      "type": "integer",
      "description": "Number of endpoints in results, after the limit cut."
    },
    "results": {
      "type": "array",
      "description": "Matching endpoints, best match first.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": [
          "endpoint_id",
          "method",
          "path",
          "summary",
          "score"
        ],
        "properties": {
          "endpoint_id": {
            "type": "string",
            "description": "Dotted identifier to pass to comfyui_get or comfyui_execute, e.g. \"history.get\"."
          },
          "method": {
            "type": "string",
            "description": "HTTP method the endpoint is invoked with."
          },
          "path": {
            "type": "string",
            "description": "Request path template; {name} segments are filled from the params object."
          },
          "summary": {
            "type": "string",
            "description": "What the endpoint returns and the traps it carries."
          },
          "score": {
            "type": "integer",
            "description": "Keyword match score; higher sorts first."
          }
        }
      }
    }
  }
}`

// codeOrchGetOutputSchema describes comfyui_get's single-endpoint metadata.
// It is the same record comfyui_search emits, minus the search-only score.
const codeOrchGetOutputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "endpoint_id",
    "method",
    "path",
    "summary"
  ],
  "properties": {
    "endpoint_id": {
      "type": "string",
      "description": "Dotted identifier of the endpoint that was looked up."
    },
    "method": {
      "type": "string",
      "description": "HTTP method; comfyui_get only resolves GET endpoints."
    },
    "path": {
      "type": "string",
      "description": "Request path template; {name} segments are filled from the params object."
    },
    "summary": {
      "type": "string",
      "description": "What the endpoint returns and the traps it carries."
    }
  }
}`

// codeOrchExecuteOutputSchema describes comfyui_execute's response envelope.
// `path` is the request actually issued, after placeholder substitution and
// query assembly, so a caller can tell a 404 caused by a wrong id from one
// caused by an unsubstituted placeholder. Exactly one of body, body_text, or
// body_omitted is present.
const codeOrchExecuteOutputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "endpoint_id",
    "method",
    "path",
    "body_bytes"
  ],
  "properties": {
    "endpoint_id": {
      "type": "string",
      "description": "Dotted identifier of the endpoint that was executed."
    },
    "method": {
      "type": "string",
      "description": "HTTP method used for the call."
    },
    "path": {
      "type": "string",
      "description": "Path actually requested, after path-placeholder substitution and query-string assembly."
    },
    "body_bytes": {
      "type": "integer",
      "description": "Size in bytes of the bounded response body carried by the text content."
    },
    "body": {
      "description": "Response body parsed as JSON. Unconstrained: the shape is whatever the ComfyUI endpoint returned. Absent when the body was not JSON or was omitted."
    },
    "body_text": {
      "type": "string",
      "description": "Response body verbatim when it did not parse as JSON — ComfyUI's /view returns raw file bytes. Absent when body or body_omitted is present."
    },
    "body_omitted": {
      "type": "boolean",
      "description": "True when the body was left out of structuredContent because it exceeds the inline mirror budget; read it from the text content instead."
    }
  }
}`
