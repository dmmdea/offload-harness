### Fixed - a GPU lease no longer cuts the calls in progress on the models it unloads, a call whose model was taken away is sent again, and "no speech" is only read from an answer

- **What happened.** `gpu reserve --drain --unload-seat` waited for the agent seat only and then unloaded every other resident
  model at once. On 2026-11-25 a grant ended four transcriptions one second later (`whisper-server 500: matrix: model unloaded`),
  and they were filed as failed although the same request would have loaded the model back. Nothing could have seen them either:
  whisper-server publishes no in-flight count, and the call was recorded nowhere outside its own process.
- **What changed.** A transcription and a forecast are marked in the run registry while they are on their engine
  (`gpu status` shows them). A lease names the models it is clearing, so new calls to them queue at the fence, and it unloads a
  model only when the engine's own count and the registry say nothing is running on it, the agent seat included. The wait is the lease's own
  queue budget; when it ends, the cards go back and nothing is cut. A render waits the same way for the marked calls (its own window,
  60 s at least) and answers busy instead of cutting one. The last transcription of a burst no longer unloads a model another
  process is transcribing on.
- **And when a model is taken away anyway** (another client's swap, a config reload, an unload by hand), the transcription, forecast,
  embedding, text and vision calls are sent again: at most twice, inside the call's own timeout, behind the same fence. The evidence is
  llama-swap's own words, or a 5xx with no body, which is what a call gets that the engine was already working on. A call under a contract
  keeps the contract's own rules.
- **Audio that has speech is no longer reported as silent.** A bare 5xx in a transcription that ran alone was read as "empty transcript
  (no speech detected)", because whisper-server died on audio without speech and that looks the same; on a real run a recording full of
  speech came back that way after an unload by another process. The verdict is now only ever an answer: 200 with an empty transcript.
- **Audio without speech no longer fails the call.** whisper-server detects the language a second time just to list its probabilities, which nothing
  here reads, and dies of it when there is no speech to detect one in: a 500 `basic_string: construction from null is not valid`, or
  its process ends. The request now asks for no such list (`no_language_probabilities=true`), so such a clip is answered as what it is.
- **Details.** [ADR 0104](docs/architecture/decisions/0104-a-lease-waits-for-the-calls-it-would-cut-and-a-cut-call-is-sent-again.md),
  `docs/systems/gpu-lease.md`, `docs/systems/offload-pipeline.md`.
