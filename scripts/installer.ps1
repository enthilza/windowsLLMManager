[CmdletBinding()]
param(
    [string]$PackagePath,
    [string]$InstallDirectory = 'C:\Program Files\WindowsLLMManager',
    [switch]$KeepPackage,
    [switch]$AllowTargetMismatch
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run install.cmd as Administrator.' }

if (-not $PackagePath) {
    $packages = @(Get-ChildItem -LiteralPath $PSScriptRoot -Filter 'windows-llm-manager-*.zip')
    if ($packages.Count -ne 1) { throw 'Specify -PackagePath or leave exactly one provisioning ZIP beside install.ps1.' }
    $PackagePath = $packages[0].FullName
}
$PackagePath = [IO.Path]::GetFullPath($PackagePath)
if (-not (Test-Path -LiteralPath $PackagePath)) { throw "Package not found: $PackagePath" }

$temp = Join-Path $env:TEMP ("wlm-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $temp | Out-Null
try {
    Expand-Archive -LiteralPath $PackagePath -DestinationPath $temp
    $required = 'agent.exe', 'updater.exe', 'config.json', 'updater-config.json', 'tls-cert.pem', 'tls-key.pem', 'install-settings.json', 'rotate-token.cmd', 'rotate-token.ps1'
    foreach ($name in $required) {
        if (-not (Test-Path -LiteralPath (Join-Path $temp $name))) { throw "Package is missing $name" }
    }
    $settings = Get-Content -LiteralPath (Join-Path $temp 'install-settings.json') -Raw | ConvertFrom-Json
    $localNames = @($env:COMPUTERNAME, "$env:COMPUTERNAME.$env:USERDNSDOMAIN") | ForEach-Object { $_.TrimEnd('.') }
    $localIPs = @((Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue).IPAddress)
    $nameMatches = $settings.target_name -in $localNames
    $ipMatches = @($settings.target_ips | Where-Object { $_ -in $localIPs }).Count -gt 0
    if (-not $AllowTargetMismatch -and -not $nameMatches -and -not $ipMatches) {
        throw "This package is issued for '$($settings.target_name)', not '$env:COMPUTERNAME'. Use the correct host package or explicitly pass -AllowTargetMismatch."
    }

    if (Get-ScheduledTask -TaskName 'WindowsLLMManagerUpdateCheck' -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName 'WindowsLLMManagerUpdateCheck' -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName 'WindowsLLMManagerUpdateCheck' -Confirm:$false
    }
    $service = Get-Service -Name 'WindowsLLMManager' -ErrorAction SilentlyContinue
    if ($service) {
        if ($service.Status -ne 'Stopped') { Stop-Service -Name 'WindowsLLMManager' -Force }
        & sc.exe delete WindowsLLMManager | Out-Null
        for ($i = 0; $i -lt 20 -and (Get-Service -Name 'WindowsLLMManager' -ErrorAction SilentlyContinue); $i++) { Start-Sleep -Milliseconds 500 }
        if (Get-Service -Name 'WindowsLLMManager' -ErrorAction SilentlyContinue) { throw 'Timed out waiting for the old service registration to be deleted.' }
    }

    New-Item -ItemType Directory -Force -Path $InstallDirectory, (Join-Path $InstallDirectory 'logs') | Out-Null
    $tokenPath = Join-Path $InstallDirectory 'token.txt'
    $preservedToken = $null
    if ($settings.token_mode -eq 'per-machine' -and (Test-Path -LiteralPath $tokenPath)) { $preservedToken = (Get-Content -LiteralPath $tokenPath -Raw).Trim() }
    Get-ChildItem -LiteralPath $temp -File | Where-Object Name -ne 'install-settings.json' | ForEach-Object {
        Copy-Item -LiteralPath $_.FullName -Destination (Join-Path $InstallDirectory $_.Name) -Force
    }
    $agentConfigPath = Join-Path $InstallDirectory 'config.json'
    $agentConfig = Get-Content $agentConfigPath -Raw | ConvertFrom-Json
    $agentConfig.token_path = Join-Path $InstallDirectory 'token.txt'
    $agentConfig.tls_cert_path = Join-Path $InstallDirectory 'tls-cert.pem'
    $agentConfig.tls_key_path = Join-Path $InstallDirectory 'tls-key.pem'
    $agentConfig.audit_log_path = Join-Path $InstallDirectory 'logs\audit.jsonl'
    $agentConfig.kill_switch_path = Join-Path $InstallDirectory 'KILLED'
    $agentConfig | Add-Member -NotePropertyName updater_path -NotePropertyValue (Join-Path $InstallDirectory 'updater.exe') -Force
    $agentConfig | Add-Member -NotePropertyName updater_config_path -NotePropertyValue (Join-Path $InstallDirectory 'updater-config.json') -Force
    $agentConfig | ConvertTo-Json | Set-Content -LiteralPath $agentConfigPath -Encoding UTF8
    $updaterConfigPath = Join-Path $InstallDirectory 'updater-config.json'
    $updaterConfig = Get-Content $updaterConfigPath -Raw | ConvertFrom-Json
    $updaterConfig.agent_path = Join-Path $InstallDirectory 'agent.exe'
    $updaterConfig.kill_switch_path = Join-Path $InstallDirectory 'KILLED'
    $updaterConfig.log_path = Join-Path $InstallDirectory 'logs\updater.log'
    $updaterConfig | ConvertTo-Json | Set-Content -LiteralPath $updaterConfigPath -Encoding UTF8
    if ($preservedToken) { Set-Content -LiteralPath $tokenPath -Value $preservedToken -Encoding ASCII }

    $freshToken = $false
    $token = $null
    if (-not (Test-Path -LiteralPath $tokenPath)) {
        $token = & (Join-Path $InstallDirectory 'agent.exe') --gen-token --token-output $tokenPath
        if ($LASTEXITCODE -ne 0) { throw 'Token generation failed.' }
        $freshToken = $true
    }

    & icacls.exe $InstallDirectory '/inheritance:r' '/grant:r' '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-32-545:(OI)(CI)RX' | Out-Null
    foreach ($secret in 'token.txt', 'tls-key.pem') {
        $path = Join-Path $InstallDirectory $secret
        & icacls.exe $path '/inheritance:r' '/grant:r' '*S-1-5-18:R' '*S-1-5-32-544:R' | Out-Null
    }

    Get-NetFirewallRule -DisplayName 'Windows LLM Manager HTTPS' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    $localPort = $agentConfig.listen_address.Split(':')[-1]
    New-NetFirewallRule -DisplayName 'Windows LLM Manager HTTPS' -Direction Inbound -Action Allow -Protocol TCP -LocalPort $localPort -RemoteAddress $settings.firewall_remote_address | Out-Null

    $binaryPath = '"' + (Join-Path $InstallDirectory 'agent.exe') + '" --config "' + (Join-Path $InstallDirectory 'config.json') + '"'
    New-Service -Name 'WindowsLLMManager' -BinaryPathName $binaryPath -DisplayName 'Windows LLM Manager' -Description 'Authenticated HTTPS endpoint for non-interactive administrative PowerShell.' -StartupType Automatic | Out-Null
    & sc.exe failure WindowsLLMManager reset= 86400 actions= restart/5000/restart/15000/restart/60000 | Out-Null

    Start-Service -Name 'WindowsLLMManager'

    Write-Host 'Windows LLM Manager installed and running.'
    Write-Host "CA SHA-256: $($settings.ca_sha256_fingerprint)"
    if ($freshToken) {
        Write-Warning 'Record this token now; it will not be printed again:'
        Write-Host $token
    } elseif ($settings.token_mode -in @('packaged', 'shared')) {
        Write-Warning 'Record this package-provided token now:'
        Write-Host (Get-Content -LiteralPath $tokenPath -Raw).Trim()
    } else {
        Write-Host 'The existing per-machine token was preserved.'
    }
}
finally {
    if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Recurse -Force }
}

if (-not $KeepPackage) {
    Remove-Item -LiteralPath $PackagePath -Force
    Write-Host 'The provisioning ZIP containing the host TLS private key was deleted.'
}
