package winexec

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"unicode/utf16"
)

// TestEncodedCommandArgs_RoundTripsUnicode is a pure decode check (no process spawn):
// base64-decodes what EncodedCommandArgs built and confirms it is the exact UTF-16LE
// encoding of the script, on every platform this test suite runs on.
func TestEncodedCommandArgs_RoundTripsUnicode(t *testing.T) {
	script := "Write-Output '¿VOLVERÁ EL ROTATIVO? ñ ü é'"
	args := EncodedCommandArgs(script)
	for _, want := range []string{"-NoProfile", "-NonInteractive", "-EncodedCommand"} {
		found := false
		for _, a := range args {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("EncodedCommandArgs(%q) = %v, missing %q", script, args, want)
		}
	}
	b64 := args[len(args)-1]
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("EncodedCommandArgs produced invalid base64: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("UTF-16LE payload must have an even byte length, got %d", len(raw))
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	got := string(utf16.Decode(units))
	if got != script {
		t.Fatalf("round trip = %q, want %q", got, script)
	}
}

func TestEncodedCommandArgs_EmptyScript(t *testing.T) {
	args := EncodedCommandArgs("")
	raw, err := base64.StdEncoding.DecodeString(args[len(args)-1])
	if err != nil || len(raw) != 0 {
		t.Fatalf("EncodedCommandArgs(\"\") payload = %q, err=%v, want empty", raw, err)
	}
}

func TestSafeScriptArgs_PrependsOutputEncodingFix(t *testing.T) {
	args := SafeScriptArgs("Write-Output 'hi'")
	raw, err := base64.StdEncoding.DecodeString(args[len(args)-1])
	if err != nil {
		t.Fatalf("SafeScriptArgs produced invalid base64: %v", err)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	got := string(utf16.Decode(units))
	want := setUTF8OutputPrefix + "Write-Output 'hi'"
	if got != want {
		t.Fatalf("decoded script = %q, want %q", got, want)
	}
}

// TestEncodedCommandArgs_TextSurvivesArgvDecoding is a control: it confirms that a
// non-ASCII "-Command" argument DOES reach PowerShell's own parser correctly (isolated
// by having the script write the text it received straight to a file, never through
// Write-Output/stdout — see TestSafeScriptArgs_SurvivesPowerShell5OutputMangling for
// why routing through captured stdout would confound this). This documents that F-41's
// corruption (below) is an OUTPUT-capture defect, not an argv one — EncodedCommandArgs
// is still the right tool for safely embedding arbitrary caller text into a script
// (see its doc comment), just not because argv itself was broken.
func TestEncodedCommandArgs_TextSurvivesArgvDecoding(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 only runs on Windows")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe not on PATH")
	}
	text := "¿VOLVERÁ EL ROTATIVO? ñ ü é"
	outFile := filepath.Join(t.TempDir(), "argv.txt")
	script := fmt.Sprintf(
		`[System.IO.File]::WriteAllText("%s", '%s', (New-Object System.Text.UTF8Encoding($false)))`,
		filepath.ToSlash(outFile), text)

	cmd := exec.Command("powershell.exe", EncodedCommandArgs(script)...)
	if err := cmd.Run(); err != nil {
		t.Fatalf("EncodedCommand invocation failed: %v", err)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if string(got) != text {
		t.Fatalf("script received %q via argv, want %q", got, text)
	}
}

// TestSafeScriptArgs_SurvivesPowerShell5OutputMangling is the live F-41 regression.
// Isolating the two hops (argv-in vs stdout-out — see the package doc's A/B/C/D
// breakdown, reproduced here as cases raw/fixed) showed the corruption is entirely in
// OUTPUT capture: Windows PowerShell 5.1's default Console.OutputEncoding for a
// redirected pipe is a legacy code page, not UTF-8, regardless of how the script text
// itself was passed in. SafeScriptArgs fixes it by setting Console.OutputEncoding to
// UTF-8 before the script's Write-Output runs.
//
// Skips cleanly off Windows or where powershell.exe is not on PATH (CI parity with
// the repo's other hardware-gated tests, e.g. internal/mediaops's TestWorkerSelftest).
// Comment out setUTF8OutputPrefix's use in SafeScriptArgs to see this go red.
func TestSafeScriptArgs_SurvivesPowerShell5OutputMangling(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 only runs on Windows")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe not on PATH")
	}
	text := "¿VOLVERÁ EL ROTATIVO? ñ ü é"
	echo := "Write-Output '" + text + "'"

	rawCmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", echo)
	rawOut, err := rawCmd.Output()
	if err != nil {
		t.Fatalf("raw -Command invocation failed: %v", err)
	}
	rawGot := trimEOL(string(rawOut))
	if rawGot == text {
		t.Skip("this powershell.exe/locale did not reproduce the F-41 output mangling — nothing to prove the fix against here")
	}

	fixedCmd := exec.Command("powershell.exe", SafeScriptArgs(echo)...)
	fixedOut, err := fixedCmd.Output()
	if err != nil {
		t.Fatalf("SafeScriptArgs invocation failed: %v", err)
	}
	fixedGot := trimEOL(string(fixedOut))
	if fixedGot != text {
		t.Fatalf("SafeScriptArgs output = %q, want %q (raw -Command output was %q, proving this host mangles captured stdout)",
			fixedGot, text, rawGot)
	}
}

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
