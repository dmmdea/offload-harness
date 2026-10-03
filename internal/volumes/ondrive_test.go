package volumes

import "testing"

// The Windows long-path and device prefixes start with TWO backslashes. The first version
// of these tests wrote a single backslash on each side, which is not a prefix any Windows
// API emits, so DriveOf was "verified" against a spelling that never occurs and the real
// one went unrecognised. oneBackslash is ONE backslash, written as its code point on purpose (a run
// of backslashes in a source literal is the easiest thing in the language to misread), and
// TestPrefixFixturesAreTwoBackslashes pins the three spellings built from it byte by byte.
var oneBackslash = string(rune(92))

var (
	verbatim = oneBackslash + oneBackslash + "?" + oneBackslash // two backslashes, a question mark, one backslash
	device   = oneBackslash + oneBackslash + "." + oneBackslash // two backslashes, a dot, one backslash
	uncLead  = oneBackslash + oneBackslash                      // two backslashes: the lead of a UNC share
)

// TestPrefixFixturesAreTwoBackslashes keeps the fixtures above honest: if an edit turns
// them back into single-backslash spellings, every prefix case below would pass or fail
// for the wrong reason.
func TestPrefixFixturesAreTwoBackslashes(t *testing.T) {
	for name, c := range map[string]struct {
		got  string
		want []byte
	}{
		"verbatim": {verbatim, []byte{92, 92, '?', 92}},
		"device":   {device, []byte{92, 92, '.', 92}},
		"uncLead":  {uncLead, []byte{92, 92}},
	} {
		if got := []byte(c.got); string(got) != string(c.want) {
			t.Errorf("%s fixture is % x, want % x", name, got, c.want)
		}
	}
}

// TestDriveOf pins how a Windows path names its drive. The harness reads paths the
// operator typed into a config file, so every spelling a Windows tool accepts has to
// land on the same drive: either slash, either case, the long-path prefix, a drive
// with no slash after the colon.
func TestDriveOf(t *testing.T) {
	cases := []struct {
		path, want string
	}{
		{`C:\Users\x\.local-offload\media`, "C:"},
		{`c:/Users/x/.local-offload`, "C:"},
		{`D:`, "D:"},
		{`d:\`, "D:"},
		{verbatim + `C:\Users\x`, "C:"},
		{verbatim + `d:/data`, "D:"},
		{verbatim + `C:`, "C:"},
		{device + `C:`, "C:"},
		{device + `d:\data`, "D:"},
		{`//?/C:/Users/x`, "C:"},
		{`//./d:/data`, "D:"},
		{uncLead + `server\share\dir`, ""},       // a UNC share is not a local drive
		{`//server/share/dir`, ""},               // nor is its slash spelling
		{verbatim + `UNC\server\share`, ""},      // nor is its long-path spelling
		{verbatim + `UNC\server\share\C:\x`, ""}, // a drive-looking tail inside a share is still a share
		{verbatim, ""},                           // a prefix with nothing after it names nothing
		{`relative\dir`, ""},                     // resolves against the cwd: unknown, never guessed
		{`/var/lib/local-offload`, ""},
		{`~/.local-offload`, ""},
		{``, ""},
		{`C`, ""},    // a bare letter is a file name
		{`1:\x`, ""}, // a digit is not a drive letter
	}
	for _, c := range cases {
		if got := DriveOf(c.path); got != c.want {
			t.Errorf("DriveOf(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestOnDrive is the question doctor asks of every data path: does it sit on the
// drive Windows boots from?
func TestOnDrive(t *testing.T) {
	cases := []struct {
		path, root string
		want       bool
	}{
		{`C:\Users\x\.local-offload\cache.db`, `C:\`, true},
		{`c:/users/x/media`, `C:\`, true}, // case and slash style do not matter
		{`C:\x`, `c:`, true},              // nor does the root's spelling
		{`D:\local-offload\media`, `C:\`, false},
		{`D:\local-offload\media`, `D:\`, true},
		{uncLead + `server\share\media`, `C:\`, false},
		{verbatim + `UNC\server\share\media`, `C:\`, false},
		{verbatim + `C:\Users\x\.local-offload\cache.db`, `C:\`, true}, // the long-path spelling is the same drive
		{device + `c:\x`, `C:\`, true},
		{`C:\x`, verbatim + `C:\`, true}, // the root may be spelled that way too
		{`relative\media`, `C:\`, false}, // unknown is not "on the OS drive"
		{`C:\x`, ``, false},              // no OS drive known (a non-Windows host): nothing is on it
		{`C:\x`, `/`, false},             // a Unix root is not a drive
		{`C:\x`, `C:\Users`, false},      // a directory is not a drive root
	}
	for _, c := range cases {
		if got := OnDrive(c.path, c.root); got != c.want {
			t.Errorf("OnDrive(%q, %q) = %v, want %v", c.path, c.root, got, c.want)
		}
	}
}

// TestDriveRootOf: only a bare drive root (with or without its trailing slash)
// counts, so OnDrive cannot be fooled into treating "C:\Users" as the OS drive.
func TestDriveRootOf(t *testing.T) {
	for in, want := range map[string]string{
		`C:\`: "C:", `c:`: "C:", `C:/`: "C:", `D:\x`: "", `/`: "", ``: "", uncLead + `srv\share`: "",
		verbatim + `C:\`: "C:", verbatim + `d:`: "D:", device + `C:/`: "C:", `//?/C:/`: "C:",
		verbatim + `C:\Users`: "", verbatim + `UNC\srv\share`: "", verbatim: "",
	} {
		if got := DriveRootOf(in); got != want {
			t.Errorf("DriveRootOf(%q) = %q, want %q", in, got, want)
		}
	}
}
