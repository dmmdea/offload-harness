### Removed - tests that only checked the wording of docs

- **What.** Ten root-package test files that asserted sentences, links and claims in Markdown docs are deleted: `delegation_contract_docs`, `dispatch_log_contract_docs`, `docs_lint`, `fleet_door_claims_lint`, `fleet_ingress_docs`, `gpu_ram_docs`, `review_policy_docs`, `run_template_docs`, `self_improvement_docs` and `upgrade_note` (`_test.go`). They tested no behaviour, and every doc edit had to keep them green. `AGENTS.md` no longer points at the docs lint.
- **Why.** The testing rule of 2026-10-18 (`AGENTS.md`): CI as it stands is the gate, and tests that pin wording are cut. Tests of behaviour, the identity and leak scans, the changelog fragment check, the tier layer-set and generated-tier-docs checks and `fence_cost_docs` (its helper is shared) stay.
- **Check.** `go vet .` compiles the root package without the deleted files.
