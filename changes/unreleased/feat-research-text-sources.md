### Added - `offload_research` reads RSS/Atom feeds and, when an operator names a yt-dlp, YouTube subtitles

- **What it does.** Two text sources join pages, both fetched delegator-side with no login and no cookies, and both end in the
  same digest path, `internal/untrusted` fencing and public-web guard a fetched page uses. A response whose content type is an
  RSS or Atom media type, or whose body sniffs as a feed, is parsed with `encoding/xml`: item titles, links, dates and summaries
  become the text, at most 50 items and 1,000 bytes of summary each, and item links are never followed (a relative link is shown
  resolved against the feed's `xml:base`, else the address it was fetched from). A YouTube video URL (`watch?v=`, `/shorts/`,
  `/live/` and `/embed/` on `youtube.com`, `www.`, `m.` and `music.youtube.com`, `/embed/` on `youtube-nocookie.com`, and
  `youtu.be/<id>`) goes to the yt-dlp named by the new config key `ytdlp_path`, run once on the canonical
  `https://www.youtube.com/watch?v=<id>` with subtitles-only flags (`--skip-download --write-subs --write-auto-subs
  --sub-langs=<sub_langs, default en.*> --sub-format=vtt/srv3/best --no-cookies --no-cookies-from-browser --ignore-config
  --no-playlist --no-cache-dir --socket-timeout 15`, zero retries, warnings left on) in a per-call directory under the state
  root (else the OS temp dir), with a scrubbed environment, bounded by the call's deadline. WebVTT, SRV3 and SubRip are converted
  to plain text with timestamps, cue settings, tags and the rolling duplicate lines of automatic captions removed.
- **Off by default, and how far the path is trusted.** `ytdlp_path` is empty by default, which leaves the adapter off and a
  YouTube URL an ordinary page. The harness never searches `PATH` for the binary and never installs it; a relative path is
  refused. `ytdlp_path` is trusted as written: nothing checks the file unless the optional `ytdlp_sha256` is set, and then the
  file is hashed before every run and a mismatch refuses to run it (`ytdlp_path` error, nothing started). On Windows the file
  must be an `.exe` or a wrapper `.cmd` (the admitted zipapp is a shebang script that runs directly only on POSIX), and with a
  wrapper the hash covers the wrapper, not the interpreter or the script it calls. `offload_research` gains an optional
  `sub_langs` input (letters, digits and `. * _ - ,` only, so no `cmd.exe` metacharacter reaches a wrapper); its sources rows
  gain `error_kind` and `cleanup_error`.
- **A deadline ends the whole process tree.** The documented Windows shape is a wrapper `.cmd` that runs the interpreter, so the
  program that holds the per-call directory is a grandchild, and killing only the wrapper left it running with the directory as
  its working directory. A deadline or a cancellation now ends the tree (`taskkill /T /F`, started hidden, on Windows; `SIGKILL`
  to the process group on POSIX, through the new `winexec.KillTreeOnCancel`), the child is started through `winexec` so it opens
  no console window, one run is capped at 15 minutes, and a binary that exits 0 while a child still holds the output pipe is read
  as the success it is. The directory is removed with a bounded retry of a few hundred milliseconds; a removal that still fails
  is recorded as `cleanup_error` on the source row instead of being dropped, and each call first sweeps `ytdlp-*` directories
  directly under its own scratch root that are older than any run can be. A child whose parent had already exited when the run
  ended stays out of reach of the Windows tree walk; its hold shows as `cleanup_error` until the sweep takes the directory.
- **Feeds are read as they are served.** The text is decoded as the response's `charset` parameter names, else the XML
  declaration, else UTF-8: windows-1252 and cp1252 feeds (and iso-8859-1 and latin1, read as windows-1252 the way browsers do)
  keep their curly quotes, euro signs and dashes instead of turning into C1 control characters. An invalid UTF-8 byte or a
  character XML forbids no longer ends the parse at that item with every later item lost. A document that stops before its end
  says so first in its header, is marked `truncated`, and does not count the item it stopped in ("Items: 2 of 3 items shown" for
  four items is gone). A feed with no item or entry, and an RDF document that does not declare the RSS 1.0 namespace, are read
  as the ordinary document they were before. `golang.org/x/text` moves from an indirect to a direct requirement for the decoder
  (`go.sum` is unchanged).
- **A site break is a typed error, never a retry.** A non-zero exit is `subtitle_site_break`; an exit with no file is
  `no_subtitles`, and its reason ends with the last line yt-dlp printed, because it reports a refused subtitle download (an HTTP
  429 rate limit) as a warning and exits 0; a subtitle file of blank cues is `empty_subtitles`; a canonical URL that fails the
  public-web guard is `url_refused`; and `ytdlp_path`, `bad_sub_langs`, `unsupported_subtitle_format` and `subtitle_timeout`
  name the rest. A SRV3 track that ends inside a cue (cut by the read bound or interrupted) keeps the cues read before the cut
  and is marked `truncated`. The batch keeps its shape and the other URLs are untouched.
- **Security.** Every URL still passes `research.ValidateURL` before anything is fetched or run, and the canonical
  `https://www.youtube.com/watch?v=<id>` handed to the binary is validated as well: a hostname that resolves to a private,
  loopback, link-local or CGNAT address is refused before any process starts. The matcher is exact (a lookalike host, a userinfo
  trick, a trailing-dot name, look-alike letters, an encoded backslash or another port is an ordinary page), the adapters build
  no HTTP client, and after that pre-flight name check yt-dlp resolves and dials on its own, outside `netguard.PublicTransport`,
  which is stated in `docs/systems/mcp-server.md` and is why the adapter is off until configured. Audio transcription is not part
  of this change. `docs/systems/security.md` gains the yt-dlp rows in "Admitted third-party software": the zipapp (tag
  2026.08.19, its sha256, Unlicense) and the Windows `.exe` or wrapper shape, whose hash the operator pins in `ytdlp_sha256`.
- **Tests.** Red first (the failing tests were committed before the code, and the process-tree tests were also run red on Linux):
  a fake yt-dlp (the test binary playing the part, no network and no real yt-dlp anywhere) returns a subtitle digest;
  rolling-caption dedupe; RSS 2.0 and Atom fixtures, sniffing and the item cap; the SSRF refusal with a hostname resolving
  private through the `netguard.LookupIP` seam, for both the written and the canonical host; lookalike hosts never reach the
  binary and the adapters add no HTTP client; the adapter is off with an empty path; a site break gives a typed error with the
  fake invoked exactly once; the child environment is scrubbed; the digest input of subtitle text equals that of the same text
  fetched as a page (no model is called); a wrapper script that leaves a grandchild running is ended with the tree and the
  scratch directory goes with it (a `.cmd` on Windows, a shell script on POSIX); stale scratch directories are swept; a removal
  that fails is retried and then recorded; a hash mismatch or a malformed `ytdlp_sha256` starts nothing; `sub_langs` refuses
  every `cmd.exe` metacharacter; and the feed cases above (cp1252 bytes 0x80, 0x93, 0x94 and 0x96, a stray byte, a body cut
  mid-item, relative links with `xml:base`). Each fix was checked by one-line mutants that were red on the suite and restored
  byte for byte, among them the tree kill, the wait-delay success, the sweep's name, age and file-type checks, the retry, the
  hash comparison, the charset, the byte repair, the cut header and the new URL shapes; a repair that `bytes.Map` duplicated and a
  format check the mismatch path duplicated each survived the first tests and got tests that kill them.
