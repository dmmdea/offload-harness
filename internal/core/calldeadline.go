package core

// CallDeadlineReasonPrefix opens the reason of every subtask a delegation call's
// whole-call deadline cut (ADR 0065, register C-67): "call deadline reached; N
// unfinished — this subtask ...". It is a STABLE key: the delegator publishes it on
// the wire, the ledger and the corpus, and readers that must not mistake such a row
// for a seat's own failure (the rigger, a per-call wall readback) match on it. One
// constant, so no reader keeps a private copy of the words.
const CallDeadlineReasonPrefix = "call deadline reached"
