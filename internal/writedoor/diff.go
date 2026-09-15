package writedoor

import (
	"fmt"
	"strings"
)

// contextLines is the standard unified-diff context: three lines each side.
const contextLines = 3

// lcsMaxLines bounds the quadratic middle of the diff. Prefix/suffix trimming
// already collapses the common "one line changed in a long file" case to a
// handful of lines, so this ceiling is only reached when the two versions are
// genuinely unrelated — where a minimal diff has no review value anyway and a
// whole-file replacement is the honest rendering. Without the bound a seat
// could hand the node a pair of 8,000-line files and cost it a 64-million-cell
// table, which is a denial of service dressed as a diff.
const lcsMaxLines = 2000

// noEOLMarker is the unified-diff note for a line that ends the file without a
// terminator. Written as a raw literal because its first byte IS a backslash,
// and spelling that as a Go escape is how it gets lost.
const noEOLMarker = `\ No newline at end of file` + "\n"

// line is one source line WITHOUT its terminator, plus whether it ended the
// file with no terminator at all. The flag is part of the comparison KEY, not
// decoration: "a\nb" and "a\nb\n" split into identical text, so a diff that
// compared text alone would render a trailing-newline change as no change —
// silently dropping a real edit from the write set.
type line struct {
	text  string
	noEOL bool
}

func (l line) key() string {
	if l.noEOL {
		return l.text + "\x00\\ No newline at end of file"
	}
	return l.text
}

// splitLines splits s into lines, tagging the last one when s does not end in a
// newline. The empty string is zero lines (an empty file), not one empty line.
func splitLines(s string) []line {
	if s == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(s, "\n")
	noEOL := trimmed == s
	parts := strings.Split(trimmed, "\n")
	out := make([]line, len(parts))
	for i, p := range parts {
		out[i] = line{text: p}
	}
	if noEOL {
		out[len(out)-1].noEOL = true
	}
	return out
}

// op is one edit-script entry.
type op struct {
	kind byte // ' ' context, '-' removed, '+' added
	line line
}

// hunks renders the unified-diff body (every @@ hunk) for one file pair.
func hunks(before, after string) string {
	script := editScript(splitLines(before), splitLines(after))

	var out strings.Builder
	oldNo, newNo := 1, 1
	i := 0
	for i < len(script) {
		if script[i].kind == ' ' {
			oldNo++
			newNo++
			i++
			continue
		}
		// Back up over the leading context.
		start := i
		lead := 0
		for start > 0 && script[start-1].kind == ' ' && lead < contextLines {
			start--
			lead++
		}
		hOldStart, hNewStart := oldNo-lead, newNo-lead

		// Extend forward through changes, absorbing unchanged runs short
		// enough that two hunks would overlap (the standard coalescing rule).
		end := i
		for end < len(script) {
			if script[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(script) && script[run].kind == ' ' {
				run++
			}
			if run-end <= 2*contextLines && run < len(script) {
				end = run
				continue
			}
			break
		}
		trail := 0
		for end < len(script) && script[end].kind == ' ' && trail < contextLines {
			end++
			trail++
		}

		oldCount, newCount := 0, 0
		for _, o := range script[start:end] {
			switch o.kind {
			case ' ':
				oldCount++
				newCount++
			case '-':
				oldCount++
			case '+':
				newCount++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", rangeSpec(hOldStart, oldCount), rangeSpec(hNewStart, newCount))
		for _, o := range script[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.line.text)
			out.WriteByte('\n')
			if o.line.noEOL {
				out.WriteString(noEOLMarker)
			}
		}
		oldNo, newNo = hOldStart+oldCount, hNewStart+newCount
		i = end
	}
	return out.String()
}

// rangeSpec renders a unified-diff range. A zero-length range is written with
// the line number of the position BEFORE it, which is what patch expects.
func rangeSpec(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start-1)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// editScript produces the ' '/'-'/'+' script for two line slices: common prefix
// and suffix are matched cheaply, and the differing middle goes through an LCS
// table (or, past lcsMaxLines, a whole-middle replacement).
func editScript(oldLines, newLines []line) []op {
	pre := 0
	for pre < len(oldLines) && pre < len(newLines) && oldLines[pre].key() == newLines[pre].key() {
		pre++
	}
	suf := 0
	for suf < len(oldLines)-pre && suf < len(newLines)-pre &&
		oldLines[len(oldLines)-1-suf].key() == newLines[len(newLines)-1-suf].key() {
		suf++
	}
	midOld := oldLines[pre : len(oldLines)-suf]
	midNew := newLines[pre : len(newLines)-suf]

	script := make([]op, 0, len(oldLines)+len(newLines))
	for _, l := range oldLines[:pre] {
		script = append(script, op{' ', l})
	}
	if len(midOld) > lcsMaxLines || len(midNew) > lcsMaxLines {
		for _, l := range midOld {
			script = append(script, op{'-', l})
		}
		for _, l := range midNew {
			script = append(script, op{'+', l})
		}
	} else {
		script = append(script, lcsScript(midOld, midNew)...)
	}
	for _, l := range oldLines[len(oldLines)-suf:] {
		script = append(script, op{' ', l})
	}
	return script
}

// lcsScript is the classic longest-common-subsequence edit script. Deletions
// are emitted before insertions at the same position, which is what every diff
// reader expects to see.
func lcsScript(a, b []line) []op {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i].key() == b[j].key():
				table[i][j] = table[i+1][j+1] + 1
			case table[i+1][j] >= table[i][j+1]:
				table[i][j] = table[i+1][j]
			default:
				table[i][j] = table[i][j+1]
			}
		}
	}
	out := make([]op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i].key() == b[j].key():
			out = append(out, op{' ', a[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			out = append(out, op{'-', a[i]})
			i++
		default:
			out = append(out, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, op{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, op{'+', b[j]})
	}
	return out
}
