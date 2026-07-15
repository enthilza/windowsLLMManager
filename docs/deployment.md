# Deployment and TLS topology

## Internal CA

`deploy.ps1` creates one ECDSA P-256 root CA in the external secrets directory on first use. Its private key never enters the repository, provisioning ZIP, target host, skill, or GitHub release. The public certificate and SHA-256 certificate fingerprint initialize the skill trust material.

Each host-specific package contains a unique ECDSA leaf key and a certificate with the requested DNS/IP SANs. The certificate file contains the leaf followed by the root so clients can build the private chain. The client still pins the exact root certificate and fingerprint; a different self-signed root is rejected.

## Direct LAN HTTPS

The skill connects directly to `https://host:port`, verifies hostname/SAN and requires the chain to end at the pinned internal CA. The Windows firewall rule should normally set `RemoteAddress` to the tunnel host IP. Use `LocalSubnet` only when direct LAN administration from the whole subnet is intentional.

## Cloudflare HTTP Tunnel

Cloudflare HTTP ingress terminates client TLS at the Cloudflare edge. An external HTTP client therefore cannot see or validate the agent's internal-CA certificate. The secure split is:

- client to Cloudflare: normal public Web PKI and optional Cloudflare Access;
- `cloudflared` to agent: internal CA verification using `caPool`, with hostname verification through `originServerName`;
- agent `trusted_proxy_ip`: the exact LAN IP of the `cloudflared` host;
- firewall `RemoteAddress`: the same tunnel-host IP.

Example per-ingress origin settings:

```yaml
ingress:
  - hostname: host01.example.com
    service: https://10.0.0.11:8443
    originRequest:
      caPool: C:\Cloudflared\windows-llm-manager-ca.crt
      originServerName: host01.example.internal
  - hostname: host02.example.com
    service: https://10.0.0.12:8443
    originRequest:
      caPool: C:\Cloudflared\windows-llm-manager-ca.crt
      originServerName: host02.example.internal
  - service: http_status:404
```

The agent accepts `CF-Connecting-IP` only when the raw TCP peer exactly matches `trusted_proxy_ip`; direct LAN callers cannot spoof it.

If the coding-agent skill itself must validate the internal CA over the internet, use an end-to-end TCP transport such as Cloudflare Access TCP rather than HTTP ingress. The usual HTTP tunnel cannot provide origin-certificate visibility to the end client.

## Bootstrap files

Treat each provisioning ZIP as secret because it contains the target's leaf TLS private key and may contain a shared bearer token. Transfer it through an authenticated channel. The installer removes the ZIP after success unless `-KeepPackage` is explicitly supplied for diagnostics.
