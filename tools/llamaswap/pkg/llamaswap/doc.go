// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

// Package llamaswap is the public, importable Go client for a llama-swap proxy.
//
// It exists because every consumer that has ever talked to llama-swap on this
// box re-implemented the same three things badly: alias resolution, keep-set
// protection, and the auto-start trap. This package is the one arrangement of
// those rules, so an importing program inherits them instead of rediscovering
// them.
//
// # What it guarantees
//
//   - Alias resolution. llama-swap models answer to a canonical id plus any
//     number of aliases published under meta.llamaswap.aliases. Every method
//     that takes a model name accepts either spelling; [Client.Resolve] exposes
//     the mapping directly.
//
//   - Keep-set protection from CONFIG, never from the server. The live API
//     reports ttl:0 for a seat configured ttl:-1 (verified on v249), so a
//     keep-set derived from the server would rest on a value the server
//     misreports. [Client.KeepSet] reads the llama-swap YAML plus the CLI's own
//     config file, and [Client.Unload] refuses a protected member — matched by
//     alias as well as by id — unless the caller explicitly overrides.
//
//   - The auto-start trap is respected. GET /upstream/{model}/anything makes
//     llama-swap START that model, so a "probe" can trigger a multi-GB load.
//     [Client.Props], [Client.Slots], and the drain check consult /running
//     first and refuse rather than silently loading a model.
//
//   - Typed errors that map one-to-one onto the CLI's exit codes, so a program
//     and a shell script branch on the same taxonomy. See [ExitCode].
//
// # Loopback discipline
//
// The default base URL is the 127.0.0.1 literal, never a loopback hostname. On
// a dual-stack Windows host "localhost" resolves ::1 first; when the proxy
// binds IPv4 only, every call eats the full IPv6 connect timeout (~21s) before
// falling back. [New] normalizes a loopback hostname to the literal for the
// same reason. Non-loopback hostnames (a Tailscale MagicDNS name for a remote
// rig, say) are left exactly as given.
//
// # What it deliberately does not do
//
// It never writes the llama-swap YAML, never restarts the service, and never
// runs a background watcher. Those are operator actions with real blast radius;
// the CLI surfaces them and a human runs them.
//
// # Usage
//
//	c, err := llamaswap.New("", nil)          // "" → http://127.0.0.1:11436
//	if err != nil {
//		return err
//	}
//	id, aliases, err := c.Resolve(ctx, "local-embed")   // → "embeddinggemma"
//
// The zero value of [Options] is the supported default for every field.
package llamaswap
