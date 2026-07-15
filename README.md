# Windows LLM Manager

Windows LLM Manager is an authenticated HTTPS service for non-interactive administrative PowerShell on Windows 10/11. It includes:

- `agent.exe`: Windows service with one-shot and persistent PowerShell execution;
- `updater.exe`: signed GitHub Release updater with checksum, embedded cosign verification and rollback;
- `deploy.cmd`: interactive host-specific provisioning package builder;
- `release.cmd`: separate universal `agent.exe` signing and GitHub release workflow;
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

For the normal one-PC workflow, run this from an ordinary Windows terminal with no arguments:

```powershell
.\deploy.cmd
```

The script asks whether to generate a bearer token into the package and then asks for the target PC's IPv4 address. The address becomes the certificate IP SAN, so clients connect to that same IP. If no token is packaged, `install.cmd` generates it on the target. In both modes the installer displays the installed token once.

The first run creates the internal CA and cosign pair outside the repository; creating the password-protected signing key adds a one-time password prompt. Every generated package receives a unique CA-signed leaf certificate and private key. Output under `.\deployPackage\<IP>-<version>\` contains a host-specific ZIP, `install.ps1`, and a thin `install.cmd` launcher. The directory is ACL-restricted and excluded by `.gitignore`.

After initializing the CA, deploy also updates the two public trust files in existing Codex (`%CODEX_HOME%\skills\remote-windows-admin`) and Claude Code (`%USERPROFILE%\.claude\skills\remote-windows-admin`) installations. This prevents a previously installed skill from retaining its `UNINITIALIZED_RUN_DEPLOY_PS1` placeholder. No bearer token, CA private key, cosign key or host leaf key is copied into either skill.

The provisioning ZIP contains a leaf TLS private key and may contain a bearer token. Because this repository is stored under Nextcloud, configure the sync client to exclude `deployPackage` or remove the package immediately after transferring it to the target PC. The internal CA key, cosign key and temporary staging files always remain in the external secrets directory and never enter `deployPackage`.

Advanced automation can still pass `-Version`, `-GitHubOwner`, `-GitHubRepository`, `-TargetName`, `-TargetIP`, proxy/firewall settings, or a fleet manifest to `deploy.cmd`. Ordinary package creation never creates or publishes GitHub release assets and does not unlock an existing cosign private key.

For a fleet, pass `-ManifestPath targets.csv`. Columns are `TargetName`, `TargetIP` (comma/semicolon separated), `TrustedProxyIP`, and `FirewallRemoteAddress`. The script creates one unique TLS package per row.

Copy that directory securely to the matching IP and run `install.cmd` as Administrator. The installer verifies that the target owns the packaged IP, deletes the sensitive ZIP after success, locks down the installation, creates the firewall rule, service and update Scheduled Task, and displays the installed token. It leaves the non-secret installer scripts for transparent manual cleanup.

The installer also places `rotate-token.cmd` and `rotate-token.ps1` in `C:\Program Files\WindowsLLMManager`. Run `rotate-token.cmd` as Administrator to atomically create a replacement token, restart the service when it was running, roll back on failure, restore the locked token ACL and display the new token once. The old token becomes invalid after the service restarts.

`-IncludeToken` creates a new token for each package. Use `-SharedToken` only for a consciously accepted homogeneous batch: it creates or reuses an ACL-locked `shared-token.txt` so separate TLS packages deliberately share one bearer token. Provisioning ZIPs, bearer tokens and TLS keys are never release assets.

## Build a universal release

Release creation is deliberately separate from provisioning and never asks for a target IP:

```powershell
# First update VERSION to 0.2.0, commit it, and push the release source.
.\release.cmd
# Review build\release-v0.2.0, then publish explicitly:
.\release.cmd -Publish
```

The release contains only the universal `agent.exe`, its SHA-256 file and detached cosign signature. Every installed updater downloads the same binary, verifies it with its embedded public key, stops the service, replaces only `agent.exe`, and restarts it. The machine's TLS certificate/key, bearer token and configuration remain local and unchanged.

See [deployment notes](docs/deployment.md) for TLS and Cloudflare topology.
