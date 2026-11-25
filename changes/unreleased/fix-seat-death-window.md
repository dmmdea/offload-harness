### Fixed - a run whose engine dies under it waits for the engine again: the engine is asked, and followed until it is gone

- **What was wrong.** A run survives the death of its engine (ADR 0066): the failed step waits for the seat and is sent again. The
  death is confirmed against llama-swap first, because a 5xx alone is no proof, and that confirmation was ONE reading, taken the
  instant the call failed. vLLM's API server is another process than its engine: when the engine dies it answers every open and new
  request with an error in the same second, is told to stop by a watchdog that looks every 5 s, and is gone only once it has shut
  down. Read in that gap the seat is listed and readable, so the failure stood as an ordinary infrastructure error. On 2026-11-25 the
  agent seat of the reference 3-card box died (a worker hung, the engine's 120 s timeout) and five contracts ended that way in one
  second (`engine error in stream: The server had an error while processing your request`, one of them 127 ms after it had
  started). The next request started the engine again three minutes later.
- **What it does now.** When the seat still reads ready after such a failure, the engine itself is asked (`GET /health` at the seat's
  own address). vLLM answers 503 from the moment its engine is dead, before it errors the open requests; llama-server has the same
  door. 200, or no such door, and the failure is an ordinary error at once, as before. 503, or no answer, and the seat is followed for
  up to 20 s (`seatDeathWindow`): the first reading that shows it loading, absent or refusing connections is the death, and the run
  waits for the engine and sends the step again as ADR 0066 says.
- **What did not change.** What counts as a failure that may be a dead seat, the bound of the wait that follows, and the one re-issue
  a seat that came back earns.
- **Check.** A contract (`delegate --route local`) against an engine stand-in that dies the way vLLM did: an error frame on the open
  stream, listed, readable and answering 503 for 4 s more, then gone, started by the next request. Build 0.238.0 ended at once with the
  night's own sentence (`infrastructure: agent loop: engine error in stream: The server had an error while processing your request.`).
  This build asked four times, read the seat gone 4.0 s after the failure, sent the step again, waited 6.1 s for the engine and
  succeeded (`seat_recoveries` 1). The same stand-in erroring one request and going on working: 0.3 s to the ordinary error on both
  builds (20.4 s on the first version of this change). The existing table of `TestConfirmSeatDownReadsTheSeatAfterAFailedCall` gains
  the incident. Not seen on a live engine's death since: the seat's engine died once in four weeks, and killing it on purpose would end
  other sessions' runs.
