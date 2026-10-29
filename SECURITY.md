# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report them privately using GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
(the **"Report a vulnerability"** button on the repository's **Security** tab). Include a
description of the issue, the affected version, and steps to reproduce.

We will acknowledge your report and work with you on a fix and coordinated disclosure.

## Security model

offload-harness is designed to be safe by default:

- **The default path runs entirely on your machine.** All cascade inference happens against a local llama.cpp
  server; there is no cloud fallback, and when the harness cannot do a task confidently it returns a structured
  *defer* instead of guessing.
- **The cascade holds no cloud credentials.** It cannot escalate to a hosted model.
- **No media egress.** Images, audio, and video are accepted only as a local file path or a
  `data:` URI — never a remote URL — so there is no network egress for media.
- **Fleet nodes are local infrastructure.** Delegation targets must be loopback or the operator's own tailnet; a public
  host is refused at config load.

By default nothing in offload-harness sends your data to a third party. Three **opt-in** surfaces, each off until you
configure it, do send content off the box, and the README says so where they are described:

- **`offload_nim` / `shadow-label --oracle nim`** call an NVIDIA NIM endpoint with the prompt or the queued input text.
  The key is read from `$NVIDIA_API_KEY` only and is sent only to NVIDIA hosts; nothing escalates into it.
- **The browse lane** (`offload_browse`, the agent `browse` tool) drives your own browser and asks a loopback decision
  endpoint you run for each step; that endpoint may front a hosted model, so visible page text can leave the machine.
- **The coding agent's `--allow-fetch`, `--allow-search` and `--allow-github`** flags reach the hosts you allow-list,
  DuckDuckGo, and GitHub with your token. They are off by default.

See the [Security](README.md#security) section of the README for the agent's full posture.
