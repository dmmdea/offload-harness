# Patch: `internal/cli/which.go`

**Wave:** D (analytics verbs)

## What was changed

Two entries appended to `whichIndex`, after the `swaps` entry:

```go
{Command: "residency", Description: "Reconstruct each seat's load/evict timeline from request gaps, and cost a different idle-TTL with keep-set and eviction-group safety.", Group: "Local state that compounds", WhyItMatters: "Answers 'what would a longer/shorter TTL on this seat save or cost' with measured numbers before you edit the YAML."},
{Command: "saturation", Description: "Per-seat error and load pressure: 429/5xx counts and rates, request volume, and the hourly load curve.", Group: "Local state that compounds", WhyItMatters: "Reach for it during or after a 429 storm to see which seats are rejecting and when the load actually lands."},
```

## Why

`which` is the capability router agents use to find a verb by intent. The two
new wave-D verbs must be discoverable there, or `which "ttl what-if"` /
`which "429"` would not surface them.

## What a regen must preserve

The two entries. The file is otherwise generated; a regen re-emits the base
five-entry index (`keepset audit`, `seat log`, `sync`, `swaps`, `verify`) and
drops these two. `TestWhichIndex_ExistsAndIsWellFormed` checks index→tree
resolution, so a regen that drops them still passes — the guard is this note,
not a test. Re-add both after the `swaps` entry.
