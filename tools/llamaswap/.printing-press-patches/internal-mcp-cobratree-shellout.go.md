# Patch: `internal/mcp/cobratree/shellout.go`

**Wave:** no-visible-windows fix round (2026-10-12)

**Upstream status:** **Not yet upstreamed.** The emitted `shellout.go` builds the companion
CLI's command with a bare `exec.CommandContext`; the same one-line change belongs in the Printing
Press template, and until it lands a reprint of this tree silently reverts the fix.

**Twin:** the identical change is made for `comfyui-pp-cli` (`0006-hide-companion-cli-window.patch`
in that tree's `.printing-press-patches/`). The two copies must stay semantically identical, and
the copies of both CLIs outside this repository owe the same patch.

## What was changed

`RunCLICommand` now takes its command from a new `newCLICommand(ctx, binPath, args)`, which
builds it with `exec.CommandContext` exactly as before and then calls `hideWindow(cmd)`.
`hideWindow` lives in two new hand-authored files in the same package: `hide_windows.go`
(`CREATE_NO_WINDOW` plus `SysProcAttr.HideWindow`, other attributes kept) and `hide_other.go`
(a no-op). `hide_windows_test.go` pins the flags on the command `newCLICommand` returns, and the
argv-echo helper that `shellout_test.go` builds with `go build` is hidden the same way.

## Why

`RunCLICommand` runs once per Cobra-mirror MCP tool call, in an MCP server that its host
normally starts without a console. A console program started by a process with no console gets a
NEW visible console window on Windows, and with Windows Terminal as the default terminal that is
a Windows Terminal window on the operator's desktop, once per tool call. The rest of this tree
already hid its children (`hideSpawnedWindow`, `measure.HideWindow`); this was the one start that
did not.

## What a regen must preserve

1. `newCLICommand` and the `hideWindow(cmd)` call in it, or any equivalent that gives the child
   `CREATE_NO_WINDOW` and `HideWindow` on Windows.
2. `hide_windows.go`, `hide_other.go` and `hide_windows_test.go` (hand-authored; a regen does not
   delete them, but the generated `shellout.go` stops calling them).
3. The harness's `TestEveryChildProcessStartIsHidden` (`internal/winexec`, in the root module)
   reads this file and fails when `RunCLICommand`'s start is not hidden.
