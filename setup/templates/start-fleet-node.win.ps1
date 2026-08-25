# start-fleet-node.win.ps1 - TEMPLATE for running `local-offload fleet-serve` as a Windows
# scheduled task. Replace __OFFLOAD_HOME__ (install root, e.g. D:/offload-stack) and
# __NODE_ID__ (e.g. aorus-ampere8). See docs/FLEET-NODE.md "Running as a Windows scheduled
# task" for the registration recipe (S4U principal + boot trigger + hidden VBS shim) and the
# measured gotchas this file encodes. Save AS UTF-8 WITH BOM; keep paths FORWARD-slashed
# (PS 5.1 reads a BOM-less file as ANSI, and backslash escapes get eaten by generators).
#
# Proven live on the ampere-8 reference box 2026-08-25. Three measured facts baked in:
#  1. Under S4U (session 0) `tailscale ip -4` returns NOTHING - read the address from the
#     INTERFACE (Get-NetIPAddress -InterfaceAlias Tailscale) instead.
#  2. At boot the Tailscale address EXISTS before it is BINDABLE ("The requested address is
#     not valid in its context") - readiness is a REAL bind test, retried, not a resolve.
#  3. An immediate child exit is detected and surfaced instead of passing silently (the
#     task's own lastResult=0 is a false liveness signal for a detached child).
$Log = '__OFFLOAD_HOME__/fleet/fleet-node.log'
function W($s) { Add-Content -Path $Log -Value ('[' + (Get-Date -Format 'yyyy-MM-dd HH:mm:ss') + '] ' + $s) -Encoding UTF8 }

if ((Get-NetTCPConnection -State Listen -LocalPort 18811 -ErrorAction SilentlyContinue | Measure-Object).Count -gt 0) {
  W 'launcher: 18811 already listening - declining duplicate'; exit 0
}

$ip = $null
for ($i = 1; $i -le 60; $i++) {
  $a = (Get-NetIPAddress -AddressFamily IPv4 -InterfaceAlias 'Tailscale' -ErrorAction SilentlyContinue |
        Where-Object { $_.IPAddress -like '100.*' } | Select-Object -First 1).IPAddress
  if ($a) {
    try { $l = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Parse($a), 18811); $l.Start(); $l.Stop(); $ip = $a; break }
    catch { W ('launcher: ' + $a + ':18811 not bindable yet (' + $_.Exception.Message.Trim() + ')') }
  } else { W ('launcher: no Tailscale 100.x address yet (attempt ' + $i + '/60)') }
  Start-Sleep -Seconds 10
}
if (-not $ip) { W 'launcher: FATAL - no bindable Tailscale address after 10 min'; exit 1 }

$p = Start-Process -FilePath '__OFFLOAD_HOME__/bin/local-offload.exe' `
  -ArgumentList 'fleet-serve', '-listen', ($ip + ':18811'), '-listen-trusted-network', '-node-id', '__NODE_ID__' `
  -WindowStyle Hidden -PassThru `
  -RedirectStandardOutput '__OFFLOAD_HOME__/fleet/fleet-node.out.log' `
  -RedirectStandardError  '__OFFLOAD_HOME__/fleet/fleet-node.err.log'
W ('launcher: started fleet-serve pid ' + $p.Id + ' on ' + $ip + ':18811')
Start-Sleep -Seconds 6
if ($p.HasExited) {
  W ('launcher: exited immediately - ' + ((Get-Content '__OFFLOAD_HOME__/fleet/fleet-node.err.log' -Tail 2 -ErrorAction SilentlyContinue) -join ' | '))
  exit 1
}
W 'launcher: node UP'
Wait-Process -Id $p.Id
W 'launcher: fleet-serve exited'
