# Provenance of this copy

This is the **sanitized public snapshot** of the operator's DaVinci Resolve reference library:
Studio 21.1 driven on a Windows editing rig (RTX 3070 class) and a Windows workstation, measured
2026-08-31 → 2026-09-10 (146 sidecar commands verified live on Studio 21.1.0.14). Machine names,
user accounts, tailnet addresses and repository paths were replaced with placeholders
(`<user>`, `<editor>`, `<dev>`, `<tailnet-ip>`, `the editing rig`, `the workstation`); every
number and every verified/unverified verdict is unchanged. The live capability dump
(`live-dump-2026-09-01.json`) keeps its measured values with the same placeholders.

The unredacted library is the operator's `~/.claude/skills/davinci-resolve`; this copy is refreshed
from it by the scrub script (private evidence repo `tools/scrub-tool-skill-for-public.py`) and
gated by `identity_lint_test.go` in this repository.
