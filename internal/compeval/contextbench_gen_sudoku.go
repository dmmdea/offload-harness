// contextbench_gen_sudoku.go - the sudoku-sketchpad task generator.
//
// Ported to Go from contextbench/tasks/sudoku_sketchpad/generate.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0). The board construction, the
// panel format and the order of random draws follow the original so the same seed
// streams the same operations; the code and comments are written for this repository
// and no upstream prose is carried over.
//
// The task: the agent keeps a whole sudoku board as a panel of (row,col,value) tuples
// in its context and edits it as one fully specified move arrives per turn. Gold is a
// list of per-version deltas; the grade is how many of the versions it displayed.
package compeval

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// CBSudokuSymbols are the single-character cell values: 1-9 then A-Z.
const CBSudokuSymbols = "123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func cbSym(v int) string { return string(CBSudokuSymbols[v-1]) }

// cbSolvedBoard is a random fully solved board built by shuffling a base pattern.
func cbSolvedBoard(side, box int, rng *pyRand) [][]int {
	pattern := func(r, c int) int { return (box*(r%box) + r/box + c) % side }
	shuffled := func(n int, base int) []int {
		s := make([]int, n)
		for i := range s {
			s[i] = base + i
		}
		rng.shuffleInts(s)
		return s
	}
	bands := shuffled(box, 0)
	var rows []int
	for _, g := range bands {
		for _, r := range shuffled(box, 0) {
			rows = append(rows, g*box+r)
		}
	}
	stacks := shuffled(box, 0)
	var cols []int
	for _, g := range stacks {
		for _, c := range shuffled(box, 0) {
			cols = append(cols, g*box+c)
		}
	}
	nums := shuffled(side, 1)
	out := make([][]int, len(rows))
	for i, r := range rows {
		out[i] = make([]int, len(cols))
		for j, c := range cols {
			out[i][j] = nums[pattern(r, c)]
		}
	}
	return out
}

type cbBoard struct {
	side   int
	placed [][]int
}

func newCBBoard(side int) *cbBoard {
	b := &cbBoard{side: side, placed: make([][]int, side)}
	for i := range b.placed {
		b.placed[i] = make([]int, side)
	}
	return b
}

// render is the panel the agent keeps: one tuple per cell, row-major.
func (b *cbBoard) render(version int) string {
	lines := []string{"<<<SKETCHPAD BEGIN>>>", fmt.Sprintf("VERSION: %d", version)}
	for r := 0; r < b.side; r++ {
		segs := make([]string, b.side)
		for c := 0; c < b.side; c++ {
			v := "."
			if b.placed[r][c] != 0 {
				v = cbSym(b.placed[r][c])
			}
			segs[c] = fmt.Sprintf("(%d,%d,%s)", r+1, c+1, v)
		}
		lines = append(lines, strings.Join(segs, " "))
	}
	lines = append(lines, "<<<SKETCHPAD END>>>")
	return strings.Join(lines, "\n")
}

// cells is the canonical gold mapping: "rXcY" to "=d" or ".".
func (b *cbBoard) cells() map[string]string {
	m := make(map[string]string, b.side*b.side)
	for r := 0; r < b.side; r++ {
		for c := 0; c < b.side; c++ {
			v := "."
			if b.placed[r][c] != 0 {
				v = fmt.Sprintf("=%d", b.placed[r][c])
			}
			m[fmt.Sprintf("r%dc%d", r+1, c+1)] = v
		}
	}
	return m
}

func cbDiff(before, after map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range after {
		if before[k] != v {
			out[k] = v
		}
	}
	return out
}

// GenerateSudoku builds one sudoku_sketchpad instance. When k.Turns is positive it
// overrides k.Boards and boards keep chaining until that many operations exist.
func GenerateSudoku(seed int64, k CBKnobs) *CBInstance {
	rng := newPyRand(uint64(seed))
	box, givens := k.Box, k.Givens
	side := box * box
	byTurns := k.Turns > 0

	var ops []CBOp
	var deltas []CBSudokuDelta
	initial := ""
	version := 0
	boardsUsed := 0
	done := func() bool {
		if byTurns {
			return len(ops) >= k.Turns
		}
		return boardsUsed >= k.Boards
	}

	for b := 0; !done(); b++ {
		full := cbSolvedBoard(side, box, rng)
		all := make([][2]int, 0, side*side)
		for r := 0; r < side; r++ {
			for c := 0; c < side; c++ {
				all = append(all, [2]int{r, c})
			}
		}
		rng.shufflePairs(all)
		given := map[[2]int]bool{}
		for _, cell := range all[:givens] {
			given[cell] = true
		}
		var empties [][2]int
		for _, cell := range all {
			if !given[cell] {
				empties = append(empties, cell)
			}
		}
		sort.Slice(empties, func(i, j int) bool {
			if empties[i][0] != empties[j][0] {
				return empties[i][0] < empties[j][0]
			}
			return empties[i][1] < empties[j][1]
		})
		state := newCBBoard(side)
		for cell := range given {
			state.placed[cell[0]][cell[1]] = full[cell[0]][cell[1]]
		}
		boardsUsed++
		cells := state.cells()

		if b == 0 {
			initial = state.render(0)
			deltas = append(deltas, CBSudokuDelta{V: 0, Reset: true, Cells: cells})
		} else {
			version++
			deltas = append(deltas, CBSudokuDelta{V: version, Reset: true, Cells: cells})
			body := fmt.Sprintf("NEW BOARD #%d (your sketchpad becomes version %d): the previous puzzle is complete. "+
				"Discard it and reset your sketchpad to this fresh puzzle — set the version id to %d and load this panel exactly:\n%s",
				b+1, version, version, state.render(version))
			ops = append(ops, CBOp{Index: len(ops), Header: fmt.Sprintf("new board #%d", b+1), Body: body})
			if byTurns && len(ops) >= k.Turns {
				break
			}
		}

		prev := cells
		for _, cell := range empties {
			r, c := cell[0], cell[1]
			sym := cbSym(full[r][c])
			version++
			state.placed[r][c] = full[r][c]
			after := state.cells()
			deltas = append(deltas, CBSudokuDelta{V: version, Cells: cbDiff(prev, after)})
			prev = after
			body := fmt.Sprintf("Move (board #%d): Place %s in cell r%dc%d (row %d from the top, column %d from the left).\n"+
				"In your sketchpad, set that cell's tuple value to %s and increment the VERSION line to %d. Leave every other cell unchanged.",
				b+1, sym, r+1, c+1, r+1, c+1, sym, version)
			ops = append(ops, CBOp{Index: len(ops), Header: fmt.Sprintf("board %d move v%d", b+1, version), Body: body})
			if byTurns && len(ops) >= k.Turns {
				break
			}
		}
	}

	toks := func(s string) int { return (utf8.RuneCountInString(s) + 1) / 4 }
	actions := 0
	for _, op := range ops {
		actions += toks(op.Body)
	}
	perAction := 0
	if len(ops) > 0 {
		perAction = actions / len(ops)
	}
	return &CBInstance{
		Family: CBFamilySudoku,
		Seed:   seed,
		Ops:    ops,
		Gold:   CBGold{Side: side, InitialGrid: initial, Deltas: deltas},
		Meta: map[string]any{
			"task_family": CBFamilySudoku, "seed": seed, "box": box, "side": side, "n_cells": side * side,
			"givens": givens, "boards": boardsUsed, "n_turns": len(ops), "approx_grid_tokens": toks(initial),
			"approx_actions_tokens": actions, "approx_per_action_tokens": perAction,
		},
	}
}
