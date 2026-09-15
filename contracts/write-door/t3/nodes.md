# Fleet nodes

The write door (ADR 0044) is opened per node with `agent_allow_write: true`.

| node_id | agent seat | window (tokens) | write door |
|---|---|---|---|
| node-a | pool-a | 163840 | closed |
| node-b | pool-b | 131072 | closed |
| node-c | pool-c | 32768 | closed |
