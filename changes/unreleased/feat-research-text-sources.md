### Added - `offload_research` reads RSS/Atom feeds and, when an operator names a yt-dlp, YouTube subtitles

- **What it does.** Two text sources join pages, both fetched delegator-side with no login and no cookies, and both end in the
  same digest path, `internal/untrusted` fencing and public-web guard a fetched page uses. A response whose content type is an
  RSS or Atom media type, or whose body sniffs as a feed, is parsed with `encoding/xml` (no new dependency): item titles, links,
  dates and summaries become the text, at most 50 items and 1,000 bytes of summary each, and item links are never followed. A
  `youtube.com/watch`, `youtu.be` or `youtube.com/shorts` URL goes to the yt-dlp named by the new config key `ytdlp_path`, run
  once with subtitles-only flags (`--skip-download --write-subs --write-auto-subs --sub-langs=<sub_langs, default en.*>
  --sub-format=vtt/srv3/best --no-cookies --no-cookies-from-browser --ignore-config --no-playlist --no-cache-dir
  --socket-timeout 15`, zero retries) in a per-call directory under the state root (else the OS temp dir) that is removed
  afterwards, with a scrubbed environment, bounded by the call's deadline. WebVTT, SRV3 and SubRip are converted to plain text
  with timestamps, cue settings, tags and the rolling duplicate lines of automatic captions removed.
- **Off by default.** `ytdlp_path` is empty by default, which leaves the adapter off and a YouTube URL an ordinary page. The
  harness never searches `PATH` for the binary and never installs it; a relative path is refused. `offload_research` gains an
  optional `sub_langs` input; its sources rows gain `error_kind`. On Windows `ytdlp_path` must name an `.exe` or a wrapper `.cmd`
  (the admitted zipapp is a shebang script that runs directly only on POSIX). A feed with no item or entry, and an RDF document
  that does not declare the RSS 1.0 namespace, are read as the ordinary document they were before.
- **A site break is a typed error, never a retry.** A non-zero exit is `subtitle_site_break`, an exit with no file is
  `no_subtitles`, a subtitle file of blank cues is `empty_subtitles`, and `ytdlp_path`, `bad_sub_langs`,
  `unsupported_subtitle_format` and `subtitle_timeout` name the rest; the
  batch keeps its shape and the other URLs are untouched.
- **Security.** Every URL still passes `research.ValidateURL` before anything is fetched or run, and the canonical
  `https://www.youtube.com/watch?v=<id>` handed to the binary is validated as well: a hostname that resolves to a private,
  loopback, link-local or CGNAT address is refused before any process starts. The matcher is exact (a lookalike host, a userinfo
  trick or another port is an ordinary page), the adapters build no HTTP client, and yt-dlp's own connection is the one thing
  outside `netguard.PublicTransport`, which is stated in `docs/systems/mcp-server.md` and is why the adapter is off until
  configured. Audio transcription is not part of this change. `docs/systems/security.md` gains the yt-dlp row in "Admitted
  third-party software" (tag 2026.08.19, the zipapp asset, its sha256, Unlicense).
- **Tests.** Red first (the failing tests were committed before the code): a fake yt-dlp (the test binary playing the part, no
  network and no real yt-dlp anywhere) returns a subtitle digest; rolling-caption dedupe; RSS 2.0 and Atom fixtures, sniffing and
  the item cap; the SSRF refusal with a hostname resolving private through the `netguard.LookupIP` seam, for both the written
  and the canonical host; lookalike hosts never reach the binary and the adapters add no HTTP client; the adapter is off with an
  empty path; a site break gives a typed error with the fake invoked exactly once; the child environment is scrubbed; and the
  digest input of subtitle text equals that of the same text fetched as a page (the context document `Build` hands the seat; no
  model is called). Thirteen one-line mutants (the canonical-host check, `--no-cookies`, the dedupe, a suffix host match, the
  item cap, the off switch, the scratch cleanup, the no-file error, a retry on failure, the path search, the language check, the
  feed sniff and the environment scrub) were each red on the suite; the path search, the feed sniff and the environment scrub survived the first test set and each got a test that kills it.
