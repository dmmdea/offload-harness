### Fixed - the vLLM seat's cache-server share is mounted where the LMCache MP server looks, not only where the launcher looks

- **What was wrong.** On WSL2 the seat launcher (`seat_fg.sh`) is started through `wsl.exe`, and that session has a mount
  namespace of its own whose `/` is private. The launcher mounted the cache-server share at `SEAT_L2_MOUNT_DIR` in that
  namespace, logged `cache-server share mounted`, and then started the LMCache MP server as a systemd unit
  (`systemd-run`), which lives in systemd's namespace and never saw the mount. The MP server is the process that writes
  the L2 store, so it wrote the store into the bare mount point on the distro's own disk. On the reference box that was
  91 GiB in 27 hours (2026-11-19 08:07 to 11-20 11:31, 1,875 chunk files written as the seat came ready and served), until
  the disk was full. From then on the store's writes failed, and after the next host restart WSL mounted the distro
  read-only with a RAM overlay: every start of the seat died before it answered, and every local contract was deferred
  `seat down`. In those 27 hours nothing reached the cache server at all.
- **What it does now.** When the launcher's mount namespace is not pid 1's, it mounts the share in pid 1's namespace as
  well (`nsenter -t 1 -m`, the same source and options) and logs
  `cache-server share also mounted in systemd's mount namespace, where the MP server runs`. When that mount fails, or
  `nsenter` is missing, the tier is degraded (`CACHE SERVER DEGRADED ...`) instead of writing to the local disk. Files left
  in the bare mount point by an earlier start are named with their size before the share hides them. The mount in pid 1's
  namespace outlives the start, so a later start finds it: it says so, and when an earlier start mounted it from another
  address it warns, reports the address the MP server really uses in the status file, and names the unmount to run in
  that namespace (the "unmount and restart" hint names it too). The write probe behind `SEAT_L2_MIN_MBPS` now runs in pid
  1's namespace, so the floor measures the mount the MP server writes through rather than the launcher's copy.
- **Not changed.** A launcher that already runs in systemd's namespace (native Linux, or one started by a unit) does
  nothing new. The address choice, the write floor, the prune and everything after the mount are unchanged.
- **Check.** On the reference box, after the orphaned store was archived off the distro's disk and the distro restarted,
  two cold loads of the seat through the contract door. First start: the launcher logged `cache-server share also mounted
  in systemd's mount namespace, where the MP server runs (//<store>/kvcache)`; pid 1's view, the session's view and the MP
  server's own `/proc/<pid>/mounts` all showed the CIFS share at the mount point; the store's new chunk files landed on
  the share and the distro's free space stayed at 76 GiB. Second start, the distro held open so pid 1's mount survived:
  `cache-server share already mounted in systemd's mount namespace too` and `write probe ~328 MB/s (floor 100), through
  systemd's mount namespace, where the MP server writes`. The launcher's existing store tests
  (`seat_fg.kv-and-store.tests.sh`) pass unchanged as a non-root user, where the block stays off.
- **Where.** `setup/templates/vllm-seat/seat_fg.sh`.
