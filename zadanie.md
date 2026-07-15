# Zadanie: Windows LLM Manager — vzdialená správa Windows staníc cez AI

**Owner:** Peter (DS9 s.r.o.)
**Status:** rev. 2 (po bezpečnostnej revízii)
**Formát:** jedno zadanie, dve etapy (serverová časť + skill pre kódovacieho agenta)

---

## 0. Cieľ projektu (prehľad)

Cieľom je systém na **vzdialenú administráciu fleetu Windows 10/11 staníc pomocou
jazykového modelu**. Namiesto toho, aby sa administrátor prihlasoval na každý
počítač ručne, riadi celý fleet cez jeden LLM-ovský rozhranie, ktoré prekladá
pokyny v prirodzenom jazyku na konkrétne administratívne operácie.

Systém má **dve nezávislé časti**, ktoré tvoria dve etapy tohto zadania:

- **Etapa 1 — serverová časť (Go agent).** Na každej spravovanej Windows stanici
  beží malý Go servis, ktorý vystavuje HTTPS API. Cez toto API sa dá vykonať
  PowerShell príkaz (jednorazovo alebo v perzistentnej session s admin právami),
  a to bez viditeľného okna — PowerShell beží „v pamäti", riadený cez stdin/stdout.
  Agent je zabezpečený bearer tokenom, sám sa auto-updatuje z GitHub Releases
  (s overením podpisu), a má bezpečnostné prvky: auto-blocklist, audit log,
  health endpoint a kill-switch.

- **Etapa 2 — klientská časť (skill pre kódovacieho agenta).** Klient NIE je
  samostatný skompilovaný program — je to **skill** (SKILL.md + pomocné
  PowerShell skripty) pre kódovacieho agenta (Claude Code / OpenCode a pod.).
  Skill popisuje, ako sa pripojiť na serverové API, aké endpointy existujú, aký
  výstup vracajú, a hlavne akú **disciplínu** má agent dodržiavať (verifikácia
  po každom kroku, idempotencia, rozlišovanie „server ma odmieta" vs „príkaz
  zlyhal"). Ciele (IP/hostnamy) a ich tokeny dodáva operátor ad hoc v každom
  zadaní — skill ich neukladá.

**Typický scenár použitia:** operátor povie modelu niečo ako „na týchto X
počítačoch prekopíruj novú verziu softvéru do priečinka, preplánuj scheduled
task na nový a vymeň ikonu na ploche" — a kódovací agent (riadený skillom)
sám zvolí konkrétne PowerShell príkazy, vykoná ich cez serverové API na každej
stanici, po každom kroku overí výsledok, a nahlási výsledok za každý stroj.

**Technologický základ:** Go (server + updater), PowerShell (vykonávané príkazy
+ pomocné skripty klienta), Cloudflare Tunnel (vzdialený prístup), GitHub
Releases (distribúcia + auto-update), cosign (podpisovanie binárok).

---
---

# ETAPA 1 — Serverová časť (Go agent)

## 1.1 Runtime model

- Single static Go binary, cross-compiled from Linux dev machine:
  `GOOS=windows GOARCH=amd64 go build`
- Installed as a Windows service (`kardianos/service` or `golang.org/x/sys/svc`).
- Binds to `0.0.0.0:<port>` — reachable over the LAN, not just from localhost.
  **Required companion control:** since this opens the port to every interface,
  add a Windows Firewall inbound rule scoping access to only the expected source
  (the tunnel-hosting machine's IP, or `RemoteAddress=LocalSubnet` at minimum).
  Bearer token auth (1.4) still applies regardless, but firewall scoping is the
  layer that stops unauthenticated port scanning/connection attempts from
  reaching the Go process at all.

## 1.2 Two execution modes

**A. Persistent session (stateful)**

- `POST /session` → spawns `powershell.exe -NoLogo -NoExit -Command -` as a child
  process with `CREATE_NO_WINDOW`; stdin/stdout/stderr kept as open pipes in the Go
  process. Returns `session_id`.
- `POST /session/{id}/exec` → writes command + newline to stdin, appends a sentinel
  echo (`Write-Output "___END_<uuid>___"`), reads stdout until sentinel line is
  seen. This delimits one command's output. Stderr read in parallel goroutine.
  **The `<uuid>` must be freshly random per command** (not per session), and the
  read loop must match the **entire line** equal to the sentinel, not a substring
  — otherwise a command that happens to emit (or is deliberately induced to emit)
  the sentinel string could truncate or spoof output framing.
- **Max concurrent sessions capped at 5** (configurable). `POST /session` beyond
  the cap returns an error rather than spawning unbounded PowerShell processes —
  protects against a client bug or runaway loop exhausting the machine.
- `DELETE /session/{id}` → kills the child process, cleans up.
- `POST /session/{id}/restart` → kills and respawns a fresh PowerShell process
  under the same `session_id`, for context/memory hygiene on long-lived sessions.
- `GET /session/{id}/info` → returns session age/uptime, for manual or policy-based
  recycling (e.g. auto-restart every N hours).
- Idle-timeout reaper goroutine kills sessions unused for >30 min (configurable) to
  avoid orphaned PowerShell processes.
- Real process state persists across calls: variables, `cd`, imported modules —
  functionally identical to a human typing into a visible terminal, just read via
  pipes instead of a rendered console.

**B. Stateless one-shot exec (no session)**

- `POST /exec` with `{ "command": "...", "format": "json_object" | "lines" }`
- **Command is passed via stdin, never concatenated into `-Command "..."`.**
  Spawn `powershell.exe -NoLogo -NonInteractive -Command -` (dash = read from
  stdin), write the command to stdin, close stdin, capture stdout/stderr, wait
  for exit. This avoids command injection: an unbalanced quote or `;` in the
  command string can't break out of an intended shell invocation because there
  is no surrounding string to break out of — PowerShell parses the stdin
  content as its own script. This is the same safe mechanism the session mode
  already uses, just for a single-shot process.
- **Output format is determined by the nature of the command's output, not an
  arbitrary client choice:**
  - **Tabular/structured output** (anything that's naturally objects with fields —
    `Get-Process`, `Get-Service`, `Get-ChildItem`, etc.) → `format: "json_object"`.
    The server wraps the command so its output goes through `ConvertTo-Json` —
    but this wrapping is done by composing it as part of the stdin script with
    proper boundaries (e.g. writing the user command, then on a separate stdin
    line piping the *result* through `ConvertTo-Json`), **not** by string-
    appending `| ConvertTo-Json` onto an untrusted command string. PowerShell's
    object model is preserved with real field names/types.
  - **Line-based/plain-text output** (log tails, `ipconfig`, raw command output
    with no inherent object structure) → `format: "lines"`. Server captures raw
    stdout as-is and splits on newlines into a `[]string`.
  - The client (LLM agent) decides which mode fits the command it's issuing.
- No state pollution risk. Best for one-off checks: `Get-Service`, disk space,
  uptime, etc.

## 1.3 Known console-less limitations

Cmdlets depending on `$Host.UI.RawUI` (real console buffer API) misbehave without an
allocated window:

- Fine: `Get-*`/`Set-*` cmdlets, `Write-Output`, `ConvertTo-Json`, pipeline ops,
  `cd`/`Set-Location` — i.e. anything you'd put in a non-interactive `.ps1` script.
- Problematic: `Read-Host -AsSecureString` and other interactive prompts;
  `Start-Transcript` (some PS versions require a real console host);
  `Write-Progress` (no-ops or clutters stdout, harmless);
  `$Host.UI.RawUI.WindowSize`/`BufferSize` reads (return defaults or throw).
- No real terminal "size" exists — `[Console]::WindowWidth`/`Height` return
  defaults (often 80/120 col) or throw. Avoid `Format-Table` for anything you'll
  parse programmatically; use `ConvertTo-Json`/`ConvertTo-Csv` instead, where width
  is irrelevant.
- **Safety net — explicit default width:** since some commands or scripts will
  produce table-formatted output regardless of the guidance above (third-party
  modules, forced `Format-Table`, `Out-Host` defaults), the agent sets a wide
  default width on every spawned PowerShell process so wrapped table output is
  still parseable as text rather than truncated/wrapped at 80 columns:
  ```powershell
  $Host.UI.RawUI.BufferSize = New-Object Management.Automation.Host.Size(200, 50)
  ```
  Run once per session at spawn time (persistent sessions) or prepended to the
  command for stateless `/exec` calls. This is a fallback, not a substitute for
  requesting `ConvertTo-Json`/`ConvertTo-Csv` — it just guarantees that if a table
  does get produced, it's at least usably wide instead of wrapped at a narrow
  default.

## 1.4 Auth

**What the token actually is — requirements:**

There's no protocol-level requirement (bearer tokens are just opaque strings
in the `Authorization` header) — but *practically*, the token must have
enough entropy that it can't be guessed or brute-forced, since it's the only
thing standing between "anyone who can reach the port" and "admin PowerShell
execution." Concrete requirements:

- **Minimum 32 bytes (256 bits) of cryptographic randomness**, base64 or
  hex-encoded for safe use in an HTTP header — e.g. Go's
  `crypto/rand` → 32 random bytes → `base64.URLEncoding`, giving a ~43-character
  string. Do **not** hand-pick a "random-looking" password — human-chosen
  strings, even long ones, have far less real entropy than they look like.
- No character-set restrictions beyond "safe in an HTTP header value" — avoid
  characters that need escaping (stick to base64url or hex, both header-safe
  by construction).
- **Token-per-host is optional, not mandatory** — two supported models,
  chosen at deploy time (see 1.7): either a unique token per host (best
  isolation — one leak compromises only that machine), or one shared token
  across a batch of hosts (convenience for homogeneous fleets). Which one
  applies is determined purely by whether the deploy package ships a
  `token.txt` or not. Neither is "the rule"; they're two deliberate options
  with a stated tradeoff (1.7).
- **Transport must be HTTPS, not HTTP** — the bearer token authenticates every
  request, so the leg carrying it must be encrypted end to end. Cloudflare
  terminates TLS at its edge, but the hop from the tunnel-hosting machine to
  each agent (and the agent's own `0.0.0.0` listener) must also be HTTPS —
  otherwise the token travels the LAN in plaintext and anyone sniffing that
  segment captures admin credentials. Agent serves HTTPS with an internal-CA
  or self-signed cert; `cloudflared` origin config set accordingly
  (`originRequest` with the expected cert). *(Exact Cloudflare routing config
  to be finalized separately — open item.)*
- Server-side: store a SHA-256 hash of the token, not the plaintext, in the
  agent's own comparison logic — even though the token also lives in
  `token.txt` on disk (see below), the *comparison* the server does on every
  request should be against a hash, and the comparison itself must use
  `subtle.ConstantTimeCompare` rather than `==`, to avoid timing side-channels
  that let an attacker guess the token byte-by-byte over many requests.

**token.txt file-storage plan — with concrete ACL steps:**

Plaintext token in a separate file, permission-locked to administrators only,
shipped alongside the binaries — the right approach absent a full
secrets-manager/vault integration, and how many production Windows services
handle a local secret in practice. Concrete implementation:

- File: `C:\Program Files\WindowsLLMManager\token.txt`, containing only the
  raw token string, no other content.
- **Lock it down with `icacls` at install time** so only `SYSTEM` and
  `Administrators` can read it — the Windows service itself typically runs as
  `LocalSystem`, which needs read access; interactive users (even local
  admins logging in normally) shouldn't casually `type` the file open unless
  they're deliberately elevating:
  ```powershell
  icacls "C:\Program Files\WindowsLLMManager\token.txt" /inheritance:r
  icacls "C:\Program Files\WindowsLLMManager\token.txt" /grant:r "SYSTEM:(R)"
  icacls "C:\Program Files\WindowsLLMManager\token.txt" /grant:r "BUILTIN\Administrators:(R)"
  ```
  `/inheritance:r` strips inherited permissions from the parent folder first
  (otherwise the default "Users: Read" on `Program Files` subfolders can
  still apply) — this step is what actually matters; the `/grant` lines just
  restore access for the two principals that legitimately need it.
- `agent.exe` reads the token from this file **once at service startup**,
  computes its SHA-256, and holds only the hash in memory for the lifetime of
  the process — it never needs to re-read the plaintext file to serve
  requests, which limits the window where the plaintext token exists in a
  readable location.
- The client side (Etapa 2 skill) needs its own copy of each host's token —
  supplied by the operator ad hoc per request, NOT fetched from the server.
  There's no way around the operator needing to know/distribute each host's
  token; treat those token values with the same care on whichever machine runs
  the coding agent.

- Every request checked via `Authorization: Bearer <token>` with
  `subtle.ConstantTimeCompare` against the stored hash (never `==` on
  secrets).

**Rate limiting & auto-blocklist:**

- Rate-limit incoming requests per source IP (e.g. N requests/second) as
  baseline defense-in-depth.
- **Auto-blocklist:** if an IP sends 5 requests with a wrong/missing token, add
  it to a blocklist. Every subsequent request from a blocklisted IP is
  silently ignored (dropped before auth processing) — no response, no further
  token checks. This stops token-guessing brute-force cold after 5 tries.
- **Blocklist management endpoints** (authenticated, admin-only):
  - `GET /blocklist` → returns all currently blocked IPs (with timestamp of
    when each was blocked and the failed-attempt count that triggered it).
  - `DELETE /blocklist/{ip}` → removes a specific IP from the blocklist (in
    case a legitimate client tripped it, e.g. after a token rotation
    mismatch).
- **Note the interaction with the tunnel:** if all traffic arrives via
  `cloudflared`, the source IP the agent sees may be the tunnel's, not the
  real client's — so the blocklist may need to key on a forwarded-client-IP
  header (e.g. `CF-Connecting-IP`) rather than the raw TCP source, or the
  blocklist becomes all-or-nothing for tunneled traffic. Decide based on the
  final tunnel/TLS config (open item).
- Log every failed-auth attempt and every auto-block regardless, so the audit
  trail (1.8) captures probing activity even before the blocklist trips.

## 1.5 Network exposure — tunnel topology

Recommended: **one `cloudflared` tunnel, one ingress config, multiple hostnames**,
running on a single machine that has LAN line-of-sight to all other managed hosts.
Only that one machine needs `cloudflared` installed.

```yaml
# config.yml on the tunnel-hosting machine
tunnel: <tunnel-id>
credentials-file: /path/to/creds.json

ingress:
  - hostname: host01.yourdomain.com
    service: https://10.0.0.11:8080
  - hostname: host02.yourdomain.com
    service: https://10.0.0.12:8080
  - hostname: host03.yourdomain.com
    service: https://10.0.0.13:8080
  - service: http_status:404   # required catch-all, must be last
```

DNS: each hostname gets a CNAME to `<tunnel-id>.cfargotunnel.com`
(`cloudflared tunnel route dns` does this automatically). *(Note: `service`
entries point at HTTPS per 1.4 — the exact origin-cert config for `cloudflared`
is an open item to finalize.)*

Tradeoff: the tunnel-hosting machine is a single point of failure for *reaching*
all agents (the agents themselves keep running independently if it goes down).
Alternative (tunnel-per-host, N tunnel processes) only worth it past roughly
50 machines or if machines aren't on a shared LAN.

**Note given 0.0.0.0 binding:** since each agent listens on all interfaces
rather than only `127.0.0.1`, the single-tunnel-fan-out model still works as
described (the tunnel-hosting machine's `service:` entries point at each agent's
LAN IP:port), but it also means each agent is independently reachable directly
over the LAN — the firewall scoping mentioned in 1.1 is what keeps that from
becoming an unauthenticated-adjacent attack surface, not the tunnel topology
itself.

## 1.6 Auto-update

**Problem this solves:** the agent will be deployed on X machines that can't
realistically be logged into individually to push updates/bugfixes.

**Design: two binaries, not one.**

- **`agent.exe`** — the server itself (everything in 1.1-1.5). Runs as the
  Windows service day-to-day.
- **`updater.exe`** — small, rarely-changing binary whose only job is:
  check for a newer release, download it, verify it, swap the file, restart
  the service. Kept deliberately minimal so it doesn't need updating often
  itself.

A single self-replacing exe is avoidable and not recommended: Windows locks a
running executable's file, so `agent.exe` cannot overwrite itself while
executing. Two binaries sidesteps this cleanly — `updater.exe` swaps
`agent.exe` while the service is stopped, so the file handle is already
released.

**Trigger mechanism:** `agent.exe` owns only a lightweight timer configured by
`update_check_interval_min` in `config.json` (default 20 minutes; 0 disables
automatic checks). On every tick it launches `updater.exe --check-only` as a
detached process and prevents overlapping launches. All GitHub, verification,
service-control and file-replacement logic remains isolated in `updater.exe`.
The detached updater survives when it stops the parent Windows service. A
legacy `WindowsLLMManagerUpdateCheck` Scheduled Task is removed during migration.

**Update flow:**

1. `updater.exe --check-only` calls the GitHub REST API,
   `GET /repos/{owner}/{repo}/releases/latest` (no auth needed for a public
   repo; unauthenticated rate limit is 60 req/hour per IP — comfortably
   sufficient at a 15-20 min poll interval regardless of fleet size, since
   each machine polls GitHub directly).
2. Compares the release tag against `agent.exe`'s embedded version string
   (build-time `-ldflags "-X main.version=1.4.2"`).
3. If newer: downloads the new `agent.exe` binary, its published checksum, and
   its signature to a temp path.
4. **Verifies checksum, then signature, before touching anything running.** If
   either verification fails, abort, delete the temp file, log the failure,
   exit — do not proceed to step 5.
5. Backs up the current binary: `agent.exe` → `agent.exe.bak`.
6. `Stop-Service -Name "WindowsLLMManager" -Force` — via the Service Control
   Manager, not by killing the process by PID, since the SCM guarantees the
   file handle is released before the call returns.
7. Replace `agent.exe` with the verified new binary.
8. `Start-Service -Name "WindowsLLMManager"`.
9. **Rollback check:** wait for the service to reach `RUNNING` state within a
   timeout (e.g. 30s). If it doesn't, restore `agent.exe.bak`, restart the
   service again, and log the failed update attempt — a machine should never
   end up down because an update didn't take.
10. On confirmed success, delete `agent.exe.bak` (or keep the last one only,
    overwriting on next successful update, for a one-generation rollback
    history).
11. **Kill-switch interaction (see 1.8):** before doing any of the above, the
    updater checks for the kill-switch flag file. If present, it skips the
    update entirely — an update must never silently un-brake a machine.

**Security — integrity verification is mandatory, not optional:**

Since this binary executes arbitrary PowerShell commands with admin rights
across the whole fleet, a compromised or MITM'd update is the single
highest-blast-radius failure mode in this entire design. Concretely:

- Publish a **SHA-256 checksum file** alongside every GitHub release asset
  (e.g. `agent.exe.sha256`, a text file containing just the hash). Generate
  it at build/release time: `sha256sum agent.exe > agent.exe.sha256` (or Go
  equivalent).
- `updater.exe` computes the SHA-256 of the downloaded file and compares
  against the published checksum **before** step 5. Any mismatch → abort, no
  exceptions, no "warn and continue."
- **Signing is REQUIRED, not optional** — sign each release with `cosign` in
  **key-based mode**, which fits a local (non-CI) build workflow:
  - One-time setup: `cosign generate-key-pair` → produces `cosign.key`
    (private — guard it) and `cosign.pub` (public).
  - At release time, after building `agent.exe` locally with your Go compiler:
    `cosign sign-blob --key cosign.key agent.exe --output-signature agent.exe.sig`
  - Each release therefore ships **three files**: `agent.exe`,
    `agent.exe.sha256` (cheap first gate — catches corrupted/incomplete
    downloads), and `agent.exe.sig` (the real security control — catches a
    maliciously substituted binary).
  - `updater.exe` has `cosign.pub` **embedded in it** (public key only — never
    the private key) and verifies:
    `cosign verify-blob --key cosign.pub --signature agent.exe.sig agent.exe`
  - Verification flow: checksum first (fast sanity check), then signature
    (authenticity). Signature mismatch → abort, never install.
  - **The private key `cosign.key` is the master key to the whole fleet** —
    anyone holding it can sign a malicious binary that every machine will
    accept and run as `LocalSystem`. Never commit it to the repo, never put it
    in the deploy zip, never embed it in any binary. Store it separately from
    the source, alongside your other secrets. Only `cosign.pub` is
    distributed (baked into `updater.exe`).

  *(Note: cosign also has a "keyless" mode that signs via GitHub Actions OIDC
  with no stored key — but that requires the build to run in CI, which doesn't
  fit a local build. Key-based mode above is the right choice for building on
  your own machine.)*
- Download over HTTPS only (GitHub Releases URLs are HTTPS by default) —
  this already protects against network-level MITM; checksum/signature
  verification protects against a compromised source.

**Install layout (ties into 1.4's token.txt placement):**

```
C:\Program Files\WindowsLLMManager\
├── agent.exe
├── updater.exe
├── token.txt          (ACL-locked, see 1.4)
├── agent.exe.bak       (present only between update attempts)
└── KILLED              (present only when kill-switch armed, see 1.8)
```

The service configuration owns the trigger interval and paths. `agent.exe`
runs as `LocalSystem`, launches the updater detached and keeps at most one
agent-triggered updater process active:

```json
{
  "update_check_interval_min": 20,
  "updater_path": "C:\\Program Files\\WindowsLLMManager\\updater.exe",
  "updater_config_path": "C:\\Program Files\\WindowsLLMManager\\updater-config.json"
}
```

## 1.7 Build & deployment workflow

**Goal:** a repeatable path from Go source to a provisioned machine, without
manual per-machine steps beyond "copy two files, run one command."

**`agent.exe --gen-token` — CLI switch, runs on the target machine (default flow) or at build time (shared-token flow)**

- Generates 32 random bytes via `crypto/rand`, base64url-encodes (no
  padding), writes the result to `token.txt` in the same directory as
  `agent.exe`.
- **Must not silently overwrite an existing `token.txt`** — if the file
  already exists, exit with a message ("token.txt already exists; use
  --gen-token --force to regenerate") rather than clobbering a token the
  operator's records still reference. A silent overwrite here breaks auth on a
  previously-working machine with no obvious cause.
- Prints the generated token to stdout on success, so whatever invokes it
  (`install.cmd`, or `deploy.cmd` in shared-token mode) can surface it to the
  operator.

**Two deployment modes — decided once, at `deploy.cmd`/package time, not per-machine at install time:**

- **Default (per-machine tokens):** the zip ships with no `token.txt`.
  `install.cmd` runs `--gen-token` fresh on every machine it's installed on —
  each machine ends up with its own unique token, preserving the isolation
  from 1.4 (one leak compromises only that machine).
- **`--shared-token` (fleet/factory token):** `deploy.cmd --shared-token`
  generates **one** token at build time (same `crypto/rand` process, just run
  once) and includes it as `token.txt` inside the zip. Every machine
  installed from *that specific zip* shares the one token. `install.cmd`
  detects the token.txt already present in the extracted files and uses it
  as-is, skipping `--gen-token` entirely.
- **Why this is decided at package time, not as an install.cmd prompt:**
  this guarantees every machine installed from the same zip is consistent —
  either the whole batch shares a token or none of them do. Surfacing the
  choice as an install-time prompt risks an operator picking the wrong mode
  on one machine within an otherwise-consistent batch, silently creating a
  mixed-trust fleet that's hard to reason about later.
- **Explicit tradeoff — state this consciously each time `--shared-token` is
  used:** a shared token collapses the "one leak = one machine" isolation
  back to "one leak = every machine provisioned from that zip." This is a
  legitimate convenience for genuinely homogeneous batches (e.g.
  provisioning 30 identical machines at once) — reserve it for that case, and
  default back to per-machine tokens for anything long-lived or
  heterogeneous, rather than reaching for `--shared-token` out of habit.

**`deploy.cmd` — build + package + signed release (run on your dev machine, not the target)**

*Prerequisites the script handles automatically (first-run setup):*

- **Check for `cosign`; if absent, download/install it.** (cosign is a single
  standalone binary — the script can fetch the Windows release from the
  Sigstore GitHub releases and drop it on PATH, or into the project folder.)
- **Check for the signing key pair; if absent, generate it once** via
  `cosign generate-key-pair`, producing `cosign.key` (private) and
  `cosign.pub` (public). This runs only the first time. **`cosign.key` must be
  stored outside the repo and never committed/zipped/embedded** (see 1.6);
  `cosign.pub` gets embedded into `updater.exe` at its build time.
- **Check for the GitHub CLI (`gh`) or a release-upload mechanism; if absent,
  install/configure it** — needed for the automated release step below.

*Build + package steps:*

1. Cross-compile: `set GOOS=windows&& set GOARCH=amd64&& go build -o agent.exe .`
   (and equivalently for `updater.exe` from its own source, with `cosign.pub`
   embedded via `-ldflags` or an embedded file).
2. Create/clean a `DEPLOY\` folder.
3. Copy `agent.exe` and `updater.exe` into `DEPLOY\`.
4. **If `--shared-token` was passed:** run `agent.exe --gen-token` once,
   targeting `DEPLOY\token.txt`, and print the generated token to the console
   now. **Otherwise (default):** no `token.txt` in the zip.
5. Zip the contents of `DEPLOY\` into `windows-llm-manager.zip`.
6. Generate `install.cmd` alongside the zip.

*Signed release to GitHub (for the auto-update mechanism):*

7. Generate the checksum: `sha256sum agent.exe > agent.exe.sha256` (or Go/
   PowerShell equivalent).
8. Sign the binary:
   `cosign sign-blob --key cosign.key agent.exe --output-signature agent.exe.sig`
9. **Publish the release to GitHub automatically** as part of `deploy.cmd` —
   create a new versioned release (tag matching `agent.exe`'s embedded version
   string) and upload all three artifacts: `agent.exe`, `agent.exe.sha256`,
   `agent.exe.sig`. E.g. via `gh release create <version> agent.exe
   agent.exe.sha256 agent.exe.sig`. This is what the fleet's `updater.exe`
   instances poll and pull from.

*Outputs — two distinct things:*

- **For provisioning NEW machines:** `windows-llm-manager.zip` + `install.cmd`
  (copy to target, run as admin).
- **For auto-updating EXISTING machines:** the signed GitHub release
  (`agent.exe` + `.sha256` + `.sig`), pulled automatically by `updater.exe`
  per 1.6 — no manual action per machine.

Whether the install batch is shared-token or per-machine is baked into the
zip's contents (step 4), not decided at install time.

**`install.cmd` — run on the target machine, as Administrator**

1. Check for and stop/remove any existing service registration first
   (`sc query WindowsLLMManager` → if present, `sc stop` + `sc delete`) —
   makes the installer safely re-runnable for redeploys/fixes, not just
   first-time installs.
2. Extract the zip contents to `C:\Program Files\WindowsLLMManager\`.
3. Check whether `token.txt` is already present after extraction:
   - **Present** (shared-token batch, or a redeploy over an existing
     install) → use it as-is, skip `--gen-token` entirely.
   - **Absent** (default per-machine batch, first install) → run
     `agent.exe --gen-token` to create one fresh on this machine.
   This single check is what makes `install.cmd` behave correctly for both
   deployment modes without needing its own switch — it just does whatever
   the zip's contents imply.
4. Apply the `icacls` lockdown from 1.4 to `token.txt`. **Also lock down the
   whole `WindowsLLMManager` folder and `updater.exe` specifically** so only
   `SYSTEM` and `Administrators` can write to them — since `updater.exe` runs
   as `LocalSystem` and pulls+executes remote binaries, if a lesser user could
   overwrite `updater.exe` (or `config.json`, or drop a
   malicious `agent.exe` into the folder), that's a privilege-escalation path
   bypassing every other control. Strip inherited write permissions
   (`icacls ... /inheritance:r`) on the folder and re-grant write only to
   `SYSTEM`/`Administrators`; normal users get read/execute at most.
5. Create the Windows Firewall inbound rule scoped per 1.1 (this is
   install-time, not a separate manual step).
6. Install the Windows service (`sc create` or via the `kardianos/service`
   binary's own install subcommand, whichever the agent implements) and
   start it.
7. Configure the agent-owned updater interval/paths from 1.6 and remove the
   legacy `WindowsLLMManagerUpdateCheck` Scheduled Task if present.
8. **Print the token to the console if it was freshly generated in step 3**
   (per-machine mode only — a shared token was already printed once at
   `deploy.cmd` time). This is the only convenient moment to retrieve a
   freshly generated per-machine token, since it's about to become unreadable
   to a non-elevated session. Instruct the operator to record it now for use
   in the client (Etapa 2).
9. Delete the zip file.
10. Self-delete: `install.cmd` cannot delete itself while running (Windows
    keeps the running script's file locked) — the last line hands the
    deletion off to a detached process that outlives the script:
    ```cmd
    start /b "" cmd /c del "%~f0"
    ```

**End-to-end operator flow, once this exists:** run `deploy.cmd` once per
release → copy `windows-llm-manager.zip` + `install.cmd` to a target machine
(or drop them somewhere all machines can reach, e.g. a network share) → run
`install.cmd` as admin → record the printed token for the client → done.
Subsequent updates after this point are handled entirely by 1.6's auto-update
mechanism; `install.cmd` is only needed again for brand-new machines or a
deliberate full redeploy.

## 1.8 Observability & control

**Audit logging (append-only, with rotation):**

- Every executed command is logged: timestamp, source (which token/client,
  by token ID/label not the token itself), the command, exit status, and
  whether it was `/exec` or session-based. This is both operational (what did
  the agent do overnight across N machines) and forensic (post-incident
  reconstruction).
- **Log rotation:** the active log file has a size cap (e.g. 50 MB — pick a
  value; 5-50 MB range is reasonable). When it exceeds the cap, it's
  compressed into a timestamped zip (`audit-YYYYMMDD-HHMMSS.zip`) and a fresh
  log started. Rotated zips accumulate indefinitely (compressed audit logs
  are tiny, so unbounded retention costs little disk) — or optionally prune
  beyond a retention window if preferred.
- Ideally also forward to existing central logging (Graylog/Wazuh) so audit
  trails survive even if a machine is compromised or wiped.

**Health endpoint:**

- Authenticated `GET /health` returning agent version, uptime, count of open
  sessions, kill-switch status, and basic status — so the fleet can be polled
  for liveness/version without running a PowerShell command on each machine.
  Essential for machines you can't log into individually.

**Kill-switch (arm remotely, disarm only locally):**

- **Mechanism — a flag file on disk**, e.g.
  `C:\Program Files\WindowsLLMManager\KILLED`. Its mere existence puts the
  agent into "refuse all exec" mode.
- **Arming (remote):** an authenticated endpoint `POST /killswitch` causes the
  service to **create the flag file itself**. This is the only thing the API
  can do to the kill-switch — arm it. It can be triggered remotely, including
  fan-out across many hosts, so a fleet can be braked fast when something goes
  wrong.
- **Disarming (local only, deliberate):** there is **no API path to remove the
  flag file**. Once armed, the only way back to normal operation is for an
  administrator to connect to the machine (RDP/console) and manually delete
  the file. This is intentional: arming the kill-switch means something is
  genuinely wrong, and recovery should require a conscious human action on the
  box, not something any client (or a compromised token) can undo over the
  network.
- **State lives on disk, not in memory** — so it survives restarts. On every
  startup (service recovery, reboot, or post-update start), the agent
  re-checks for the flag file and stays in refuse-all-exec mode if it's
  present. It's checked on every request, not just at startup.
- **Interaction with auto-update (critical):** `updater.exe` must also check
  for the flag file. If the kill-switch is armed, the updater must **not**
  proceed with an update — otherwise an update would silently defeat the
  brake by stopping/replacing/restarting the service back into a running
  state. When armed: skip the update, log it, leave the machine braked and
  untouched until the flag file is removed manually.
- The `KILLED` file lives in the ACL-locked `WindowsLLMManager` folder, so
  only `SYSTEM`/`Administrators` can create or delete it anyway — a normal
  user can't clear the brake even locally.
- While armed, `GET /health` should still respond (so the fleet can be polled
  to see which machines are braked) and report kill-switch status explicitly.

---
---

# ETAPA 2 — Klientská časť (skill pre kódovacieho agenta)

**For:** coding agent (Claude Code / OpenCode / similar)
**Deliverable:** a skill directory following the standard SKILL.md + bundled
resources pattern, no compiled client binary.
**Context:** this skill lets a coding agent administer the fleet of Windows
10/11 machines from Etapa 1, each running the Go HTTPS agent, reachable over
the network (directly or via Cloudflare Tunnel), authenticated per-request by a
bearer token. **Targets (IPs/hostnames) and their tokens are supplied by the
operator ad hoc in each request — they are NOT stored in the skill.** The skill
describes the *mechanics* (where the token goes, the call shapes), never the
*values*.

## 2.1 Deliverable structure

```
remote-windows-admin/
├── SKILL.md                    (required, <500 lines)
├── references/
│   └── api-contract.md         (full endpoint reference, loaded on demand)
└── scripts/
    ├── ps_exec.ps1              (helper: one-shot /exec call)
    ├── ps_session.ps1           (helper: open/exec/close a session)
    └── verify.ps1               (helper: run a read-only check)
```

Helper scripts are **PowerShell (`.ps1`), not bash** — the client/coding agent
runs on Windows, so `.sh`/curl wrappers wouldn't be portable. Use
`Invoke-RestMethod`/`Invoke-WebRequest` (or `curl.exe` which ships with modern
Windows) inside the `.ps1` helpers.

There is **no inventory file** in the skill — targets and tokens come from the
operator per request (see Context above), so shipping an example inventory
would wrongly imply the skill stores credentials.

Keep `SKILL.md` itself lean — connection contract, the format rule, the
verification discipline, and the multi-host pattern. Push the full endpoint-by-
endpoint reference (request/response shapes, all fields) into
`references/api-contract.md` and point to it from `SKILL.md`.

## 2.2 SKILL.md — required content

### 2.2.1 Frontmatter

- `name: remote-windows-admin`
- `description`: must state clearly (a) what it does — execute PowerShell
  commands and multi-step admin operations on remote Windows machines via
  the Go agent's HTTPS API — and (b) when to trigger — any request involving
  managing/administering one or more specific Windows machines by IP/hostname,
  deploying software, scheduled tasks, services, files, shortcuts on a remote
  Windows host, etc. Make the description assertive per skill-authoring
  convention — this skill should trigger even if the user doesn't say "use the
  remote admin skill" explicitly, whenever the request implies acting on a
  remote Windows machine this system manages.

### 2.2.2 Connection contract

- **Transport is HTTPS** (`https://<host>:<port>` or the tunnel hostname) —
  never plain HTTP. The agent may present a **self-signed / internal-CA
  certificate**; in that case the client should **ignore the TLS certificate
  validation error** (e.g. `Invoke-RestMethod -SkipCertificateCheck` in
  PowerShell 6+, or the equivalent) — the token, not the cert chain, is the
  trust anchor here. Do not refuse to connect over a self-signed cert.
- **Auth:** every request carries `Authorization: Bearer <token>`. The token
  value is supplied by the operator per request — the skill instructs the
  agent to take the target's token from the operator's instruction and place
  it in this header, nothing more. It does not look the token up anywhere or
  store it.
- The call shapes:
  - `POST /exec` — stateless one-shot (see 2.2.3 for format rule).
  - `POST /session`, `POST /session/{id}/exec`, `POST /session/{id}/restart`,
    `DELETE /session/{id}` — persistent session for multi-step work sharing
    state (e.g. `cd`, variables) across calls. **Max 5 concurrent sessions
    per agent** — if `POST /session` is refused for exceeding the cap, close
    an unused session rather than retrying blindly.
  - `GET /health` — liveness/version/open-session-count/kill-switch-status
    check; use it to confirm an agent is reachable and see its version before
    starting work, without running a PowerShell command.
  - `GET /blocklist`, `DELETE /blocklist/{ip}` — admin endpoints (see 2.2.8 for
    when these matter to the client).
  - `POST /killswitch` — **arms** the kill-switch on that agent (creates the
    on-disk flag file, stopping all exec). Arm-only: there is no endpoint to
    disarm; recovery is manual on the machine. Only ever call this on explicit
    operator instruction (see 2.2.8).
- When to use which: stateless `/exec` for independent checks/actions;
  session mode when steps depend on shared state (e.g. staying in a
  particular working directory across several commands).

### 2.2.3 Command timeout handling

Any single command can hang (waiting on input, an unreachable network mount,
a stuck process). The skill must instruct the agent that:

- Commands have a server-side timeout; a timed-out command returns a distinct
  error, not a normal result.
- On timeout, the agent must **not** assume the command succeeded or failed —
  it should treat state as unknown, run a read-only check to determine actual
  state before deciding what to do, and never blindly retry a
  non-idempotent action (see 2.2.6).

### 2.2.4 Output-format rule (mandatory, state explicitly — do not let the agent infer this from general PowerShell knowledge)

Two categories only, decided by the shape of the command's *output*, not by
the command's name:

- **Tabular/object-shaped output** (anything that's naturally a set of
  objects with fields — `Get-Process`, `Get-Service`, `Get-ChildItem`,
  `Get-ScheduledTask`, etc.) → request `"format": "json_object"`. The server
  handles the JSON conversion itself (safely, via stdin — not string
  concatenation); the agent should NOT manually pipe to `ConvertTo-Json` in
  the command string — just set the format flag.
- **Line-based/plain-text output** (raw text, log tails, single status
  strings, `ipconfig`-style output with no inherent object structure) →
  request `"format": "lines"`. Server returns raw stdout split into a
  `[]string`, one element per line.
- If genuinely unsure which category a command falls into, default to
  `"lines"` and parse defensively — safer than assuming structure that isn't
  there.

### 2.2.5 Terminal-compatibility constraint (mandatory, state explicitly)

The remote command runs inside an in-memory, console-less PowerShell process
— there is no visible window, no real console buffer, no human watching a
screen. The agent must only issue commands compatible with this:

- **Never** use or rely on: progress bars (`Write-Progress`), cursor
  positioning or screen-clearing (`Clear-Host`, `[Console]::SetCursorPosition`),
  interactive prompts expecting keyboard input (`Read-Host` without piped
  input, confirmation prompts — always pass `-Confirm:$false` / `-Force`
  where the cmdlet supports it to suppress interactive confirmation), or
  anything that assumes a rendered, human-observed terminal.
- **Always prefer** non-interactive, script-safe cmdlet forms: the same
  cmdlets you'd put in a `.ps1` run via Task Scheduler, not ones you'd only
  type while watching the screen.
- A wide default buffer (200 columns) is set server-side as a fallback, but
  the agent should still avoid `Format-Table`/`Format-List` for anything it
  intends to parse — use the JSON format flag instead (2.2.4).

### 2.2.6 Verification-before-proceeding discipline (mandatory)

No command whitelist — the agent has full latitude in *which* PowerShell
commands to use. Latitude is bounded instead by process:

For any multi-step operation (the canonical example: recreate a folder, copy
new software version into it, reschedule the old task off / register the new
Scheduled Task, remove old shortcut, create new shortcut):

1. Execute one step.
2. Immediately run a **read-only verification check** specific to that step
   before moving on — e.g.:
   - After `Copy-Item` → `Test-Path` the destination + `Get-ChildItem` to
     confirm expected files/sizes.
   - After `Register-ScheduledTask` / `Unregister-ScheduledTask` →
     `Get-ScheduledTask` to confirm the task exists (or doesn't) with the
     expected trigger/action.
   - After shortcut creation/deletion → `Test-Path` the `.lnk` file.
3. Only proceed to the next step if verification passes. If it fails, stop
   the sequence and report exactly which step failed and what the
   verification check returned — do not continue down a chain assuming
   success.
4. **Irreversible steps** (deleting the old folder, unregistering the old
   task, removing the old shortcut) are a distinct category: flag these
   explicitly in the plan reported back before executing, since a failed
   verification after an irreversible step can't be undone by re-running
   the previous step.

### 2.2.7 Idempotency

Operations may be re-run — e.g. a fan-out that failed partway on host 15 and
gets restarted, or a retry after a timeout (2.2.3). The skill must instruct the
agent to make each step **idempotent — check-then-act, not blind action:**

- Before `New-Item` on a folder → check if it already exists; create only if
  absent (or use `-Force` where semantically safe).
- Before registering a Scheduled Task → check if a task of that name already
  exists; update/replace rather than erroring or double-creating.
- Copying files → safe to repeat, but verify by hash/size rather than assuming
  a re-copy is identical.
- The goal: running the same operation twice on the same host leaves it in the
  same correct end state, never a broken or doubled one.

### 2.2.8 Distinguishing "server refuses me" from "command failed" (mandatory)

The agent must clearly separate two fundamentally different outcomes, because
they demand opposite responses:

**Category 1 — the server is refusing the request itself.** These are NOT
command failures and must NOT trigger verification, retries, or
"fix-it" attempts:

- **Self-inflicted blocklist:** if the client sends several requests with a
  wrong/missing token, the agent auto-blocks the client's IP after 5 failed
  attempts (Etapa 1, 1.4). Once blocked, all further requests are silently
  dropped. If the agent finds itself suddenly getting no/dropped responses
  after auth errors, it must recognize **it blocked itself** — this is
  expected protective behavior, not a fault to repair. The agent should stop,
  report that it appears to have been blocklisted (likely a wrong/stale
  token), and ask the operator for the correct token or to clear the block
  (`DELETE /blocklist/{ip}`) — it must NOT keep hammering, and must NOT try to
  "fix" the target machine, since nothing on the target is broken.
- **Auth rejection (401/403):** wrong token → surface it as an auth problem,
  ask the operator, don't retry blindly (retrying burns toward the blocklist
  threshold).
- **Kill-switch active:** "execution disabled" → operator has deliberately
  armed the kill-switch on this agent (a flag file on the machine). **This
  cannot be cleared remotely** — no API call, including anything the agent
  might try, will re-enable execution; only manual removal of the flag file on
  the machine itself (RDP/console, by an admin) restores it. So when the agent
  sees a kill-switch response, it must: stop targeting that host entirely,
  report it as "braked — requires manual intervention on the machine," and
  **not** retry, wait-and-poll, or attempt any workaround. `GET /health` still
  works on a braked host and reports kill-switch status, so the agent can
  confirm the state, but it must treat the host as out of service until the
  operator says otherwise.
- If the agent itself has access to `POST /killswitch`, it must understand
  that endpoint only **arms** the brake (creates the flag file) — it can never
  disarm it. The agent should only ever arm it if the operator explicitly
  instructs it to; it is not something the agent decides to do on its own.

**Category 2 — a command actually ran and failed** (non-zero exit, error
output, failed verification). These DO warrant the verification/diagnosis/
reporting discipline in 2.2.6.

The single most important rule: **a refusal from the server layer (auth,
blocklist, kill-switch) is never a reason to modify anything on the target
machine.** Confusing the two could lead the agent to "repair" a machine that
was never broken.

### 2.2.9 Multi-host fan-out pattern

- The operator supplies the set of targets and their tokens in the request
  (e.g. "run this on 10.0.0.11, .12, .13 with these tokens"). The skill does
  not store or look up a fleet list.
- When a request targets "all machines" or a named subset, iterate hosts
  **sequentially**, completing and verifying the full step sequence on one
  host before moving to the next — not because of performance, but so a
  failure on host 3 doesn't leave hosts 4-N in an undiagnosed state while
  host 3's problem is still being reported.
- Report results per-host at the end: which succeeded fully, which failed and
  at which step, with the verification output that revealed the failure.
- **Kill-switch during fan-out:** if, partway through a fan-out, remaining
  hosts start returning "execution disabled / refuse all exec" (the server's
  kill-switch, 1.8), the agent must recognize this as an operator-initiated
  stop signal for the whole batch — halt the fan-out and report where it
  stopped — NOT treat it as a per-host failure to diagnose or retry.

### 2.2.10 Pointer to full reference

Close `SKILL.md` with a note: "For full endpoint request/response schemas,
see `references/api-contract.md`."

## 2.3 references/api-contract.md — content

Full request/response JSON shapes for every endpoint, pulled from Etapa 1,
with example calls:

- `POST /exec` (with `format` field)
- `POST /session`, `POST /session/{id}/exec`, `POST /session/{id}/restart`,
  `DELETE /session/{id}`
- `GET /health`
- `GET /blocklist`, `DELETE /blocklist/{ip}`
- `POST /killswitch` (arm-only; no disarm endpoint by design)

For each, note the auth header, HTTPS + self-signed-cert handling, and the
error shapes that map to the Category-1 refusals in 2.2.8 (auth failure,
blocklisted, kill-switch active) so the client can distinguish them from
command failures.

## 2.4 Helper scripts — purpose (not full implementation)

Thin PowerShell wrappers (`.ps1`, since the client runs on Windows), not a
client binary — their only job is to save the agent from re-deriving the same
request every time, and to give a single place to fix auth-header/format/TLS
handling if the server API changes. Each uses `Invoke-RestMethod` (or
`curl.exe`) with `-SkipCertificateCheck` (or equivalent) for the self-signed
cert, and sets the `Authorization: Bearer` header from a passed-in token
argument.

- **`ps_exec.ps1 -BaseUrl -Token -Command -Format`** — wraps `POST /exec`,
  returns the parsed response.
- **`ps_session.ps1 -Action open|exec|close -BaseUrl -Token [-SessionId] [-Command]`**
  — wraps the session endpoints.
- **`verify.ps1 -BaseUrl -Token -CheckCommand`** — wrapper around `ps_exec.ps1`
  for the read-only verification calls in 2.2.6, keeping verification calls
  structurally distinct from action calls in transcripts.

## 2.5 Explicit non-goals

- No taxonomic whitelist of allowed PowerShell commands/cmdlets — the agent
  decides which commands to run; this skill governs *format* and *process*,
  not command selection.
- No compiled Go client executable — all connection logic is HTTPS requests
  described in the skill and the PowerShell helper scripts.
- No autonomous execution of irreversible steps without the explicit flagging
  step in 2.2.6.4 — the skill should make the agent surface irreversible steps
  in its plan, not silently execute them as part of a chain.

---
---

## Otvorené body (mimo rozsahu tejto revízie, doriešiť pred/pri implementácii)

1. **Cloudflare HTTPS/TLS routing** — presná konfigurácia `cloudflared` origin
   certov (aby tunel dôveroval self-signed/internal-CA certu agenta), a ako sa
   reálna IP klienta dostane k agentovi pre blocklist (`CF-Connecting-IP` vs
   raw TCP source). Blocklist per-IP funkcionalita (1.4) závisí od tohto.
