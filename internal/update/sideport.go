package update

// sideChatURL is the chat route of the smoke gate's own side-port llama-server: the server the gate starts on
// update.gate.side_port, under the GPU lease the gate holds, to measure a build before it goes live. It is never
// llama-swap, so a request to it cannot make llama-swap load a model onto a card a lease fences; that is why
// upstream_fence_lint_test.go admits this file by name. A request through llama-swap goes through the fence
// (modelaffinity.AwaitModelRoute), as WatchFirstLoad's does.
func sideChatURL(sideBase string) string { return sideBase + "/v1/chat/completions" }
