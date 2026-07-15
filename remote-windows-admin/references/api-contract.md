# Windows LLM Manager API contract

## Contents

- Connection and errors
- Execution
- Sessions
- Health and control
- Status-code map

## Connection and errors

All calls use `https://` and `Authorization: Bearer <token>`. Validate the server certificate chain and hostname against `internal-ca.pem`, then require the root certificate SHA-256 fingerprint to equal `ca-fingerprint.txt`. Never disable certificate validation.

Every error response that is not an intentionally dropped blocklisted connection has this shape:

```json
{"error":{"code":"auth_failed","message":"invalid or missing bearer token","retryable":false,"details":{}}}
```

Successful command execution can still report `execution.success: false`; that is a command failure, not an API error.

## Execution

### `POST /exec`

Request:

```json
{"command":"Get-Service -Name Spooler","format":"json_object"}
```

`format` is exactly `json_object` or `lines`. For JSON output, `output` contains the converted object array. For lines, it contains an array of strings. If JSON output exceeds the configured limit, `output` is `null`, `raw_output` contains the bounded prefix, and `truncated` is true.

Response (`200`):

```json
{
  "request_id":"a1b2c3",
  "execution":{
    "success":true,
    "exit_code":0,
    "format":"json_object",
    "output":[{"Name":"Spooler","Status":4}],
    "stderr":[],
    "truncated":false,
    "timed_out":false,
    "duration_ms":91
  }
}
```

`504 command_timeout` includes `details.execution`; state is unknown. `500 shell_failure` means no trustworthy command result was framed.

## Sessions

### `POST /session`

No body. Returns `201`:

```json
{"session_id":"32-hex-character-id","created_at":"2026-07-15T12:00:00Z"}
```

Returns `409 session_limit` after the configured maximum (default 5).

### `POST /session/{id}/exec`

Uses the same request and successful response as `/exec`. Calls for one session are serialized. A timeout returns `504 command_timeout`, kills the process tree, attempts a respawn, and sets `details.execution.session_restarted`. Premature process exit returns `409 session_process_exited` with the same detail field. In both cases previous session state is lost.

### `POST /session/{id}/restart`

No body. Kills the old PowerShell tree and returns the refreshed session info under the same ID.

### `GET /session/{id}/info`

```json
{"session_id":"id","created_at":"...","last_used_at":"...","uptime_sec":120,"busy":false}
```

### `DELETE /session/{id}`

Returns `204`. An unknown/reaped session returns `404 session_not_found`.

## Health and control

### `GET /health`

```json
{"status":"ok","version":"1.0.0","uptime_sec":600,"open_sessions":1,"kill_switch_armed":false}
```

Health remains available when braked and then reports `status: braked`.

### `GET /blocklist`

```json
{"blocked":[{"ip":"203.0.113.5","blocked_at":"...","failed_attempts":5}]}
```

### `DELETE /blocklist/{ip}`

Returns `204`, `400 invalid_ip`, or `404 ip_not_blocked`. A client whose own IP is already blocked cannot use this endpoint because its connection is dropped; unblock it from another authenticated source.

### `POST /killswitch`

Explicit operator instruction is mandatory. Returns:

```json
{"armed":true,"sessions_killed":true,"disarm":"local_only"}
```

It creates the on-disk flag and immediately kills all sessions. There is no remote disarm endpoint.

## Status-code map

| HTTP | Code | Meaning and response |
|---|---|---|
| 400 | `invalid_json`, `empty_command`, `invalid_format`, `invalid_ip` | Fix the request; do not reinterpret it as command failure. |
| 401 | `auth_failed` | Stop; obtain the correct token. |
| 404 | `session_not_found`, `ip_not_blocked` | Requested server resource does not exist. |
| 409 | `session_limit` | Close a known unused session. |
| 409 | `session_process_exited` | PowerShell died; session state was lost. |
| 413 | `request_too_large` | Reduce the command/request. |
| 423 | `killswitch_active` | Stop; local admin intervention is required. |
| 429 | `rate_limited` | Slow down. |
| 500 | `shell_failure`, `session_*_failed` | Server/process failure; do not assume command state. |
| 504 | `command_timeout` | State unknown; verify read-only before deciding. |

An already-blocklisted source receives a dropped TCP connection without an HTTP body.
