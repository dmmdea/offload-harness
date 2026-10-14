### Added - doctor warns when an endpoint names a box by a private-range or tailnet IP literal instead of a name

- **What it does.** `local-offload doctor` prints a `lan ip:     WARN` row for each configured endpoint the harness dials
  whose host is an IP literal in a private range (RFC 1918, the IPv6 unique-local block), a link-local literal, or a
  tailnet CGNAT literal (that row says to use the MagicDNS name). Loopback, every name and a public literal say nothing.
  The fields read are `endpoint`, `seat_endpoints`, `cascade_remote_lanes`, `delegate_remotes`, `fleet_queue_holder`,
  the explicit entries of `pair_workloads_relay` and the `host:port` of every enabled valkey `kv_cache_server`
  binding. `nim_endpoint` and `nim_bases` are left out on purpose: a self-hosted NIM on the LAN is what that allowlist is for.
- **Why.** Cross-machine reach uses tailnet names. A literal works until the box it names moves or its firewall is
  scoped to the tailnet, and then the endpoint is dead with nothing in the file that says why. The load already refused
  a literal outside the tailnet in `seat_endpoints`, `cascade_remote_lanes` and `fleet_queue_holder`, and
  `delegate_remotes` already failed `config findings`; `endpoint`, `pair_workloads_relay` and a cache-server address
  loaded clean in silence, and a tailnet literal was accepted everywhere.
- **Shape.** Configuration only, so a serving layer that is down does not hide it. The row names the field with its list
  index or map key and never prints the address. A config with nothing to warn about prints nothing, and the row never
  changes doctor's exit code. It lives in `doctor_lanip.go`; `internal/config` is unchanged.
- **Tests.** Red first (`doctor_lanip_test.go`, committed against a stub): every field above warns and the output never
  contains the address, each range is tested at both edges (RFC 1918 class B and the tailnet CGNAT block, IPv4-mapped
  IPv6, link-local, unique-local), names, loopback and public literals are silent, a disabled or storeless binding, an
  `fs_native` path and the relay's `auto` / `off` are not read, rows come out in a fixed order, and `doctorRun` prints
  the row and still returns nil. Every address in the tests is built at runtime.
- **Docs.** `docs/OPERATOR-GUIDE.md` (the `doctor` rows beside `capacity:`) and `docs/systems/cache-server.md`.
