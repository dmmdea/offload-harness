---
status: Accepted
date: "2026-11-25"
---

# 0104 — A lease waits for the calls it would cut, and a cut call is sent again

## Context

`gpu reserve --drain --unload-seat` waited for ONE model, the agent seat, and then unloaded every other model llama-swap held, with
no look at any of them. On 2026-11-25 a lease was granted at 13:27:02 while four transcriptions were running, and at 13:27:03 all
four were answered `whisper-server 500: matrix: model unloaded`. They were filed as failed calls. The audio was on disk, and the same
request would have loaded the model back.

Four things were missing or wrong:

1. **Nothing read the other models' calls in flight** before they were unloaded. The drain (ADR 0041) was written for the agent seat;
   the unload was later widened to every resident model (the card has to be clear) and the drain was not.
2. **Nothing could have seen a transcription.** whisper-server and the forecast server publish no in-flight gauge (neither `/metrics`
   nor `/slots`), and the call was recorded nowhere outside the process that sent it. A render's own drain
   (`render/gpu-lock.mjs`) is blind in the same way, and so was the unload after the last transcription of a burst in another process.
3. **The clients read a model that was taken away as their own failure.** The agent loop re-issues such a step (ADR 0066). The
   transcription, forecast, text, vision and embedding calls did not. What such a call is told depends on where it stood: one that
   llama-swap still held gets its words (`matrix: model unloaded`), one the engine was working on gets a 502 with no body, 10 s later
   (both measured on llama-swap v273, a speech and a text model unloaded by hand).
4. **A lost answer was read as silence.** A bare 5xx in a transcription that had run alone in its process was the verdict "no speech",
   because whisper-server dies on audio without speech (F-35) and llama-swap's answer to that is the same bare 5xx. On a real run a
   recording full of speech whose model another process had unloaded was reported as having none.

## Decision

1. **A call on an engine without a gauge marks itself** in the machine-wide run registry for exactly as long as the request is on
   the engine (`modelaffinity.Call`; kinds `transcription` and `forecast`). The order is the protocol: wait at the fence, take the
   engine's slot, mark, look at the fence once more, send. A holder names the models it is clearing and then reads the marks.
   Whichever comes second sees the other, so a call is waited for or goes back to the fence.
2. **A lease names the models it is clearing** (`clearing` on the record: ids and aliases). A request for a resident model passes the
   fence, because serving it loads nothing. A model on this list does not pass: new calls queue, the ones that are running finish,
   and the model becomes idle instead of being kept busy for ever.
3. **A reservation waits for every model it is about to unload** (`internal/seatdrain`), inside its own queue budget: while it is
   only draining, for the runs registered on the other resident seats (whole-node form; a card-scoped drain already reads every run
   on its cards); then, behind the fence, for the engines' own gauges and the marks, the agent seat's included, because its own drain
   ended before the fence went up. When the deadline passes, or nothing has changed for longer than any call may last, the fence and
   the list come off, nothing is unloaded and the caller gives the cards back. A gauge that cannot be read is retried and then named
   and not waited for.
4. **A render waits for the marks** before the first unload under its lease (`awaitCallsOn`): until its own window ends, 60 s at least.
   A call still running then is not cut: the render gives the cards back with the busy answer, or with its place in line when its
   door can keep one. The engines that have a gauge stay with the runner's own drain, as before.
5. **A call whose model was taken away is sent again**, at most twice and inside its own timeout (`seatwait.TakenAway`,
   `seatwait.Resend`): on llama-swap's own words (`<router>: model unloaded`, `<router> is shutting down`), on a 5xx with no body, and,
   for a transcription and a forecast, on an answer that was cut off mid-read. It waits at the fence like a new call. The transcription,
   forecast, embedding, text and vision clients do this. A call made under a contract keeps the contract's own rules (ADR 0066): there a
   lost answer is checked against the seat first.
6. **"No speech" is only ever read from an answer** (200, an empty transcript). The request asks whisper-server for no language
   probabilities (`no_language_probabilities=true`), because computing them is what it died of: with `verbose_json` it detects the
   language a second time after the decode, and when its VAD left no audio to detect one in, the detection fails, the name of the
   language is a null pointer, and building a string from it throws (a 500 that says `basic_string: construction from null is not
   valid`) or, on a runtime that does not check, ends the process (F-35). Nothing here reads those probabilities. Measured on 1.9.5: a tone,
   digital silence and hiss answered 500 before and 200 with an empty text after; two minutes of speech gave the same 51 segments and
   2,448 characters either way.

## Alternatives considered

- **Only send the call again.** It would have saved the four calls, at the price of decoding each of them twice, on every grant.
- **Only wait.** The wait sees the harness's own calls. Another client's swap, a config reload and an unload by hand still take a
  model away.
- **Send a bare 5xx again only while a lease holds the model's card**, and keep reading the others as F-35. That was this change's first
  version, and its own real run refuted it: an unload by hand has no lease, and the recording came back as silent. Nothing on the
  asking side can tell the two apart, so what made whisper-server die had to go.
- **Read whisper-server's load some other way** (GPU utilisation, llama-swap's log, open connections). Each is an inference about
  somebody else's process. The process that sent the call knows.
- **Unload at the deadline anyway**, as the render's drain does after 60 s. A reservation is asked for with a budget of hours, and
  cutting work in flight is what `--drain` is given to prevent.
- **Mark the call from its first line, as agent runs do.** A call that waits at the fence for this very lease would be waited for by
  it: a deadlock until one of the two budgets runs out.

## Consequences

- A grant can take longer: up to the longest call on the cards. It says what it waits for, once per change.
- A process of an older build marks nothing and reads no `clearing`; its transcriptions are cut as before, until it is updated.
- A client that calls llama-swap directly is seen through the engine's gauge where there is one, and not at all on whisper-server.
- A transcription whose audio is long can now end as a capacity defer, `gpu busy`, instead of as a failure, when the lease that took
  its model outlasts its wait for the card.
- An engine that crashes on one particular input is asked for it up to three times, inside the call's own time, before the call fails.
- A whisper-server build that does not know the field ignores it and still dies on audio without speech. The call then fails as
  `the upstream vanished mid-request` after its re-sends, where it used to say "no speech": a wrong word about good audio is worse than a
  failure about silent audio.
- Not changed: the batch that joins a resident model without reading the lease (ADR 0025), the display watchdog's, the updater's and
  the upscaler's own unloads, and a command under `gpu reserve` that renders without `--unload-seat` (the runner's drain is its only wait).
