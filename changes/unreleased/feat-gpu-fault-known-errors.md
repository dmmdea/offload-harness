### Added - `gpu faults`: the driver's fault history as incidents, with known-error classes and NVIDIA's action

- **What it does.** `local-offload gpu faults [--days N] [--jobs] [--json]` covers every card and up to 30 days.
  - It reads the NVIDIA driver's fault log (Windows `nvlddmkm`, Linux `Xid`) and, on Windows, the live kernel dumps
    Windows Error Reporting filed.
  - It classifies each event against a known-error table and groups records into 3-minute incidents, placed on cards
    by PCI bus.
  - It flags NVSentinel's repeat rule (one class in 5 or more bursts on one card within 24 h) and applies NVIDIA's Xid 13
    workflow to the GPC/TPC units that graphics exceptions name.
  - `--jobs` adds the ledger rows that ran on this node during each incident.
  - Facts only: nothing moves a lease verdict (C-83-B).
- **Why.** Driver faults were found by hand, weeks late, and read without context. The pattern comes from NVIDIA's AIOps
  "supervised log errors" and NVSentinel's Xid catalog and incident rules. Neither product runs here, so this carries their
  data (the Xid catalog, Apache-2.0, see NOTICE) and their rules. Windows ids are read as the Xid they match, and the output
  says so.
- **Check.** One real 30-day run on a three-card Blackwell GeForce box:
  - 479 records in 19 incidents, placed on buses `17`, `65` and `b5`.
  - The repeat rule fired on one card.
  - Both id-13 bursts read as the application's fault (spread over 35 and 3 units).
  - 11 live dumps, deduplicated by dump file from WER's re-queued records.
  - One incident joined to ten agent-pool jobs that all ended deferred.
  - `--json` parses.

  Details: [GPU lease](docs/systems/gpu-lease.md#gpu-faults-the-fault-history-as-incidents-with-known-errors).
