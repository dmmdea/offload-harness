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
- **`kvbench --nonce N`** gives two runs the same salt, and every sample now carries **`sent_sha256`**, the sha256 of the bytes it
  sent after the salt. Two runs that share a nonce and a ladder send equal bytes on a step the rung leaves alone, so equal
  hashes prove that pair a no-op, and a different hash proves the rung rewrote the request.
- Each behaviour has a test that failed first: the replay options (not passed to the ladder), the labels, the rung
  turning on in `run`, `ab` and `kvbench` on transcripts whose older turns hold a search listing or a repetitive build log, the
  other rung's entry staying unchanged, `--arms on`, a no-op pair with equal `sent_sha256` and a changed pair with a different
  one, and one nonce across every request of a run (that last test pins behaviour that already held).

### Not verified here

- No model-backed run: `ab` was exercised with a fake scorer and `kvbench` with a fake serving endpoint, so no accuracy or
  forwarded-token result for either rung exists yet. That needs a corpus whose older turns carry search listings and build logs,
  and a seat.
- `ab` was not driven through the verb itself (it opens the live pipeline); its flag reaches `RunAB` through the same options
  value `run` and `kvbench` use, and `RunAB` is tested with both rungs.
