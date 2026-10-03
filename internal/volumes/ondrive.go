package volumes

import "strings"

// bs is ONE backslash, written as its code point on purpose. The prefixes below begin with
// TWO of them, and a run of backslashes in a source literal is the easiest thing in the
// language to misread: the first version of this file carried one per side, which no
// Windows API emits, so a config path written `<2 backslashes>?<1>C:<1>...` was not
// recognised as being on the OS drive and the doctor FAIL stayed quiet about it.
var bs = string(rune(92))

// longPathPrefixes are the spellings Windows puts in front of a drive path: the verbatim
// prefix (`<2>?<1>`) and the device prefix (`<2>.<1>`), each in its slash form too.
var longPathPrefixes = []string{bs + bs + "?" + bs, "//?/", bs + bs + "." + bs, "//./"}

// stripLongPathPrefix drops one verbatim or device prefix, so `<2>?<1>C:<1>x` reads as
// `C:<1>x`. A UNC share behind a prefix (`UNC<1>srv<1>share`) still names no drive after
// the strip: its second byte is not a colon.
func stripLongPathPrefix(p string) string {
	for _, prefix := range longPathPrefixes {
		if strings.HasPrefix(p, prefix) {
			return p[len(prefix):]
		}
	}
	return p
}

// DriveOf names the Windows drive a path sits on: its upper-cased letter and a colon
// ("C:"), or "" when the path does not name one (a relative path, a UNC share, a
// Unix path). It is pure string work on purpose: the rule it feeds ("the OS drive
// holds Windows and program installs, never data") is decided from the path the
// operator WROTE, and the same input must give the same answer on every platform a
// test or a CI runner happens to run on.
func DriveOf(path string) string {
	p := strings.TrimSpace(path)
	// A verbatim or device prefix names the same drive as the bare path; the UNC form
	// behind one (`UNC` after the prefix) names none.
	p = stripLongPathPrefix(p)
	if len(p) < 2 || p[1] != ':' {
		return ""
	}
	c := p[0]
	switch {
	case c >= 'a' && c <= 'z':
		c -= 'a' - 'A'
	case c >= 'A' && c <= 'Z':
	default:
		return ""
	}
	return string(c) + ":"
}

// DriveRootOf is root's drive in the same spelling DriveOf returns, or "" when root
// is not a drive root. A directory under a drive ("C:\Users") is not a root.
func DriveRootOf(root string) string {
	d := DriveOf(root)
	if d == "" {
		return ""
	}
	rest := stripLongPathPrefix(strings.TrimSpace(root))
	if rest = rest[2:]; rest == "" || rest == `\` || rest == "/" {
		return d
	}
	return ""
}

// OnDrive reports whether path sits on the drive root names ("C:\", "c:", "C:/").
// Both sides are read with DriveOf, so a relative path, a UNC share or an empty root
// is never "on" anything.
func OnDrive(path, root string) bool {
	r := DriveRootOf(root)
	return r != "" && DriveOf(path) == r
}
