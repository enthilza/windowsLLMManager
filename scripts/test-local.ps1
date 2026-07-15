[CmdletBinding()]
param([switch]$KeepArtifacts)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$work = Join-Path $root 'build\local-e2e'
$agent = Join-Path $work 'agent.exe'
$pki = Join-Path $work 'pki.exe'
$process = $null
if (Test-Path -LiteralPath $work) { Remove-Item -LiteralPath $work -Recurse -Force }
New-Item -ItemType Directory -Force -Path $work, (Join-Path $work 'logs') | Out-Null

try {
    $env:GOTELEMETRY = 'off'
    & go build -buildvcs=false -trimpath -ldflags '-X main.version=0.0.0-test' -o $agent '.\cmd\agent'
    if ($LASTEXITCODE -ne 0) { throw 'Agent build failed.' }
    & go build -buildvcs=false -trimpath -o $pki '.\cmd\pki'
    if ($LASTEXITCODE -ne 0) { throw 'PKI build failed.' }

    $caCert = Join-Path $work 'ca.crt'
    $caKey = Join-Path $work 'ca.key'
    $fingerprint = (& $pki init-ca --cert $caCert --key $caKey).Trim()
    & $pki issue --ca-cert $caCert --ca-key $caKey --cert (Join-Path $work 'tls-cert.pem') --key (Join-Path $work 'tls-key.pem') --name '127.0.0.1' --ip '127.0.0.1'
    if ($LASTEXITCODE -ne 0) { throw 'Leaf certificate generation failed.' }
    $token = (& $agent --gen-token --token-output (Join-Path $work 'token.txt')).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Token generation failed.' }

    $config = [ordered]@{
        listen_address = '127.0.0.1:18443'
        token_path = (Join-Path $work 'token.txt')
        tls_cert_path = (Join-Path $work 'tls-cert.pem')
        tls_key_path = (Join-Path $work 'tls-key.pem')
        trusted_proxy_ip = ''
        max_sessions = 5
        idle_session_timeout_sec = 60
        command_timeout_sec = 2
        max_output_bytes = 1048576
        max_request_bytes = 1048576
        rate_limit_per_sec = 20
        rate_limit_burst = 40
        auth_failures_before_block = 5
        audit_log_path = (Join-Path $work 'logs\audit.jsonl')
        audit_max_bytes = 1048576
        kill_switch_path = (Join-Path $work 'KILLED')
    }
    $config | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $work 'config.json') -Encoding UTF8

    $process = Start-Process -FilePath $agent -ArgumentList '--console', '--config', (Join-Path $work 'config.json') -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $work 'stdout.log') -RedirectStandardError (Join-Path $work 'stderr.log')
    . (Join-Path $root 'remote-windows-admin\scripts\_common.ps1')
    $baseUrl = [uri]'https://127.0.0.1:18443'
    $health = $null
    for ($i = 0; $i -lt 30 -and -not $health; $i++) {
        Start-Sleep -Milliseconds 250
        if ($process.HasExited) {
            $startupError = Get-Content -LiteralPath (Join-Path $work 'stderr.log') -Raw -ErrorAction SilentlyContinue
            throw "Agent exited during startup: $startupError"
        }
        try { $health = Invoke-WlmRequest -Method GET -Path '/health' -BaseUrl $baseUrl -Token $token -CAPath $caCert -CAFingerprint $fingerprint } catch { }
    }
    if (-not $health -or $health.status -ne 'ok') { throw 'Agent did not become healthy with pinned-CA validation.' }
    try {
        Invoke-WlmRequest -Method GET -Path '/health' -BaseUrl $baseUrl -Token $token -CAPath $caCert -CAFingerprint ('0' * 64) | Out-Null
        throw 'The helper accepted an incorrect CA fingerprint.'
    } catch {
        if ($_.Exception.Message -notmatch 'does not match the pinned CA fingerprint') { throw }
    }

    $lineResult = & (Join-Path $root 'remote-windows-admin\scripts\ps_exec.ps1') -BaseUrl $baseUrl -Token $token -Command "Write-Output 'e2e-ok'" -Format lines -CAPath $caCert -CAFingerprint $fingerprint
    if (-not $lineResult.execution.success -or $lineResult.execution.output[0] -ne 'e2e-ok') { throw 'One-shot execution failed.' }

    $opened = & (Join-Path $root 'remote-windows-admin\scripts\ps_session.ps1') -Action open -BaseUrl $baseUrl -Token $token -CAPath $caCert -CAFingerprint $fingerprint
    & (Join-Path $root 'remote-windows-admin\scripts\ps_session.ps1') -Action exec -BaseUrl $baseUrl -Token $token -SessionId $opened.session_id -Command '$E2EValue=41' -Format lines -CAPath $caCert -CAFingerprint $fingerprint | Out-Null
    $sessionResult = & (Join-Path $root 'remote-windows-admin\scripts\ps_session.ps1') -Action exec -BaseUrl $baseUrl -Token $token -SessionId $opened.session_id -Command '$E2EValue+1' -Format lines -CAPath $caCert -CAFingerprint $fingerprint
    if ($sessionResult.execution.output[0] -ne '42') { throw 'Persistent session state test failed.' }

    $armed = Invoke-WlmRequest -Method POST -Path '/killswitch' -BaseUrl $baseUrl -Token $token -CAPath $caCert -CAFingerprint $fingerprint
    if (-not $armed.armed) { throw 'Kill-switch did not arm.' }
    $braked = Invoke-WlmRequest -Method GET -Path '/health' -BaseUrl $baseUrl -Token $token -CAPath $caCert -CAFingerprint $fingerprint
    if (-not $braked.kill_switch_armed -or $braked.open_sessions -ne 0) { throw 'Kill-switch did not kill sessions or report braked state.' }
    try {
        & (Join-Path $root 'remote-windows-admin\scripts\ps_exec.ps1') -BaseUrl $baseUrl -Token $token -Command 'Get-Date' -Format lines -CAPath $caCert -CAFingerprint $fingerprint | Out-Null
        throw 'Execution unexpectedly succeeded while kill-switch was armed.'
    } catch {
        if ($_.Exception.Message -notmatch '423 killswitch_active') { throw }
    }

    Remove-Item -LiteralPath (Join-Path $work 'KILLED') -Force
    Write-Host 'Local HTTPS/CA/API/session/kill-switch integration test passed.'
}
finally {
    if ($process -and -not $process.HasExited) {
        Stop-Process -Id $process.Id -Force
        [void]$process.WaitForExit(5000)
    }
    if (-not $KeepArtifacts -and (Test-Path -LiteralPath $work)) {
        for ($attempt = 0; $attempt -lt 10 -and (Test-Path -LiteralPath $work); $attempt++) {
            try { Remove-Item -LiteralPath $work -Recurse -Force } catch { Start-Sleep -Milliseconds 250 }
        }
        if (Test-Path -LiteralPath $work) { throw "Unable to clean local integration-test artifacts at $work" }
    }
}
