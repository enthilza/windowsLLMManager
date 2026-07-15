# Deployment and TLS topology

## Internal CA

`deploy.ps1` creates one ECDSA P-256 root CA in the external secrets directory on first use. Its private key never enters the repository, provisioning ZIP, target host, skill, or GitHub release. The public certificate and SHA-256 certificate fingerprint initialize the skill trust material.

Each host-specific package contains a unique ECDSA leaf key and a certificate with the requested DNS/IP SANs. In the default interactive flow, `deploy.cmd` asks for one IPv4 address and places it into the IP SAN. The client must connect to that same IP. The certificate file contains the leaf followed by the root so clients can build the private chain. The client still pins the exact root certificate and fingerprint; a different self-signed root is rejected.

## Direct LAN HTTPS

The skill connects directly to `https://host:port`, verifies hostname/SAN and requires the chain to end at the pinned internal CA. The Windows firewall rule should normally set `RemoteAddress` to the tunnel host IP. Use `LocalSubnet` only when direct LAN administration from the whole subnet is intentional.

## Cloudflare HTTP Tunnel

Cloudflare HTTP ingress terminates client TLS at the Cloudflare edge. An external HTTP client therefore cannot see or validate the agent's internal-CA certificate. The secure split is:

- client to Cloudflare: normal public Web PKI and optional Cloudflare Access; the skill's default `Auto` mode selects `PublicPKI` for the DNS hostname and validates the real Cloudflare edge certificate and exact hostname;
- `cloudflared` to agent: internal CA verification using `caPool`, with the service URL IP present in the agent certificate SAN;
- agent `trusted_proxy_ip`: the exact LAN IP of the `cloudflared` host;
- firewall `RemoteAddress`: the same tunnel-host IP.

Example per-ingress origin settings:

```yaml
ingress:
  - hostname: host01.example.com
    service: https://10.0.0.11:8443
    originRequest:
      caPool: C:\Cloudflared\windows-llm-manager-ca.crt
  - hostname: host02.example.com
    service: https://10.0.0.12:8443
    originRequest:
      caPool: C:\Cloudflared\windows-llm-manager-ca.crt
  - service: http_status:404
```

The service URL IP must be present in the agent certificate SAN. When that cannot be arranged, `originRequest.noTLSVerify: true` permits the self-signed/private origin certificate but disables certificate authentication only on the `cloudflared`-to-agent hop. It does not weaken the skill's public certificate and hostname validation on the client-to-Cloudflare hop.

The agent accepts `CF-Connecting-IP` only when the raw TCP peer exactly matches `trusted_proxy_ip`; direct LAN callers cannot spoof it. For direct IP access the skill's `Auto` mode instead selects `InternalCA` and pins the bundled CA. A direct internal-CA endpoint addressed by DNS requires explicit `-TLSMode InternalCA`.

## Bootstrap files

Treat each provisioning ZIP as secret because it contains the target's leaf TLS private key and may contain a package-specific or shared bearer token. Transfer it through an authenticated channel. The installer removes the ZIP after success unless `-KeepPackage` is explicitly supplied for diagnostics.

Running `deploy.cmd` without arguments asks whether a new token should be placed into the package. Answering no omits `token.txt`; the elevated installer generates it locally. Answering yes creates a unique package token. In either case, `install.cmd` displays the installed token once. Normal package creation does not sign or publish release assets, so an existing cosign key does not need to be unlocked.

The default output is `.\deployPackage\<IP>-<version>`. Git ignores the entire `deployPackage` directory and the deploy script removes inherited ACLs, granting access only to the current user, Administrators and SYSTEM. This does not prevent a sync client running as the current user from uploading the ZIP. Exclude that directory from Nextcloud or remove it immediately after secure transfer. The CA private key, cosign private key and staging directory remain outside the repository under `%USERPROFILE%\.windows-llm-manager-secrets`.

Every provisioning ZIP contains `rotate-token.cmd` and `rotate-token.ps1`; the installer copies both beside the installed agent. The rotation helper must run elevated. It generates a new token into a temporary ACL-locked file, stops the service only for the atomic swap, starts it with the new token, removes the old token only after successful startup, and rolls back if the swap or restart fails.

## Universal GitHub releases

`release.cmd` is separate from provisioning. It takes no target name or IP and never creates a TLS certificate, token, configuration or installation ZIP. It builds one universal `agent.exe`, writes its SHA-256 digest, creates a detached cosign signature and publishes those three files only when `-Publish` is explicit.

Each installed PC keeps its host-specific TLS certificate/key, bearer token and configuration. The agent service launches the detached updater at the `update_check_interval_min` interval from `config.json` (default 20 minutes; 0 disables checks). The updater downloads the same universal release, verifies the checksum and embedded-key signature, stops the service, backs up and replaces only `agent.exe`, then starts the service. Creating an agent release therefore does not require rerunning `deploy.cmd` for existing PCs. Agent 0.1.2 removes the legacy `WindowsLLMManagerUpdateCheck` Scheduled Task after migration.
