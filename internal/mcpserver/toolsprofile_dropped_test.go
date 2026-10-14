package mcpserver

// compactDropped is the reviewed list of constraint-word sentences the compact profile drops, with the reason each is safe to drop.
// TestEveryConstraintSentenceIsKeptOrDroppedWithReason is the only reader.
var compactDropped = []droppedSentence{}
