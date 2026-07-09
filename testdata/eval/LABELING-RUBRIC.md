# Labeling Rubric — offload-harness eval gold corpus

This rubric is the **single source of truth** for how every gold case in
`testdata/eval/` is labeled. It exists so labels are auditable, reproducible,
and **consistent across the training and held-out splits**. Every input was
hand-curated to be realistic, single-intent, and to have **exactly one
defensible label** under the rules below. Anything that could plausibly take
two labels was rewritten until it was unambiguous, or dropped — there are no
deliberate "trick" or borderline cases.

The grader (`internal/eval/eval.go`, `Grade`) checks:
- **classify** → the model's `label` must equal the case's `expect` (case-insensitive).
- **triage** → the model's `decision` must equal the case's `expect` (`yes`/`no`).

All inputs are brand-agnostic and generic; no personal or brand-specific data.

---

## Files & splits

| File | Task | Count | Balance |
|---|---|---|---|
| `classify.jsonl` | classify (train) | 162 | 54 billing / 54 technical / 54 account |
| `holdout/classify.jsonl` | classify (holdout) | 45 | 15 / 15 / 15 |
| `triage.jsonl` | triage (train) | 158 | 79 yes / 79 no |
| `holdout/triage.jsonl` | triage (holdout) | 40 | 20 yes / 20 no |

The `holdout/` split is **provably disjoint** from training (no shared or
duplicate input strings; verified by exact set intersection = 0). It is
reserved for the later A1 regression A/B and must never enter the
training/replay corpus.

---

## classify — label set `["billing", "technical", "account"]`

Assign by the message's **primary intent**. Pick the single label that names
what the user actually wants resolved.

### `billing` — it is about money
Charges, refunds, invoices, prices, payment methods, plan-**cost** disputes,
currency, taxes/VAT, proration, and **stopping recurring charges** (cancelling
a subscription so billing stops, ending auto-renewal). If the core of the
request is a monetary amount, a payment, or making charges start/stop, it is
`billing`.

- *"My card was charged twice this month, please refund the duplicate."* → `billing`
- *"Please cancel my subscription so I stop getting charged."* → `billing`

### `technical` — the product is malfunctioning
Errors, crashes, bugs, API failures, things not loading, not working, broken,
timing out, or rendering incorrectly. The defining signal is **something is
broken or behaving wrong**, not a request to manage money or identity.

- *"The app crashes every time I open the settings page."* → `technical`
- *"The API returns a 500 error when I POST to /orders."* → `technical`

### `account` — identity / access / membership management
Login, passwords, two-factor, email/name/username changes, adding or removing
users, roles and permissions, SSO, connected apps/API keys, recovery options,
**deleting the account or personal data**, and merging/transferring accounts —
with **no money** and **no malfunction** involved.

- *"How do I change the email address on my profile?"* → `account`
- *"I need to cancel my account and delete all my stored data."* → `account`

### Disambiguation rules (these resolve the known ambiguities)

1. **"Cancel my subscription" → `billing`.** The intent is to stop recurring
   charges, which is monetary. This is applied **everywhere**, resolving the
   original corpus's inconsistency (the old set labeled two near-identical
   cancel-subscription inputs differently: one `account`, one `billing`). The
   rubric now fixes them all as `billing`.
   - "Stop the automatic renewal", "Cancel my subscription so I stop getting
     charged", "Stop billing me; end the subscription at the period's end" → all `billing`.
2. **"Delete / close my account" or "delete my personal data" → `account`**,
   when there is **no** billing element. Cancelling an *account* (identity
   removal, data deletion, GDPR-style erasure) is `account`; cancelling a
   *subscription* (stop paying) is `billing`. The two are kept distinct by
   whether the request is about money (billing) or identity/data (account).
3. **Billing-email or billing-address change → `billing`** (it concerns the
   invoice/payment record), whereas a **profile/login email change → `account`**.
4. **A wrong charge that stems from a feature bug is still `billing`** if the
   user is asking about the charge/refund. If instead they report the feature
   itself misbehaving (e.g. "the upgrade button does nothing"), it is `technical`.
5. **2FA "stopped working" → `account`** (it is access/identity management),
   not `technical`, unless the message describes a server error/crash in the
   product. SMS codes "never delivered" framed as a delivery malfunction →
   `technical`; "my 2FA codes stopped working after I switched phones" (a
   re-enrollment/access issue) → `account`.

---

## triage — fixed question

Every triage case uses the **identical** question:

> `Does this text indicate an error or failure?`

Decide `yes` / `no` by the **net reported outcome** of the text.

### `yes` — a genuine error or failure
The text reports an error, failure, crash, fatal condition, abort, a
**non-zero failing exit**, an unrecoverable problem, or a service being
unavailable. The operation did **not** succeed.

- *"npm ERR! build script failed with exit code 1"* → `yes`
- *"HealthCheck: /readyz returned 503 Service Unavailable."* → `yes`
- *"Job finished. Exit status: 137."* → `yes` (non-zero failing exit = failure)

### `no` — success, or a non-failure signal
The text reports success or completion; an all-passing result; a purely
informational notice; a **warning or deprecation that does not itself
constitute a failure**; **or a transient error that was recovered**, so the
**net outcome succeeded**.

- *"All 42 tests passed in 3.1s."* → `no`
- *"Warning: deprecated API ... Build completed successfully."* → `no`
  (a warning alongside a successful build is not a failure)
- *"First connection attempt failed; retried and succeeded on attempt 2. Done."*
  → `no` (transient error, **recovered**, net success)

### Disambiguation rules (decided by net outcome)

1. **Warnings / notices / deprecations alone → `no`.** They are not failures.
   Only a warning that comes *with* an actual failed result is `yes` (and then
   it is `yes` because of the failure, not the warning).
2. **Recovered transient errors → `no`.** If the text mentions a retry/blip but
   ends in success (exit 0, "reconnected; continuing", "succeeded on retry"),
   the net outcome is success.
3. **Exit codes:** exit/status **0 → `no`**; any **non-zero failing exit
   (1, 2, 137, 139, …) → `yes`**.
4. **One clear outcome per input.** No input mixes an unresolved failure with a
   final success in a way that is genuinely undecidable; each resolves to a
   single net outcome.
