package leakgate

import "bytes"

// The keyless shape rules. They need no key and no list: they flag identifiers
// by their form, so a fork's pull request and a card nobody has listed yet are
// covered too. One implementation serves the local scanner and the keyless
// tree test.
//
//	S1  a GPU id head: "GPU-" followed by hex digits as the driver prints it.
//	S2  a WSL UNC path naming a distro that is not a placeholder or a public one.
//
// Findings carry the rule name ("gpu-uuid", "wsl-unc") and never the matched
// text.

const (
	shapeGPU = "gpu-uuid"
	shapeWSL = "wsl-unc"
)

// ShapeFindings applies both rules to one text body (the bytes as read, after
// UTF-16 decoding or PNG text extraction, before the fold). The column is the
// 1-based byte offset of the match in its line.
func ShapeFindings(path string, data []byte) []Finding {
	var out []Finding
	out = shapeGPUIDs(path, data, out)
	out = shapeWSLPaths(path, data, out)
	return out
}

func isHexByte(c byte) bool { return hexVal(c) >= 0 }

// lineCol returns the 1-based line and column of byte offset off.
func lineCol(data []byte, off int) (int, int) {
	line := 1 + bytes.Count(data[:off], []byte{'\n'})
	col := off + 1
	if nl := bytes.LastIndexByte(data[:off], '\n'); nl >= 0 {
		col = off - nl
	}
	return line, col
}

// uniform reports whether all of s (lower-cased) is one repeated character.
func uniform(s []byte) bool {
	for _, c := range s {
		if lowerByte(c) != lowerByte(s[0]) {
			return false
		}
	}
	return len(s) > 0
}

func lowerByte(c byte) byte {
	if isUpper(c) {
		return c + 32
	}
	return c
}

// gpuPlaceholder: the first four digits one repeated nibble and the next four
// one repeated nibble (8 or more digits), or the whole head one repeated nibble
// (4 to 7 digits).
func gpuPlaceholder(h []byte) bool {
	if len(h) >= 8 {
		return uniform(h[:4]) && uniform(h[4:8])
	}
	return uniform(h)
}

func hasDecimal(h []byte) bool {
	for _, c := range h {
		if isDigit(c) {
			return true
		}
	}
	return false
}

// uuidGroups reports whether tail starts with two groups of exactly four hex
// digits ("-XXXX-XXXX") followed by a non-alphanumeric byte (or the end).
func uuidGroups(tail []byte) bool {
	if len(tail) < 10 || tail[0] != '-' || tail[5] != '-' {
		return false
	}
	for _, i := range []int{1, 2, 3, 4, 6, 7, 8, 9} {
		if !isHexByte(tail[i]) {
			return false
		}
	}
	return len(tail) == 10 || !alnum[tail[10]]
}

// gpuFlag is rule S1 for a maximal hex run after "GPU-" and what follows it.
func gpuFlag(run, tail []byte) bool {
	if len(tail) > 0 && isLetter(tail[0]) {
		return false // the hex run is the start of a word: GPU-accelerated, GPU-effective
	}
	switch n := len(run); {
	case n >= 8:
		return !gpuPlaceholder(run)
	case n >= 4:
		if !hasDecimal(run) || gpuPlaceholder(run) {
			return false
		}
		ellipsis := bytes.HasPrefix(tail, []byte("...")) || bytes.HasPrefix(tail, []byte("\xe2\x80\xa6"))
		return ellipsis || uuidGroups(tail)
	}
	return false
}

func shapeGPUIDs(path string, data []byte, out []Finding) []Finding {
	marker := []byte("GPU-")
	for i := 0; i < len(data); {
		idx := bytes.Index(data[i:], marker)
		if idx < 0 {
			break
		}
		at := i + idx
		p := at + len(marker)
		q := p
		for q < len(data) && isHexByte(data[q]) {
			q++
		}
		if q == p {
			i = at + 1
			continue
		}
		tailEnd := q + 24
		if tailEnd > len(data) {
			tailEnd = len(data)
		}
		if gpuFlag(data[p:q], data[q:tailEnd]) {
			line, col := lineCol(data, at)
			out = append(out, Finding{Path: path, Line: line, Col: col, Mode: "shape", Text: shapeGPU})
		}
		i = q
	}
	return out
}

// publicDistro reports whether a WSL distro name is one of the public names
// (compared case-insensitively): Ubuntu, Ubuntu-NN.NN, Debian, kali-linux,
// Alpine, docker-desktop, docker-desktop-data.
func publicDistro(d []byte) bool {
	l := asciiLower(d) // ASCII only: a non-ASCII byte never folds onto a public name
	switch string(l) {
	case "ubuntu", "debian", "kali-linux", "alpine", "docker-desktop", "docker-desktop-data":
		return true
	}
	if len(l) == 12 && bytes.HasPrefix(l, []byte("ubuntu-")) {
		v := l[7:]
		return isDigit(v[0]) && isDigit(v[1]) && v[2] == '.' && isDigit(v[3]) && isDigit(v[4])
	}
	return false
}

func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = lowerByte(c)
	}
	return out
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// wslNameEnd reports whether c ends the first path component of a UNC path.
func wslNameEnd(c byte) bool {
	switch c {
	case '/', '\\', '"', '\'', '`', ')':
		return true
	}
	return isASCIISpace(c)
}

func hasPrefixFold(b []byte, prefix string) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if lowerByte(b[i]) != prefix[i] {
			return false
		}
	}
	return true
}

func shapeWSLPaths(path string, data []byte, out []Finding) []Finding {
	for i := 0; i < len(data); i++ {
		if data[i] != 'w' && data[i] != 'W' {
			continue
		}
		rest := data[i:]
		var n int
		switch {
		case hasPrefixFold(rest, "wsl.localhost"):
			n = len("wsl.localhost")
		case hasPrefixFold(rest, "wsl$"):
			n = len("wsl$")
		default:
			continue
		}
		p := i + n
		s := p
		for s < len(data) && (data[s] == '/' || data[s] == '\\') {
			s++
		}
		if s == p {
			continue
		}
		e := s
		for e < len(data) && !wslNameEnd(data[e]) {
			e++
		}
		if e == s {
			continue
		}
		name := data[s:e]
		if name[0] != '<' && string(asciiLower(name)) != "distro" && !publicDistro(name) {
			line, col := lineCol(data, i)
			out = append(out, Finding{Path: path, Line: line, Col: col, Mode: "shape", Text: shapeWSL})
		}
		i = e - 1
	}
	return out
}
