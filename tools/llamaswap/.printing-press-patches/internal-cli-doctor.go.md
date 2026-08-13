# Patch: `internal/cli/doctor.go`

**Wave:** D (final glue) — two additive call sites in a generated file

## What was changed

Two lines, each delegating to `internal/cli/doctor_extras.go`:

1. `runDoctorExtras(cmd.Context(), flags, report)` — inserted immediately after
   `report["version"] = version` and BEFORE the `if flags.asJSON` branch, so the
   findings are in the report for both output paths.
2. `renderDoctorExtras(w, report)` — inserted after the `renderPathsReport`
   block, immediately before `return doctorExitForFailOn(failOn, report)`, so
   the human rendering lands under the generated sections.

No generated logic was modified. Every check lives in `doctor_extras.go`.

## Why

The generated doctor answers framework questions (config path, reachability,
cache freshness). It cannot know to ask deployment questions: is the proxy bound
to every interface with no API keys, is upstream persistence on, is fragment
mode enabled, is the memory stack actually answering, does this build support
the commands that need a minimum version.

## What a regen must preserve

1. **Both call sites, at those positions.** `runDoctorExtras` before the
   `flags.asJSON` branch (otherwise JSON output loses the findings);
   `renderDoctorExtras` after `renderPathsReport` (otherwise the block prints
   above the sections it belongs under).
2. The section shape. `runDoctorExtras` files
   `report["llamaswap"] = map[string]any{"status": ..., "findings": ...}`
   specifically because the generated `doctorExitForFailOn` inspects map values
   for a `"status"` key — that is what makes `doctor --fail-on warn` trip on the
   LAN-open finding without the generated gate knowing this package exists.
   `internal/cli/glue_test.go` asserts exactly this coupling.
3. Adding a check requires NO further edit here: append to `doctorExtraChecks`
   in `doctor_extras.go`.
