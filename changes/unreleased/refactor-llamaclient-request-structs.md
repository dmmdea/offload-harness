### Changed - `llamaclient` Generate, GenerateVision and GenerateVisionInterleaved take a request struct instead of eight to ten positional parameters

- **What changed.** `Client.Generate(ctx, GenRequest{Model, System, User, Grammar, MaxTokens, Temperature, TopLogprobs}, opts...)`,
  `Client.GenerateVision(ctx, VisionRequest{... ImageDataURIs ...}, opts...)` and
  `Client.GenerateVisionInterleaved(ctx, InterleavedRequest{... FrameLabels, ImageDataURIs, TrailingUser ...}, opts...)`
  replace the long positional lists, and every caller (`internal/pipeline`, `internal/backendchain`, the root affinity seam
  test and the `internal/llamaclient` tests) is converted. The `GenOption` functional options are unchanged.
- **Why.** Seven adjacent string, int and float arguments in a row (`"", "", "u", "", 64, 0, 0`) are easy to transpose and
  unreadable at the call site; named fields make a swap a compile-visible edit.
- **Check.** A pure refactor: the request bodies are built from the same values by the same code, and the existing
  `internal/llamaclient` tests, which assert the bodies on httptest servers, pass unchanged apart from the call syntax.
