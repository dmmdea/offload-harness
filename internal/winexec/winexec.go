// Package winexec builds safe argv for spawning Windows PowerShell (powershell.exe,
// i.e. Windows PowerShell 5.1, as distinct from pwsh/PowerShell 7) so text carried
// through it — script content in, captured stdout out — survives non-ASCII
// characters correctly.
//
// F-41 (2026-09-24, reproduced live on a plain Windows box, isolating each hop in
// turn): the observed defect is NOT in argv decoding — a non-ASCII "-Command <script>"
// argument reaches PowerShell's parser correctly (Go's os/exec always calls the WIDE
// CreateProcessW on Windows, and confirmed here by having the script write the text it
// received straight to a file: byte-perfect, with or without -EncodedCommand). It is
// in OUTPUT capture: Windows PowerShell 5.1's default Console.OutputEncoding, when
// stdout is redirected to a pipe (which os/exec.Cmd.Output/CombinedOutput always is),
// follows the host's legacy OEM/ANSI code page rather than UTF-8. Measured round trip
// for "¿VOLVERÁ EL ROTATIVO? ñ ü é" via `Write-Output` on a pipe:
//
//	without the fix: \xa8VOLVERA EL ROTATIVO? \xa4 \x81 \x82   (Á silently dropped to
//	                                                             bare A; ¿ ñ ü é mapped
//	                                                             to unrelated single-
//	                                                             byte codepage values)
//	with the fix   : ¿VOLVERÁ EL ROTATIVO? ñ ü é                (byte-perfect)
//
// Setting Console.OutputEncoding to UTF-8 before the script produces any output fixes
// this completely, with or without -EncodedCommand — see
// TestSafeScriptArgs_SurvivesPowerShell5OutputMangling for the live proof (and its
// A/B/C/D breakdown of which combination fixes what, kept in the test's comment since
// it is exactly how this was isolated).
package winexec

import (
	"encoding/base64"
	"unicode/utf16"
)

// setUTF8OutputPrefix is prepended by SafeScriptArgs. See the package doc for the
// measured before/after.
const setUTF8OutputPrefix = "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "

// EncodedCommandArgs returns the powershell.exe argv for running `script` via
// -EncodedCommand (a base64 UTF-16LE blob) instead of a raw "-Command <script>"
// argument. This does not by itself fix F-41 (see the package doc — that is an output-
// capture defect, not an argv one), but it is still the right way to embed arbitrary
// caller-supplied text into a script: -Command re-tokenizes its argument as
// PowerShell source, so a caller's text containing a quote, backtick, or `$` can break
// out of whatever quoting the caller built by hand. -EncodedCommand sidesteps that
// entirely. Safe for any Unicode script text, including the empty string.
func EncodedCommandArgs(script string) []string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8)) // UTF-16LE
	}
	return []string{"-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(buf)}
}

// SafeScriptArgs returns the powershell.exe argv for running `script` such that (1)
// its own stdout, when captured through a redirected pipe, comes back as UTF-8
// instead of mangled through WinPS 5.1's legacy default (F-41 — the package doc has
// the measured before/after) and (2) the script text itself is carried via
// -EncodedCommand, so caller-supplied text embedded in it cannot break PowerShell's
// own argument tokenizing (see EncodedCommandArgs). This is the helper to reach for
// whenever the caller will parse the script's captured stdout and it might contain
// non-ASCII text; Go's string(out) already assumes UTF-8, so no decoding step is
// needed on the caller's side.
func SafeScriptArgs(script string) []string {
	return EncodedCommandArgs(setUTF8OutputPrefix + script)
}
