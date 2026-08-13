# Patch: `internal/cli/feedback.go`

**Wave:** E (live-dogfood repair — Class 3)

## What was changed

An `Example` block was added to `newFeedbackCmd`, matching the house style used
by the hand-built commands (`strings.Trim(` + "`...`" + `, "\n")` with commented
lines):

```
  # One line: what surprised you
  llamaswap-pp-cli feedback "fit interval was wrong on gemma-4-e4b"

  # Longer note piped in, so a run's own output becomes the report
  llamaswap-pp-cli logs triage --json | llamaswap-pp-cli feedback --stdin

  # Also POST it upstream (needs LLAMASWAP_FEEDBACK_ENDPOINT)
  llamaswap-pp-cli feedback --send "the swap thrash report came back empty on a box that thrashes"

  # Read back what has been recorded
  llamaswap-pp-cli feedback list --limit 5
```

`Annotations: {"pp:no-error-path-probe": "true"}` was added at the same time —
see below. Nothing else changed; `feedback list` already had its own `Example`.

## Why

`feedback --help` rendered no Examples section at all, which is the one thing
an agent reads before the flags — live dogfood 2026-08-13 failed it with
`missing Examples section`. The command's real surface is a **positional**
`[text]` argument plus `--stdin` and `--send`; there is no `--message` flag, so
the examples invoke it the way it actually works and cover each entry path
(positional, stdin, upstream POST, read-back).

## Why `pp:no-error-path-probe`

Adding the `Example` had a second-order effect: it gave the live-dogfood runner
a positional invocation to model, so the error-path probe stopped skipping and
started asserting a non-zero exit for
`llamaswap-pp-cli feedback __printing_press_invalid__`. `feedback` takes
**free-form text** — every positional is valid by construction, and the one
refusal it has (empty text) cannot be expressed as a positional. So the probe
was asserting a failure the command is right not to produce, and writing its
probe string into the feedback ledger to do it. `pp:no-error-path-probe` is the
generator's own sanctioned marker for this (the generated `tail` command
carries it for the same reason).

## What a regen must preserve

Both annotations, and the `Example` block, and the rule behind it: every long flag named in the
example must exist on `feedback`, on its subcommands, or on the root's
persistent flags. `TestFeedbackHasExamples` (`internal/cli/rawtext_test.go`)
asserts both — it parses every `--flag` token out of the example and fails if
one is not a real flag, so an example cannot drift into something that cannot
be pasted and run.
