[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run rotate-token.cmd as Administrator.'
}

$serviceName = 'WindowsLLMManager'
$agentPath = Join-Path $PSScriptRoot 'agent.exe'
$tokenPath = Join-Path $PSScriptRoot 'token.txt'
$temporaryPath = Join-Path $PSScriptRoot ('.token-' + [Guid]::NewGuid().ToString('N') + '.tmp')
$backupPath = Join-Path $PSScriptRoot ('.token-' + [Guid]::NewGuid().ToString('N') + '.bak')

if (-not (Test-Path -LiteralPath $agentPath)) { throw "Agent not found: $agentPath" }
$service = Get-Service -Name $serviceName -ErrorAction Stop
$wasRunning = $service.Status -ne 'Stopped'
$oldTokenMoved = $false
$newTokenInstalled = $false

try {
    $newToken = ((& $agentPath --gen-token --token-output $temporaryPath) -join '').Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Token generation failed.' }
    if ($newToken -notmatch '^[A-Za-z0-9_-]{43}$') { throw 'The generated token has an invalid format.' }

    & icacls.exe $temporaryPath '/inheritance:r' '/grant:r' '*S-1-5-18:R' '*S-1-5-32-544:R' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Failed to lock the new token ACL.' }

    if ($wasRunning) {
        Stop-Service -Name $serviceName -Force
        (Get-Service -Name $serviceName).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }

    if (Test-Path -LiteralPath $tokenPath) {
        Move-Item -LiteralPath $tokenPath -Destination $backupPath
        $oldTokenMoved = $true
    }
    Move-Item -LiteralPath $temporaryPath -Destination $tokenPath
    $newTokenInstalled = $true

    if ($wasRunning) {
        Start-Service -Name $serviceName
        (Get-Service -Name $serviceName).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
    }

    if ($oldTokenMoved -and (Test-Path -LiteralPath $backupPath)) {
        Remove-Item -LiteralPath $backupPath -Force
        $oldTokenMoved = $false
    }

    Write-Host 'Token rotation completed successfully.'
    if (-not $wasRunning) { Write-Warning 'The service was already stopped and remains stopped. The new token takes effect when it starts.' }
    Write-Warning 'Record this new token now; the previous token is no longer valid:'
    Write-Host $newToken
} catch {
    $rotationError = $_
    try {
        $currentService = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
        if ($currentService -and $currentService.Status -ne 'Stopped') {
            Stop-Service -Name $serviceName -Force -ErrorAction SilentlyContinue
            $currentService.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
        }
        if ($newTokenInstalled -and (Test-Path -LiteralPath $tokenPath)) {
            Remove-Item -LiteralPath $tokenPath -Force
            $newTokenInstalled = $false
        }
        if ($oldTokenMoved -and (Test-Path -LiteralPath $backupPath)) {
            Move-Item -LiteralPath $backupPath -Destination $tokenPath
            $oldTokenMoved = $false
        }
        if ($wasRunning) {
            Start-Service -Name $serviceName
            (Get-Service -Name $serviceName).WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
        }
    } catch {
        Write-Warning "Token rollback also failed: $($_.Exception.Message)"
    }
    throw $rotationError
} finally {
    if (Test-Path -LiteralPath $temporaryPath) { Remove-Item -LiteralPath $temporaryPath -Force -ErrorAction SilentlyContinue }
    if (-not $oldTokenMoved -and (Test-Path -LiteralPath $backupPath)) { Remove-Item -LiteralPath $backupPath -Force -ErrorAction SilentlyContinue }
}
