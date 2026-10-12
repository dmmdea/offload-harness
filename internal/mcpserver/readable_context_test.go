package mcpserver

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/research"
)

// TestResearchHandsPagesOverInLinesTheSeatsReadFileCanReturn: the research door wraps a page's lines at
// research.MaxLineChars, and the agent's read_file cuts a line at agent.ReadFileLineCap and cannot return the rest.
// The first must sit under the second with room for the line number and the tool's framing, or the wrap protects nothing.
func TestResearchHandsPagesOverInLinesTheSeatsReadFileCanReturn(t *testing.T) {
	if research.MaxLineChars+100 > agent.ReadFileLineCap {
		t.Fatalf("research.MaxLineChars = %d, agent.ReadFileLineCap = %d: a wrapped line must fit read_file with room to spare", research.MaxLineChars, agent.ReadFileLineCap)
	}
}
