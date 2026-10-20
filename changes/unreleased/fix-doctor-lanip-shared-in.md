### Fixed - doctor's tailnet-literal advice covers a node shared in from another tailnet

- **What it does.** The `lan ip:` row told the operator to use the MagicDNS name for every tailnet CGNAT literal. The row now
  names both ways out for a node shared in from another tailnet: list that tailnet's zone in `tailnet_suffixes` and use the
  node's full name, or keep the 100.x address. It stays informational and never changes doctor's exit code.
- **Why.** A node shared in from another tailnet has no dotless name on this one, and its full name sits under the other
  tailnet's zone, which the tailnet guard refuses unless `tailnet_suffixes` lists it. Following the old advice on a live
  delegator config dropped the node from every fleet lane (doctor printed the guard refusal and exited 1); the config was
  restored from its backup.
- **Check.** One real run: the patched binary's `doctor --config <live delegator config>` prints the new row and exits 0.
