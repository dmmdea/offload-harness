// contextbench_instr.go - what the agent is told, per family.
//
// The upstream benchmark ships its instructions as markdown scaffolds. Those are
// prose, not logic, and are not carried into this repository; these short texts are
// written for the adapter and state the same contracts: the shape of an answer block,
// the sentinels, and the standalone readiness marker. Because the wording differs,
// scores from this adapter compare harness configurations with each other, not with
// published numbers.
//
// The texts deliberately contain no example that a grader could mistake for an
// answer: placeholders only, never a real key, query number or board tuple.
package compeval

import "strings"

const cbCommonProtocol = `Operations reach you one at a time, each in its own turn. Handle the operation you were just given, then end your reply with ` +
	CBAdvanceMarker + ` on a line of its own and the next operation will follow. The marker only counts when it is alone on its line.
After the last operation your reply ends the session; the marker is not needed then.
Everything you need later must still be in this conversation when you need it: only what is in your context is graded, never anything you write elsewhere.`

var cbFamilyInstruction = map[string]string{
	CBFamilyKV: `Key-value memory task.
Operations are either a SET batch (many lines of the form "SET <key> = <value>", wrapped in BEGIN/END sentinel lines) or a "GET <key>" request.
Every SET arrives before any GET, and you are not told which keys will be asked about, so any value you have seen may be needed.
Answer a GET with an answer block carrying the stored value verbatim:
<<<ANSWER key=<the key>>>
<the stored value>
<<<ANSWER END>>>`,
	CBFamilyLog: `Incident log triage task.
Operations are either a LOG-BATCH (service log lines of the form "<timestamp> [LEVEL] <service> req=<id> <message> #<tag>", wrapped in BEGIN/END sentinel lines) or a "QUERY <n>: <question>".
A question either asks for the exact message of the line with a given req id, or asks how many lines match a level and a service.
You are not told the questions in advance, so you must be able to consult the whole log when a QUERY arrives.
Answer a QUERY with an answer block carrying either the exact message text or the integer count and nothing else:
<<<ANSWER qid=<the query number>>>
<your answer>
<<<ANSWER END>>>`,
	CBFamilyNeedle: `Needle retention task.
Each operation is a chunk that opens with a few "needle" lines (each starts with a tag in square brackets such as "[n...#...]") followed by a long filler block wrapped in START/END sentinel lines.
By the end, every needle line from every chunk must still be present, verbatim and on its own line, in your context window. The filler is worthless and only takes room.
There is no answer to write: what is graded is what your context still holds when the session ends.`,
	CBFamilySudoku: `Sudoku sketchpad task.
You maintain a sudoku board as a panel in your context: one "(row,col,value)" tuple per cell, row-major, between the lines <<<SKETCHPAD BEGIN>>> and <<<SKETCHPAD END>>>, with a "VERSION: <n>" line just under the begin line.
A value is a symbol (1-9, then A, B, ... for larger boards) or "." for an empty cell. Rows and columns count from 1, from the top and the left.
Each operation is one move: place a symbol in a cell. Apply it by changing that one tuple and raising the version to the number the move states; leave every other tuple unchanged.
After every move, write the whole updated panel in your reply between the two sentinel lines. When a NEW BOARD operation arrives, replace your panel with the one it carries.
Your starting panel (version 0):
{initial}`,
}

// CBInstruction is the system text for one instance. delivery is "tool_result" or
// "user" and only changes one sentence: where the operations come from.
func CBInstruction(in *CBInstance, delivery string) string {
	body := cbFamilyInstruction[in.Family]
	if in.Family == CBFamilySudoku {
		body = strings.Replace(body, "{initial}", in.Gold.InitialGrid, 1)
	}
	var from string
	if delivery == CBDeliveryUser {
		from = "Each operation arrives as a user message."
	} else {
		from = "Each operation arrives as the result of a call to the next_op tool, which the harness makes for you; you never need to call it yourself."
	}
	return "You are working through a long stream of operations that does not fit in your context window.\n" + from + "\n\n" +
		body + "\n\n" + cbCommonProtocol
}
