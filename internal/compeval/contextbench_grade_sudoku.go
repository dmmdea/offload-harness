// contextbench_grade_sudoku.go - the sudoku-sketchpad grader.
//
// Ported to Go from contextbench/graders/sudoku.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0): parsing sketchpad panels out
// of the frames, folding the gold's per-version deltas back into full boards, and
// scoring how many versions the agent displayed exactly. The code and comments are
// written for this repository. One deliberate difference: a cell value must be a
// single symbol; the original also accepts a multi-character run that happens to be
// a substring of the symbol table.
package compeval

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	cbSudokuTupleRE   = regexp.MustCompile(`\(\s*(\d+)\s*,\s*(\d+)\s*,\s*([^)]*?)\s*\)`)
	cbSudokuVersionRE = regexp.MustCompile(`VERSION:\s*(\d+)`)
)

// cbPanel is one parsed board: cell name ("rXcY") to value ("." or "=d").
type cbPanel map[string]string

// key is the canonical form of a panel, so equal boards compare equal.
func (p cbPanel) key() string {
	names := sortedKeys(p)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(p[k])
		b.WriteByte(';')
	}
	return b.String()
}

// cbParsePanelBody parses one board body. Every cell locates itself through its
// (row,col,value) tuple and a later occurrence wins.
func cbParsePanelBody(body []string, side int) cbPanel {
	cells := cbPanel{}
	for _, m := range cbSudokuTupleRE.FindAllStringSubmatch(strings.Join(body, "\n"), -1) {
		r, _ := strconv.Atoi(m[1])
		c, _ := strconv.Atoi(m[2])
		v := strings.TrimSpace(m[3])
		if r < 1 || r > side || c < 1 || c > side {
			continue
		}
		name := "r" + strconv.Itoa(r) + "c" + strconv.Itoa(c)
		switch {
		case v == "." || v == "":
			cells[name] = "."
		case len(v) == 1 && strings.Contains(CBSudokuSymbols[:side], v):
			cells[name] = "=" + strconv.Itoa(strings.Index(CBSudokuSymbols, v)+1)
		}
	}
	return cells
}

type cbParsedBlock struct {
	version int // -1 when the block carries no VERSION line
	cells   cbPanel
}

// cbParseBlocks returns every sketchpad block in text.
func cbParseBlocks(text string, side int) []cbParsedBlock {
	var blocks []cbParsedBlock
	var buf []string
	open := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(line, "SKETCHPAD BEGIN"):
			buf, open = nil, true
		case strings.Contains(line, "SKETCHPAD END"):
			if open {
				ver := -1
				for _, bl := range buf {
					if m := cbSudokuVersionRE.FindStringSubmatch(bl); m != nil {
						ver, _ = strconv.Atoi(m[1])
						break
					}
				}
				if cells := cbParsePanelBody(buf, side); len(cells) > 0 {
					blocks = append(blocks, cbParsedBlock{ver, cells})
				}
			}
			buf, open = nil, false
		case open:
			buf = append(buf, line)
		}
	}
	return blocks
}

func cbCellAccuracy(gold, state cbPanel) float64 {
	if len(gold) == 0 {
		return 0
	}
	hit := 0
	for k, v := range gold {
		if state[k] == v {
			hit++
		}
	}
	return float64(hit) / float64(len(gold))
}

type cbGoldState struct {
	v     int
	cells cbPanel
}

// cbGoldStates folds the compact per-version deltas into full boards.
func cbGoldStates(deltas []CBSudokuDelta) []cbGoldState {
	var out []cbGoldState
	cur := cbPanel{}
	for _, d := range deltas {
		if d.Reset {
			cur = cbPanel{}
		}
		for k, v := range d.Cells {
			cur[k] = v
		}
		cp := make(cbPanel, len(cur))
		for k, v := range cur {
			cp[k] = v
		}
		out = append(out, cbGoldState{d.V, cp})
	}
	return out
}

func gradeSudoku(gold CBGold, frames []CBFrame) CBGrade {
	// No silent default board size: a larger board graded on a 9x9 assumption would
	// look healthy while it was wrong.
	if gold.Side <= 0 {
		return invalidGrade(len(frames), "gold has no board side; refusing to default to 9x9")
	}
	if len(frames) == 0 {
		return invalidGrade(0, "no context snapshots")
	}
	side := gold.Side

	byVersion := map[int]map[string]cbPanel{}
	content := map[string]bool{}
	var final cbPanel
	for _, f := range frames {
		blocks := cbParseBlocks(cbFrameText(f), side)
		bestVer, bestIdx := -1, -1
		for i, b := range blocks {
			content[b.cells.key()] = true
			if b.version >= 0 {
				if byVersion[b.version] == nil {
					byVersion[b.version] = map[string]cbPanel{}
				}
				byVersion[b.version][b.cells.key()] = b.cells
				if bestIdx < 0 || b.version >= bestVer {
					bestVer, bestIdx = b.version, i
				}
			}
		}
		if bestIdx >= 0 {
			final = blocks[bestIdx].cells
		} else if len(blocks) > 0 {
			final = blocks[len(blocks)-1].cells
		}
	}

	total, versioned, contentHit := 0, 0, 0
	cellSum := 0.0
	var finalGold cbPanel
	for _, gs := range cbGoldStates(gold.Deltas) {
		if gs.v == 0 {
			continue
		}
		total++
		finalGold = gs.cells
		k := gs.cells.key()
		if _, ok := byVersion[gs.v][k]; ok {
			versioned++
		}
		if content[k] {
			contentHit++
		}
		if seen := byVersion[gs.v]; len(seen) > 0 {
			names := make([]string, 0, len(seen))
			for n := range seen {
				names = append(names, n)
			}
			sort.Strings(names)
			best := 0.0
			for _, n := range names {
				if a := cbCellAccuracy(gs.cells, seen[n]); a > best {
					best = a
				}
			}
			cellSum += best
		}
	}
	if total == 0 {
		return invalidGrade(len(frames), "gold has no graded versions")
	}
	finalEM, finalCell := 0.0, 0.0
	if final != nil && finalGold != nil {
		if final.key() == finalGold.key() {
			finalEM = 1
		}
		finalCell = cbCellAccuracy(finalGold, final)
	}
	t := float64(total)
	return CBGrade{
		Reward: float64(contentHit) / t, Valid: true, Matched: versioned, Total: total, Frames: len(frames),
		Sub: map[string]float64{
			"reward_content": float64(contentHit) / t, "reward_versioned": float64(versioned) / t,
			"reward_cell": cellSum / t, "reward_final_em": finalEM, "reward_final_cell": finalCell,
		},
	}
}
