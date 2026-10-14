# Synthetic failure-class table for the register tests

Rows for classes the fixture ledger holds, one row for a class it does not hold (`write`), and no row for
`capacity_wait`, `config` or `other`, so those three must read as undocumented.

| class | meaning | root cause | structural fix |
|---|---|---|---|
| `node_busy` | A seat answered a capacity defer after admission. | Another job held what the call needed. | open |
| `abstention` | The model ran and gave no usable answer. | The work, not the stack. |  |
| `failed_verification` | A result failed the contract's own checks. | | Tighten the contract (synthetic row). |
| `budget` | A ceiling stopped the run. | A step budget. | open |
| `stall_prefill` | The run went silent while the prompt was being processed. | Not recorded. | open |
| `poll_deadline` | A node owned the job and it did not finish inside the poll budget. | A job larger than the budget. | open |
| `node_unreachable` | No node answered. | The network path. | open |
| `write` | The write door was refused. | The node does not opt in. | open |

A second table must not be mistaken for the first.

| wire defer class | filed under |
|---|---|
| `capacity` | `node_busy` |
