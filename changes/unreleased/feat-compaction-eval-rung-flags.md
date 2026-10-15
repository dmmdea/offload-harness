### Added - compaction-eval can switch on the search-results and log-collapse rungs

- **The verb can now measure the two rungs.** `compaction-eval run`, `freeze`, `ab` and `kvbench` take `--search-results` and
  `--log-collapse`, added to `--gcf --skeleton`. They flow through `agent.ReplayOpts` and `compeval.LadderOpts` into the same
  production ladder every other flag reaches, and the report's `ladder` names the arm (`gcf+skeleton+search`,
  `gcf+skeleton+logs`, `gcf+skeleton+search+logs`). Until now neither rung could be replayed, so neither could be judged by the
  measurement gate.
- **No default or production path changes.** The rungs stay off in the live loop, `compact()` is untouched, and the pinned
  default-ladder golden is unchanged. Without a rung flag, `compaction-eval run` prints the same bytes as before; two golden
  outputs recorded from the verb before the change pin that.
- **`kvbench --arms on`** runs only the ladder arm and skips the raw arm, which a candidate-versus-incumbent comparison does not
  need (an unknown or repeated arm is refused; with one arm there is nothing to pair, so the paired totals stay empty).
- **`kvbench`** samples now carry **`sent_sha256`**, the sha256 of the transcript the ladder produced, taken before the nonce salt
  (its canonical encoding, not the HTTP body). It does not depend on the run's nonce, so two runs with their own fresh nonces
  prove a pair a no-op when the hashes of a step are equal, and a different hash proves the rung rewrote the request.
  `--nonce N` sets the body salt; reuse it only on a cold seat, since a warm seat serves the second run's unchanged prompts from
  the first run's cache. The calibration controls draw their own nonce every run (`control_nonce` in the report), so a reused
  `--nonce` no longer replays the first run's negative control into the cache and fails the second run.
- Each behaviour has a test that failed first: the replay options (not passed to the ladder), the labels, the rung
  turning on in `run`, `ab` and `kvbench` on transcripts whose older turns hold a search listing or a repetitive build log, the
  other rung's entry staying unchanged, `--arms on`, a no-op pair with equal `sent_sha256` and a changed pair with a different
  one, one body nonce and one control nonce across the requests of a run, the same `sent_sha256` for one transcript under two nonces,
  and a second run on a multi-prompt-cache seat that reuses the first run's nonce staying admissible.

### Not verified here

- No model-backed run: `ab` was exercised with a fake scorer and `kvbench` with a fake serving endpoint, so no accuracy or
  forwarded-token result for either rung exists yet. That needs a corpus whose older turns carry search listings and build logs,
  and a seat.
- `ab` was not driven through the verb itself (it opens the live pipeline); its flag reaches `RunAB` through the same options
  value `run` and `kvbench` use, and `RunAB` is tested with both rungs.
