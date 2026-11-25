### Fixed - a run whose engine dies under it waits for the engine again: the seat is followed after the failure, not read once

- **What was wrong.** A run survives the death of its engine (ADR 0066): the failed step waits for the seat and is sent again. The
  death is confirmed against llama-swap first, because a 5xx alone is no proof, and that confirmation was ONE reading, taken the
  instant the call failed. vLLM's API server is another process than its engine: when the engine dies it answers every open and new
  request with an error in the same second, is told to stop by a watchdog that looks every 5 s, and is gone only once it has shut
  down. Read in that gap the seat is listed and readable, so the failure stood as an ordinary infrastructure error. On 2026-11-25 the
  agent seat of the reference 3-card box died (a worker hung, the engine's 120 s timeout) and five contracts ended that way in one
  second (`engine error in stream: The server had an error while processing your request`, one of them 127 ms after it had
  started). The next request started the engine again three minutes later.
- **What it does now.** After a failure that looks like a dead seat, the seat is followed for 20 s (`seatDeathWindow`, four
  cold-load polls): the first reading that shows it loading, absent or refusing connections is the death, and the run waits for the
  engine and sends the step again as ADR 0066 says. The following ends early when the engine's counters move, because an engine that
  worked after the failure is alive, and when the run ends. A seat that still reads ready at the window's end keeps the failure an
  ordinary error, as before; a seat that is unreadable is read again instead of being given up on at the first try.
- **What did not change.** What counts as a failure that may be a dead seat, the bound of the wait that follows, and the one re-issue
  a seat that came back earns. An ordinary 5xx from an idle, healthy seat now takes up to 20 s longer to be published.
- **Check.** A contract (`delegate --route local`) against an engine stand-in that dies the way vLLM did: an error frame on the open
  stream, listed and readable for 4 s more, then gone, started by the next request. Build 0.238.0 ended at once with the night's own
  sentence (`infrastructure: agent loop: engine error in stream: The server had an error while processing your request.`). This build
  read the seat gone at 4.1 s, sent the step again, waited 6.1 s for the engine and succeeded (`seat_recoveries` 1). The existing
  table of `TestConfirmSeatDownReadsTheSeatAfterAFailedCall` gains the incident. Not seen on a live engine's death since: the seat's
  engine died once in four weeks, and killing it on purpose would end other sessions' runs.
