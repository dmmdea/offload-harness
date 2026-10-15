// contextbench_levels.go - the levels of record: which generator knobs make which
// named task size, per family.
//
// The table is data ported from contextbench/configs/paper.yaml in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0): the knob values of each
// published level, with the family's fixed knobs merged in. The budget of record is
// 32768 tokens with 2048 reserved (CBDefaultBudget, CBDefaultReserve), and the
// published seeds are 42 through 49.
package compeval

import (
	"fmt"
	"strings"
)

// The context budget of record and the reserve held back for the reply.
const (
	CBDefaultBudget  = 32768
	CBDefaultReserve = 2048
)

// CBLevel is one named task size.
type CBLevel struct {
	Name  string
	Knobs CBKnobs
}

func kvLevel(name string, records int) CBLevel {
	return CBLevel{name, CBKnobs{Records: records, ValueWords: 24, Queries: 24, RecordsPerChunk: 100}}
}

func logLevel(name string, lines, perBatch int) CBLevel {
	return CBLevel{name, CBKnobs{Lines: lines, LinesPerBatch: perBatch, Queries: 24}}
}

func needleLevel(name string, k, chunks int) CBLevel {
	return CBLevel{name, CBKnobs{NeedlesPerChunk: k, Chunks: chunks, FillerLines: 140, WordsPerLine: 10}}
}

func sudokuLevel(name string, turns int) CBLevel {
	return CBLevel{name, CBKnobs{Box: 4, Givens: 90, Boards: 1, Turns: turns}}
}

var cbLevels = map[string][]CBLevel{
	CBFamilyKV: {
		kvLevel("R130", 130), kvLevel("R260", 260), kvLevel("R520", 520), kvLevel("R1000", 1000),
		kvLevel("R2000", 2000), kvLevel("R4000", 4000), kvLevel("R8000", 8000), kvLevel("R16000", 16000),
	},
	CBFamilyLog: {
		logLevel("L160", 160, 14), logLevel("L320", 320, 14), logLevel("L640", 640, 14), logLevel("L1600", 1600, 14),
		logLevel("L3200", 3200, 27), logLevel("L6400", 6400, 54), logLevel("L12800", 12800, 54),
	},
	CBFamilyNeedle: {
		needleLevel("K4-C4", 4, 4), needleLevel("K4-C6", 4, 6), needleLevel("K4-C8", 4, 8), needleLevel("K4-C24", 4, 24),
		needleLevel("K8-C48", 8, 48), needleLevel("K8-C60", 8, 60), needleLevel("K8-C62", 8, 62),
		needleLevel("K4-C96", 4, 96), needleLevel("K2-C192", 2, 192), needleLevel("K2-C224", 2, 224),
	},
	CBFamilySudoku: {
		sudokuLevel("t4", 4), sudokuLevel("t8", 8), sudokuLevel("t15", 15), sudokuLevel("t30", 30),
		sudokuLevel("t60", 60), sudokuLevel("t100", 100), sudokuLevel("t150", 150),
	},
}

// cbDefaultLevel is the smallest published level of each family whose uncompacted
// transcript exceeds the usable budget of record (budget minus reserve), i.e. where the
// context has to be managed at all. The transcript is the instruction, every operation and,
// for sudoku_sketchpad, the whole board panel the task makes the agent print after each
// move: the sudoku stream alone is small (12k estimated tokens at t150) and never overflows,
// the panels do, from t60 (about 40k) on; t30 is about 21k.
var cbDefaultLevel = map[string]string{
	CBFamilyKV: "R1000", CBFamilyLog: "L1600", CBFamilyNeedle: "K4-C24", CBFamilySudoku: "t60",
}

// CBLevelNames lists a family's level names in table order.
func CBLevelNames(family string) []string {
	var names []string
	for _, l := range cbLevels[family] {
		names = append(names, l.Name)
	}
	return names
}

// CBLookupLevel finds a level by family and name; an empty name selects the default.
func CBLookupLevel(family, name string) (CBLevel, error) {
	levels, ok := cbLevels[family]
	if !ok {
		return CBLevel{}, fmt.Errorf("contextbench: unknown family %q (known: %s)", family, strings.Join(CBFamilies, ", "))
	}
	if name == "" {
		name = cbDefaultLevel[family]
	}
	for _, l := range levels {
		if l.Name == name {
			return l, nil
		}
	}
	return CBLevel{}, fmt.Errorf("contextbench: family %s has no level %q (known: %s)", family, name, strings.Join(CBLevelNames(family), ", "))
}

// GenerateCB builds one instance of a family at a level for a seed.
func GenerateCB(family, level string, seed int64) (*CBInstance, error) {
	l, err := CBLookupLevel(family, level)
	if err != nil {
		return nil, err
	}
	if seed < 0 {
		return nil, fmt.Errorf("contextbench: seed must be non-negative, got %d", seed)
	}
	var in *CBInstance
	switch family {
	case CBFamilyKV:
		in = GenerateKV(seed, l.Knobs)
	case CBFamilyLog:
		in = GenerateLog(seed, l.Knobs)
	case CBFamilyNeedle:
		in = GenerateNeedle(seed, l.Knobs)
	case CBFamilySudoku:
		in = GenerateSudoku(seed, l.Knobs)
	}
	in.Level = l.Name
	in.ID = fmt.Sprintf("%s-%s-seed%d", family, l.Name, seed)
	return in, nil
}
