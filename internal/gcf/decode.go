package gcf

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Decode is the ROUND-TRIP ORACLE. Production never calls it — the model
// reads the compact form directly — but every encoding must decode back to
// deep-equal rows in tests and fuzz, or the encoder is not lossless and may
// not ship. Mirrors the upstream design (their reconstructHeadroom exists
// solely as the reversibility proof).
//
// Input is one GCF block exactly as Compact emits it (fence lines included).
// Output rows carry the same typing parseEligible produced: string,
// json.Number, bool, nil; absent keys are absent (not nil).
func Decode(block string) ([]row, error) {
	lines := strings.Split(strings.TrimSpace(block), "\n")
	if len(lines) < 4 {
		return nil, fmt.Errorf("gcf: block too short (%d lines)", len(lines))
	}
	if lines[0] != fenceOpen {
		return nil, fmt.Errorf("gcf: missing open fence, got %q", lines[0])
	}
	if lines[1] != fenceHeader {
		return nil, fmt.Errorf("gcf: missing profile header, got %q", lines[1])
	}
	if lines[len(lines)-1] != "```" {
		return nil, fmt.Errorf("gcf: missing close fence")
	}

	// Structural parse — Sscanf's %s is whitespace-delimited and a field name
	// may legally contain interior spaces (fuzz-found).
	hdr := lines[2]
	if !strings.HasPrefix(hdr, "## [") || !strings.HasSuffix(hdr, "}") {
		return nil, fmt.Errorf("gcf: bad header %q", hdr)
	}
	sep := strings.Index(hdr, "]{")
	if sep < 0 {
		return nil, fmt.Errorf("gcf: bad header %q", hdr)
	}
	var n int
	if _, err := fmt.Sscanf(hdr[len("## ["):sep], "%d", &n); err != nil {
		return nil, fmt.Errorf("gcf: bad row count in %q: %w", hdr, err)
	}
	fields := strings.Split(hdr[sep+2:len(hdr)-1], ",")

	rowLines := lines[3 : len(lines)-1]
	if len(rowLines) != n {
		return nil, fmt.Errorf("gcf: header says %d rows, block has %d", n, len(rowLines))
	}

	rows := make([]row, 0, n)
	for i, rl := range rowLines {
		cells, err := splitCells(rl)
		if err != nil {
			return nil, fmt.Errorf("gcf: row %d: %w", i, err)
		}
		if len(cells) != len(fields) {
			return nil, fmt.Errorf("gcf: row %d has %d cells, want %d", i, len(cells), len(fields))
		}
		r := row{Vals: map[string]any{}}
		for j, c := range cells {
			v, present, err := decodeCell(c)
			if err != nil {
				return nil, fmt.Errorf("gcf: row %d col %d: %w", i, j, err)
			}
			if present {
				r.Keys = append(r.Keys, fields[j])
				r.Vals[fields[j]] = v
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// splitCells splits one row line on pipes, honoring JSON-quoted cells (a
// quoted cell may contain pipes; its escapes were produced by json.Marshal so
// a quote inside is always backslash-escaped).
func splitCells(line string) ([]string, error) {
	var cells []string
	i := 0
	for {
		if i < len(line) && line[i] == '"' {
			j := i + 1
			for j < len(line) {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == '"' {
					break
				}
				j++
			}
			if j >= len(line) {
				return nil, fmt.Errorf("unterminated quoted cell")
			}
			cells = append(cells, line[i:j+1])
			i = j + 1
			if i == len(line) {
				return cells, nil
			}
			if line[i] != '|' {
				return nil, fmt.Errorf("junk after quoted cell")
			}
			i++
			continue
		}
		j := strings.IndexByte(line[i:], '|')
		if j < 0 {
			cells = append(cells, line[i:])
			return cells, nil
		}
		cells = append(cells, line[i:i+j])
		i += j + 1
	}
}

// decodeCell inverts encodeCell. present=false only for the missing sentinel.
func decodeCell(c string) (v any, present bool, err error) {
	switch c {
	case cellMissing:
		return nil, false, nil
	case cellNull:
		return nil, true, nil
	case "true":
		return true, true, nil
	case "false":
		return false, true, nil
	}
	if len(c) > 0 && c[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(c), &s); err != nil {
			return nil, false, fmt.Errorf("bad quoted cell %q: %w", c, err)
		}
		return s, true, nil
	}
	if looksNumeric(c) {
		return json.Number(c), true, nil
	}
	return c, true, nil // bare string.
}
