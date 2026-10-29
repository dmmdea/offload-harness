### Changed - warnMissingGatedModels takes one gatedModelSet instead of nine positional bools

- **What changed.** `warnMissingGatedModels` and `warnMissingGatedModelsTo` in `install_render.go` now take a `gatedModelSet` struct with nine named bool fields in place of nine positional bools; the one production caller and the three test helpers in `install_gatedmodels_test.go` are converted. The warning text and the per-model warning order are unchanged.
- **Why.** Nine adjacent bools of the same type could be swapped at a call site without the compiler noticing, and the gate of one model would silently become the gate of another.
- **Check.** `go build ./...`, `go vet .` and `go test . -run 'Gated|Render|Install|Changelog|Fragment'` pass with no change to any expected warning.
