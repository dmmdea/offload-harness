# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report them privately using GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
(the **"Report a vulnerability"** button on the repository's **Security** tab). Include a
description of the issue, the affected version, and steps to reproduce.

We will acknowledge your report and work with you on a fix and coordinated disclosure.

## Security model

local-offload is designed to be safe by default:

- **It runs entirely on your machine.** All inference happens against a local llama.cpp server.
- **It never calls a cloud model and holds no cloud credentials.** There is no cloud fallback
  inside the harness; when it cannot do a task confidently, it returns a structured *defer*.
- **No media egress.** Images, audio, and video are accepted only as a local file path or a
  `data:` URI — never a remote URL — so there is no network egress for media.

Nothing in local-offload sends your data to a third party.
