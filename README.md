# Windows LLM Manager

Windows LLM Manager is an authenticated HTTPS service for non-interactive administrative PowerShell on Windows 10/11. It includes:

- `agent.exe`: Windows service with one-shot and persistent PowerShell execution;
- `updater.exe`: signed GitHub Release updater with checksum, embedded cosign verification and rollback;
- `deploy.ps1`: Windows-first host-specific packaging, internal CA and release workflow;
- `scripts/installer.ps1`: elevated target installer used as `install.ps1` in generated packages;
- `remote-windows-admin/`: skill and CA-validating PowerShell API helpers.

The service runs as `LocalSystem` and intentionally permits arbitrary PowerShell to an authenticated operator. Treat its bearer token, signing key, CA key and leaf TLS keys as privileged credentials.

## Development checks

```powershell
$env:GOTELEMETRY = 'off'
go test ./...
.\scripts\test-local.ps1
python "$env:USERPROFILE\.codex\skills\.system\skill-creator\scripts\quick_validate.py" .\remote-windows-admin
```

The local integration test creates a temporary CA and leaf certificate, starts the agent on `https://127.0.0.1:18443`, validates the pinned CA through the PowerShell 5.1 helper, exercises `/exec`, persistent session state and the kill-switch, then removes its artifacts.

## Build a provisioning package

Run from an ordinary Windows terminal. Signing is interactive when the cosign key is first generated or unlocked.

```powershell
.\deploy.cmd `
  -Version 0.1.0 `
  -GitHubOwner YOUR_OWNER `
  -GitHubRepository windows-llm-manager `
  -TargetName host01.example.internal `
  -TargetIP 10.0.0.11 `
  -TrustedProxyIP 10.0.0.5 `
  -FirewallRemoteAddress 10.0.0.5 `
  -SecretsDirectory D:\WindowsLLMManager-Secrets
```

The first run creates the internal CA and cosign pair outside the repository. Every target receives a unique CA-signed leaf certificate and private key. The output under `<SecretsDirectory>\packages\<host>-<version>\` contains a host-specific ZIP, `install.ps1`, and a thin `install.cmd` launcher. Staging and provisioning ZIPs never enter this repository or its Nextcloud-synchronized path.

For a fleet, pass `-ManifestPath targets.csv`. Columns are `TargetName`, `TargetIP` (comma/semicolon separated), `TrustedProxyIP`, and `FirewallRemoteAddress`. The script creates one unique TLS package per row; `-Publish` is applied only once.

Copy that directory securely to the matching host and run `install.cmd` as Administrator. The installer deletes the sensitive ZIP after success, locks down the installation, creates the firewall rule, service and update Scheduled Task, and prints a newly generated per-machine token. It leaves the non-secret installer scripts for transparent manual cleanup.

Use `-SharedToken` only for a consciously accepted homogeneous batch. It creates or reuses an ACL-locked `shared-token.txt` in the external secrets directory so separate host-specific TLS packages can deliberately share one bearer token; pass `-SharedTokenFile` to isolate different batches. Use `-Publish` only after `gh auth login`; it publishes only `agent.exe`, `agent.exe.sha256`, and `agent.exe.sig`. Provisioning ZIPs and TLS keys are never release assets.

See [deployment notes](docs/deployment.md) for TLS and Cloudflare topology.
