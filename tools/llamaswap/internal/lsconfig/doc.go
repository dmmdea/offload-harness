// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

// Package lsconfig is READ-ONLY config intelligence for a llama-swap YAML.
//
// THE TRUST CONTRACT (binding, not aspirational):
//
//	This package NEVER writes, rewrites, re-marshals, formats, or otherwise
//	mutates a llama-swap configuration file. Not with a backup first, not
//	"atomically", not behind a --force flag. The live config on an operating
//	box is 60%+ comments, and those comments are the operator's decision
//	journal: which flags are load-bearing, which were tried and reverted, and
//	why. Two production memory-stack outages on this class of deployment were
//	caused by programs writing that file from a stale in-memory model and
//	silently dropping load-bearing flags. yaml.Marshal of a parsed config is
//	exactly that failure mode with better manners.
//
//	Parsing therefore goes through yaml.v3's Node API, which preserves
//	comments, key order, and line positions, and every rendering path in this
//	package emits either (a) bytes copied verbatim from the source file or
//	(b) newly computed report text. There is no Marshal path and no io.Writer
//	that points at an input file.
//
//	The only filesystem writes anywhere in this feature area are NEW
//	content-addressed backup files plus their sidecar index, created by
//	`config backup` alongside the source. Those never overwrite an existing
//	path (a colliding sha means the content is already archived).
//
// What the package does provide:
//
//   - Load: Node-based parse retaining per-model comment blocks and exact
//     source line spans (parse.go).
//   - Expander: macro expansion — ${name} from macros:, ${env.VAR} from the
//     environment, ${PID}/${PORT}/${MODEL_ID} deliberately left symbolic —
//     with cycle detection (expand.go).
//   - ClassifySeat / ParseCmd: the whisper-server escape hatch. A seat whose
//     binary is not llama-server is SeatNonLlamaServer, and every
//     llama-server-specific check downstream skips it with an explicit note
//     rather than emitting a false positive (classify.go).
//   - DiscoverCorpus: recursive, extension-tolerant discovery of historical
//     config copies, content-hashed. Filenames are LABELS ONLY; mtime and
//     the content hash are the truth (corpus.go).
//   - Validate: draft-07 validation against the vendored upstream schema plus
//     nearest-key suggestions for unknown top-level keys (validate.go).
//   - Lint: the semantic check catalog (lint.go).
//   - Diff: per-model, per-flag semantic diff between two configs (diff.go).
package lsconfig
