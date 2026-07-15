---
name: remote-windows-admin
description: Execute PowerShell commands and verified multi-step administrative operations on remote Windows 10/11 machines through the Windows LLM Manager HTTPS API. Use whenever managing one or more specific Windows hosts by IP or hostname, including software deployment, files, services, scheduled tasks, shortcuts, diagnostics, health checks, sessions, blocklist administration, or an explicitly requested emergency kill-switch.
---

# Remote Windows Admin

Use only the bundled PowerShell helpers. They enforce HTTPS, the pinned internal CA, bearer auth, bounded request handling, and consistent API errors. Never use `-SkipCertificateCheck`, a global certificate callback, or plain HTTP.

Before acting, obtain from the operator for each host:

- HTTPS base URL;
- bearer token;
- intended operation and expected end state.

The internal CA certificate and SHA-256 fingerprint are bundled in `references/internal-ca.pem` and `references/ca-fingerprint.txt`. Refuse to connect if they are absent, uninitialized, expired, do not match, or do not validate the server certificate and hostname. With normal Cloudflare HTTP ingress the client sees Cloudflare's edge certificate, not the agent certificate; use direct HTTPS or end-to-end TCP passthrough when this skill must validate the internal CA. On the `cloudflared` origin hop, configure `caPool` with the same CA.

Use `scripts/ps_admin.ps1` for health, blocklist, unblock, and explicitly instructed kill-switch calls. Its kill-switch action additionally requires `-ConfirmArm`.

## Choose an execution mode

Use `scripts/ps_exec.ps1` for independent checks or actions. Set `-Format json_object` for object-shaped output and `-Format lines` for plain text. Do not add `ConvertTo-Json`; the server applies its fixed Base64 scriptblock wrapper. If unsure, use `lines`.

Use `scripts/ps_job.ps1` for operations that can approach or exceed 120 seconds, including DISM, SFC, Windows Update, MSI/EXE installers, large copies, image servicing, and lengthy scans. Submit once, retain the returned job ID, and poll that exact ID with `-Action Status`. Never submit the same mutation again while its job is `running` or `cancelling`. The default long-job limit is 7200 seconds and the server enforces its configured maximum.

Use `scripts/ps_session.ps1` when later steps genuinely depend on variables, functions, modules, or current directory from earlier steps. No more than five sessions may exist on a host. Always close a session in cleanup. Two exec requests for one session are serialized by the server.

Use `scripts/verify.ps1` for read-only postcondition checks so verification remains visibly distinct from mutation.

## Work safely

Before a multi-step change, state the intended steps and explicitly flag deletion, task removal, replacement without backup, and other irreversible actions. Do not arm `POST /killswitch` unless the operator explicitly requests it.

For every host, work sequentially:

1. Call `GET /health`. Stop if the host is braked.
2. Check current state before acting.
3. Make one idempotent change.
4. Immediately run a specific read-only verification.
5. Continue only when verification proves the expected state.
6. Report the result and verification output for that host before moving to the next host.

Prefer check-then-act and repeatable operations. Verify copied files by hash or size, tasks by their action/trigger, services by configuration and state, and shortcuts/files by their exact path and expected properties.

Remote PowerShell is non-interactive and console-less. Never use prompts, `Read-Host`, progress UI, cursor control, `Clear-Host`, or commands requiring a person at the console. Suppress confirmations with `-Confirm:$false` or `-Force` only when semantically safe. Avoid `Format-Table` and `Format-List` for parsed output.

## Handle outcomes correctly

Treat these server-layer refusals as non-command failures:

- `401 auth_failed`: stop immediately; do not retry and burn another blocklist attempt.
- dropped connection after auth failures: assume the source IP may be blocklisted; stop and report it.
- `423 killswitch_active`: stop targeting the host. It can only be disarmed locally by an administrator.
- `409 session_limit`: close a known unused session; never retry in a loop.
- `429 rate_limited`: obey the response and slow down.

Treat `execution.success: false` in a successful HTTP response as a command failure. Diagnose from `exit_code` and `stderr`, then verify state.

On `504 command_timeout`, the synchronous PowerShell process tree was killed. Do not claim that the HTTP request or its PowerShell process is still running. Because Windows servicing can delegate work to system services outside that tree, target state is nevertheless unknown; inspect state read-only before deciding and never blindly repeat a non-idempotent action. In session mode the session is also restarted and all state is lost.

For asynchronous jobs, only `completed`, `failed`, `timed_out`, and `cancelled` are terminal. Continue polling `running` or `cancelling`. A completed job can still contain `execution.success: false`; treat that as command failure. If a job disappears with `404 job_not_found`, do not repeat its mutation merely because the retained result expired.

On `409 session_process_exited`, the command called `exit` or otherwise killed PowerShell. The server attempts to respawn it under the same ID, but prior session state is lost. Report this before continuing.

If a kill-switch response appears during fan-out, halt the entire remaining batch and report the last completed host.

For full endpoint schemas and status codes, read `references/api-contract.md`.
