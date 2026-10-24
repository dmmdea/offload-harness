### Fixed - `gpu_faults` and `gpu faults --json` no longer print `0001-01-01T00:00:00Z` for a card with no incident

- **What it does.** A card with no incident used to print `last_incident` (on `/fleet/health`) and `last` (in `gpu faults
  --json`) as `0001-01-01T00:00:00Z`, because `omitempty` never omits a `time.Time`. Both fields now use `omitzero`, so
  they are absent when there is nothing to report, as the docs describe.
- **Why.** The first fleet readings after 0.201.0 showed the zero date on every incident-free card. It reads as data,
  not as "none".
- **Check.** Marshalling a card with no incident now gives
  `{"index":0,"uuid":"GPU-x","bus":"01:00","events":0,"incidents":0,"incidents_24h":0}` on the wire and
  `{"index":0,"uuid":"GPU-x","bus":"01:00","events":0,"incidents":0}` in the report.
