### Fixed - the hosted NVIDIA client dials public addresses only

- **What.** `nimclient.New` routes a hosted NVIDIA base (`IsHostedNVIDIA`: https, `api.nvidia.com` or a subdomain) through `netguard.PublicTransport`, so the connection that carries the NVIDIA key can only reach a public address. Self-hosted NIM bases on the LAN or the tailnet keep the plain transport and stay keyless.
- **Why.** Register SF-01: the key was bound to the hosted host name in 0.156.1, but a DNS answer for that name that pointed at a private, loopback or tailnet address would still have been dialled with the key attached. The base allowlist (S-30) narrowed this; the dial gate closes it.
