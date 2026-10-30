### Fixed - the validator's no-schema path now requires a JSON object (issue #5)

- **What changed.** `validator.Validate` with a nil or empty schema unmarshals into `map[string]any` and rejects `null`
  with a "not a JSON object" error, so `123`, `"str"`, `[1,2,3]` and `null` no longer pass; a plain object still does.
- **Why.** The doc comment said it confirmed a JSON object but it unmarshalled into `any`, so any valid JSON passed (#5).
  Every caller in `internal/pipeline` turns a validation error into a retry or defer verdict, never a crash.
- **Check.** `go test ./internal/validator/ ./internal/pipeline/`; the test that pinned the lax behaviour now asserts
  scalars, arrays and null are rejected and an object passes.

### Removed - the ledger summary's deprecated `est_dollar_saved` alias (issue #7)

- **What changed.** `Summary.EstDollarSaved` and its `est_dollar_saved` JSON key are gone; `est_value_kept_local` carries
  the same number and is unchanged.
- **Why.** The alias was marked "remove in v0.7" and kept the misleading money-saved name alive (#7).
- **Check.** `go test ./internal/ledger/`; the alias assertions in the ledger tests are deleted.
