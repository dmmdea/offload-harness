# 08 — Windows quoting (PowerShell 5.1 / pwsh 7 / cmd / Git Bash), exit codes, progress

Measured 2026-09-01 on the workstation with ffmpeg 8.1.2. Method: the same 16 filtergraph variants
were run from each shell; a Python `argv.py` echoed exactly what a native exe receives, ffmpeg
wrote one PNG, and a pixel diff against the plain frame proved whether text was drawn.
Nothing here is theory.

## The three escaping levels [doc, ffmpeg-filters §4.2 + ffmpeg-utils §2.1]
1. Option value: `:` and `\` `'` are special → `\:`; or quote the value in single quotes `'…'` (everything literal, but `'` itself cannot appear inside).
2. Filtergraph: `[ ] , ;` and `\ '` are special → `\,` `\;` etc.
3. Shell: whatever the shell does to `\ ' " $ % ^`.
The doc's own advice: avoid it — use `textfile=` and `-filter_script`.

## What reached ffmpeg, per shell, and what happened [measured]

| # | Written in the shell | bash | PS 5.1 | pwsh 7.6 | cmd | Result |
|---|---|---|---|---|---|---|
| 1 | `"drawtext=fontfile=C\:/Windows/Fonts/arial.ttf:text='Hi'…"` | `C\:/` | same | same | same | **parse error** in all: "No option name near '/Windows/Fonts/…'" |
| 2 | `"drawtext=fontfile='C\:/Windows/Fonts/arial.ttf':text='Hi'…"` | `'C\:/'` | same | same | same | **drawn** everywhere ← the portable form |
| 3 | `"drawtext=fontfile=C\\:/…"` (bash needs `C\\\\:`) | `C\\:/` | same | same | same | drawn everywhere |
| 4 | `"drawtext=fontfile=/Windows/Fonts/arial.ttf…"` | `/Windows/…` | same | same | same | drawn (cwd on C:); bash did NOT rewrite this one (no path-looking prefix) |
| 5 | `"drawtext=font=Arial…"` | | | | | Fontconfig error + **segfault** (139 / −1073741819) in every shell |
| 6 | `"drawtext=fontfile='C\:\\Windows\\Fonts\\arial.ttf'…"` | `'C\:\\Windows\\…'` | same | same | same | drawn (backslash path, escaped) |
| 7 | `-filter_script:v vf.txt` | file content verbatim | | | | parses; drawn only when the text has no `%` (07) |
| 9 | `"subtitles='C\:/full/path/test.srt'"` | | | | | parses everywhere; drawn when timing is right (07) |
| 10 | `"subtitles=C\\:/full/path/test.srt"` | **`C\\;C:\Program Files\Git\Users\…`** (MSYS mangled) → "No such filter" | ok | ok | ok | Git Bash trap |
| 11 | `"subtitles=/Users/<user>/…/test.srt"` | **`C:/Program Files/Git/Users/…`** (MSYS mangled) → "Unable to parse original_size" | ok | ok | ok | Git Bash trap |
| 12 | `'…text="Hi there"…'` | `"Hi there"` | **`Hi there`** (5.1 strips inner double quotes) | `"Hi there"` | `"Hi there"` (with `\"`) | drawn in all, but the quotes are part of the text except in PS 5.1 |
| 14 | `"…text='%{pts\:hms}'…"` | ok | ok | ok | cmd needs `%%{pts\:hms}` in a .cmd file | drawn |
| 16 | `'drawtext=fontfile=C\\Windows\\Fonts\\arial.ttf…'` (unescaped colon) | | segfault | segfault | | `C\\Windows` is not a path → fontconfig fallback → crash |

cmd column detail (full 17-variant run from a `.cmd` file via `cmd /c`, 2026-09-01, every line pixel-verified):
`fontfile='C\:/…'` drawn · `fontfile=C\:/…` unquoted parse error (−22) · drive-less `/Windows/Fonts/arial.ttf` drawn · `font=Arial` segfault · `fontfile='C\:\Windows\Fonts\arial.ttf'` with SINGLE backslashes segfault (cmd does not eat backslashes, ffmpeg treats `\W` as an escape → garbage path → fontconfig) · `-filter_script:v` drawn · `text='Hello\, world\: 100%% done':expansion=none` drawn (the `%%` becomes `%` in a batch file) · `subtitles='C\:/full/test.srt'` drawn · `subtitles=C\:/full/test.srt` unquoted → "Unable to parse original_size" (needs `C\\:` when unquoted) · drive-less `subtitles=/full/test.srt` drawn · `text=\"Hi there\"` drawn with the quotes · `textfile=…:expansion=none` drawn · `text='%%{pts\:hms}'` drawn · `-filter_complex "[0:v]…[v]" -map "[v]"` drawn · `text='50%%':expansion=none` drawn · `text='100%%'` (normal expansion) → "Stray %" and NOT drawn, rc 0.

PowerShell-specific bites [measured]:
- In a double-quoted PS string `$5` and `$name:` are variables: `"text='cost $5'"` reached ffmpeg as `cost ` and `"text=$t:fontsize=80"` was parsed as the scoped variable `$t:fontsize` (empty). Use **single-quoted** PS strings for filtergraphs, or `${t}`.
- PS 5.1 strips embedded `"` from arguments to native programs (variant 12); pwsh 7 (`$PSNativeCommandArgumentPassing=Windows`) passes them. Write text with single quotes inside the graph, never double.
- `$LASTEXITCODE` after `& ffmpeg … 2>&1 | Select-Object -First 2` was **−1** (5.1) or **0** (7.6) for a run that really exited 0/−22: `Select-Object -First N` stops the pipeline and kills ffmpeg. Capture with `2>$null` or `2>&1 | Out-String` (measured correct in both versions), or redirect stderr to a file.
- `-f null NUL` and `-passlogfile x` work from PS (files `x-0.log`, `x-0.log.mbtree`); `-f null -` also works.
- PowerShell 5.1 remote shells (laptop node/editing rig over SSH) have no `&&`; chain with `;` or `if ($LASTEXITCODE -eq 0) {…}`.

cmd-specific [measured]:
- In a `.cmd`/`.bat` file `%` must be doubled (`%%{pts\:hms}`, `100%%`); on the interactive prompt single `%` is fine. `^` is the escape character; `"` groups.
- `del /q file` and `2>nul` for cleanup/redirect; `-f null NUL`.
- Launch from Git Bash as `cmd //c "C:\full\path\script.cmd"` (`//c` survives MSYS conversion; a bare `q.cmd` was "not recognized"), or from PowerShell `cmd /c "C:\full\path\script.cmd"`.
- Do NOT pass ffmpeg argument strings through `call :label %*` subroutines or `set "ARGS=…"` macros: `%~1` de-quoting and `%` expansion mangle filtergraphs (two harness attempts failed exactly there). Write each ffmpeg line in full, or generate the .cmd from Python.
- An unquoted `"C:\Program Files\Python314\python.exe"` in a `set PY=` line runs `C:\Program` — quote the path in the `set` (`set "PY=…"`) and call it as `"%PY%"`.

Git Bash-specific [measured]:
- MSYS path conversion rewrites any argument that starts with `/` followed by a word, or contains `X\\:` shapes, into `C:/Program Files/Git/...` — filter values, not just paths. `MSYS_NO_PATHCONV=1 ffmpeg …` disables it for that command [community, standard MSYS2 mechanism]; measured alternative: use relative paths, `./file`, or `-filter_script`.
- `printf "file 'a.mp4'\n"` inside a double-quoted heredoc-less tool call broke this session's Bash tool twice ("unexpected EOF while looking for matching `'`"); write script files with a proper heredoc (`<<'EOF'`) or the Write tool and run `bash script.sh`.
- `/dev/null` exists in Git Bash; `-f null /dev/null` works, as does `-f null -`.
- Exit codes are truncated to 8 bits (below).

## Portable recipe (works in all four shells) [measured]
1. Font/subtitle paths: forward slashes, colon escaped, value single-quoted **inside** the graph: `fontfile='C\:/Windows/Fonts/arial.ttf'`, `subtitles='C\:/x/y.srt'` — in bash the `\:` must be written `\\:` inside double quotes, or use single quotes around the whole `-vf` argument.
2. Text: `text='…'` with `\,` `\:` `\;` escaped; any `%` → `expansion=none` or move to `textfile=`.
3. Anything complex → `-filter_script:v graph.txt` / `-filter_complex_script graph.txt` (file content is parsed at levels 1–2 only; UTF-8, no shell involved). Or generate the command from Python with `subprocess.run([...])` and a list argv — no shell parsing at all (this is how the pipeline should call ffmpeg).
4. Verify by pixel diff (02) — a bad path can produce a 0-diff frame with exit 0.

## Exit codes [measured, 8.1.2]

| Situation | PowerShell `$LASTEXITCODE` | Git Bash `$?` |
|---|---|---|
| success | 0 | 0 |
| missing input | −2 (ENOENT) | 127 |
| filtergraph parse error / bad option value | −22 (EINVAL) | 127 |
| `moov atom not found` (truncated mp4) | −1094995529 (AVERROR_INVALIDDATA) | 183 |
| "No such filter" | AVERROR_FILTER_NOT_FOUND | 8 |
| segfault (fontconfig) | −1073741819 (0xC0000005) | 139 |
| `-n` and the output exists ("File … already exists. Exiting.") | **0** | **0** |
| `-xerror`, `-err_detect explode` on the truncated file | same −1094995529 | 183 |
| ffprobe missing file / invalid data | 1 | 1 |
Rules: test `!= 0`; never branch on the value; and because `-n` exits 0, check that the output's
mtime changed. ffprobe exits 1 for any failure.

## Progress parsing [measured]
```
ffmpeg -hide_banner -loglevel error -nostats -y -i in.mp4 -c:v libx264 … -progress pipe:1 -f mp4 out.mp4
frame=90 fps=0.00 stream_0_0_q=-1.0 bitrate=5612.8kbits/s total_size=2058016 out_time_us=2933333 out_time_ms=2933333 out_time=00:00:02.933333 dup_frames=0 drop_frames=0 speed=8.08x progress=continue|end
```
Blocks repeat every `-stats_period` (default 0.5 s [doc]); the last block has `progress=end`.
Percent = `out_time_us / (expected_duration × 1e6)`; `-progress` can also be a file or a URL.
With `-nostats` nothing else goes to stdout, so `key=value` parsing is safe; stderr keeps errors.
`-loglevel error` + `-hide_banner` for logs; `-v warning` when you want the DTS/keyframe warnings
that indicate a broken concat (03).

## Timeouts / hangs [community + inferred]
ffmpeg never hangs on files, but it does on `-f lavfi` sources without `-t`, on `-re`, and on
pipes nobody reads. Always pass `-t`/`-frames:v` for generators and a watchdog in the caller
(`subprocess.run(timeout=…)`).
