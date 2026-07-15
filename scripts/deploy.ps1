[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^v?\d+\.\d+\.\d+$')]
    [string]$Version,

    [Parameter(Mandatory = $true)]
    [string]$GitHubOwner,

    [Parameter(Mandatory = $true)]
    [string]$GitHubRepository,

    [string]$TargetName,

    [string[]]$TargetIP = @(),
    [string]$ManifestPath = '',
    [string]$TrustedProxyIP = '',
    [string]$FirewallRemoteAddress = 'LocalSubnet',
    [ValidateRange(1, 65535)]
    [int]$Port = 8443,
    [string]$SecretsDirectory = (Join-Path $env:USERPROFILE '.windows-llm-manager-secrets'),
    [string]$OutputDirectory = '',
    [switch]$SharedToken,
    [string]$SharedTokenFile = '',
    [switch]$Publish,
    [switch]$Draft,
    [switch]$Prerelease,
    [string]$ReleaseNotes = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if ($ManifestPath) {
    $rows = @(Import-Csv -LiteralPath $ManifestPath)
    if ($rows.Count -eq 0) { throw 'The target manifest is empty.' }
    for ($index = 0; $index -lt $rows.Count; $index++) {
        $row = $rows[$index]
        $rowTargetName = if ($row.PSObject.Properties['TargetName']) { $row.TargetName } else { '' }
        if (-not $rowTargetName) { throw "Manifest row $($index + 1) has no TargetName." }
        $rowIPValue = if ($row.PSObject.Properties['TargetIP']) { $row.TargetIP } else { '' }
        $rowProxyValue = if ($row.PSObject.Properties['TrustedProxyIP']) { $row.TrustedProxyIP } else { '' }
        $rowFirewallValue = if ($row.PSObject.Properties['FirewallRemoteAddress']) { $row.FirewallRemoteAddress } else { '' }
        $rowIPs = if ($rowIPValue) { @($rowIPValue -split '[,;]' | ForEach-Object { $_.Trim() } | Where-Object { $_ }) } else { @() }
        $child = @{
            Version = $Version; GitHubOwner = $GitHubOwner; GitHubRepository = $GitHubRepository
            TargetName = $rowTargetName; TargetIP = $rowIPs; Port = $Port
            TrustedProxyIP = $(if ($rowProxyValue) { $rowProxyValue } else { $TrustedProxyIP })
            FirewallRemoteAddress = $(if ($rowFirewallValue) { $rowFirewallValue } else { $FirewallRemoteAddress })
            SecretsDirectory = $SecretsDirectory; OutputDirectory = $OutputDirectory; ReleaseNotes = $ReleaseNotes
        }
        if ($SharedToken) { $child.SharedToken = $true; $child.SharedTokenFile = $SharedTokenFile }
        if ($Publish -and $index -eq 0) { $child.Publish = $true }
        if ($Draft) { $child.Draft = $true }
        if ($Prerelease) { $child.Prerelease = $true }
        & $PSCommandPath @child
    }
    return
}
if (-not $TargetName) { throw 'Specify -TargetName or -ManifestPath.' }

$ProjectRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$SecretsDirectory = [IO.Path]::GetFullPath($SecretsDirectory)
if ($SecretsDirectory.StartsWith($ProjectRoot, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'SecretsDirectory must be outside the project repository.'
}
if (-not $OutputDirectory) { $OutputDirectory = Join-Path $SecretsDirectory 'packages' }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
if ($OutputDirectory.StartsWith($ProjectRoot, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'OutputDirectory contains host TLS private keys and must be outside the project/synchronized repository.'
}

$Version = $Version.TrimStart('v')
$Tag = "v$Version"
$SafeTarget = ($TargetName -replace '[^A-Za-z0-9._-]', '_')
$BuildRoot = Join-Path $ProjectRoot 'build'
$Stage = Join-Path $SecretsDirectory ("staging\$SafeTarget-$Tag-" + [Guid]::NewGuid().ToString('N'))
$Dist = Join-Path $OutputDirectory "$SafeTarget-$Tag"
$Tools = Join-Path $SecretsDirectory 'tools'
$CaCert = Join-Path $SecretsDirectory 'windows-llm-manager-ca.crt'
$CaKey = Join-Path $SecretsDirectory 'windows-llm-manager-ca.key'
$CosignPrefix = Join-Path $SecretsDirectory 'cosign'
$CosignKey = "$CosignPrefix.key"
$CosignPub = "$CosignPrefix.pub"

function Invoke-Native {
    param([Parameter(Mandatory = $true)][string]$FilePath, [Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$FilePath failed with exit code $LASTEXITCODE"
    }
}

function Protect-SecretsDirectory {
    New-Item -ItemType Directory -Force -Path $SecretsDirectory, $Tools, $OutputDirectory | Out-Null
    $account = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    Invoke-Native -FilePath icacls.exe -Arguments @($SecretsDirectory, '/inheritance:r', '/grant:r', "$account`:(OI)(CI)F", '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F') | Out-Null
}

function Find-OrInstallCosign {
    $existing = Get-Command cosign.exe -ErrorAction SilentlyContinue
    if ($existing) { return $existing.Source }
    $local = Join-Path $Tools 'cosign.exe'
    if (Test-Path -LiteralPath $local) { return $local }

    Write-Host 'Cosign was not found; downloading the official Windows release and verifying its published SHA-256 checksum.'
    $release = Invoke-RestMethod -Uri 'https://api.github.com/repos/sigstore/cosign/releases/latest' -Headers @{ 'User-Agent' = 'WindowsLLMManager-Deploy' }
    $binaryAsset = $release.assets | Where-Object name -eq 'cosign-windows-amd64.exe' | Select-Object -First 1
    $checksumAsset = $release.assets | Where-Object name -Match '^cosign.*checksums\.txt$' | Select-Object -First 1
    if (-not $binaryAsset -or -not $checksumAsset) { throw 'The current cosign release does not contain the expected Windows binary/checksum assets.' }
    $tempBinary = "$local.download"
    $tempChecksums = Join-Path $Tools 'cosign-checksums.txt'
    Invoke-RestMethod -Uri $binaryAsset.browser_download_url -OutFile $tempBinary
    Invoke-RestMethod -Uri $checksumAsset.browser_download_url -OutFile $tempChecksums
    $line = Get-Content -LiteralPath $tempChecksums | Where-Object { $_ -match '\s+cosign-windows-amd64\.exe$' } | Select-Object -First 1
    if (-not $line) { throw 'Unable to find cosign-windows-amd64.exe in the published checksum file.' }
    $expected = ($line -split '\s+')[0].ToUpperInvariant()
    $actual = (Get-FileHash -LiteralPath $tempBinary -Algorithm SHA256).Hash
    if ($actual -ne $expected) { throw 'Downloaded cosign.exe failed SHA-256 verification.' }
    Move-Item -LiteralPath $tempBinary -Destination $local -Force
    Remove-Item -LiteralPath $tempChecksums -Force
    return $local
}

function Ensure-GitHubCLI {
    $existing = Get-Command gh.exe -ErrorAction SilentlyContinue
    if ($existing) { return $existing.Source }
    if (-not $Publish) { return $null }
    $winget = Get-Command winget.exe -ErrorAction SilentlyContinue
    if (-not $winget) { throw 'GitHub CLI is required for -Publish. Install gh and run gh auth login.' }
    Invoke-Native -FilePath $winget.Source -Arguments @('install', '--id', 'GitHub.cli', '--exact', '--source', 'winget', '--accept-package-agreements', '--accept-source-agreements')
    $candidates = @(
        (Join-Path $env:ProgramFiles 'GitHub CLI\gh.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\GitHub CLI\gh.exe')
    )
    $found = $candidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
    if (-not $found) { throw 'GitHub CLI installation completed but gh.exe was not found. Open a new terminal and retry.' }
    return $found
}

Protect-SecretsDirectory
$Cosign = Find-OrInstallCosign
$Gh = Ensure-GitHubCLI

New-Item -ItemType Directory -Force -Path $BuildRoot | Out-Null
if (Test-Path -LiteralPath $Stage) { Remove-Item -LiteralPath $Stage -Recurse -Force }
if (Test-Path -LiteralPath $Dist) { Remove-Item -LiteralPath $Dist -Recurse -Force }
New-Item -ItemType Directory -Force -Path $Stage, $Dist | Out-Null

$PkiExe = Join-Path $BuildRoot 'pki.exe'
$env:GOTELEMETRY = 'off'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
Invoke-Native -FilePath go -Arguments @('build', '-buildvcs=false', '-trimpath', '-o', $PkiExe, '.\cmd\pki')

if (-not (Test-Path -LiteralPath $CaCert) -and -not (Test-Path -LiteralPath $CaKey)) {
    Write-Host 'Creating the internal CA. Its private key remains outside the repository.'
    Invoke-Native -FilePath $PkiExe -Arguments @('init-ca', '--cert', $CaCert, '--key', $CaKey)
} elseif (-not (Test-Path -LiteralPath $CaCert) -or -not (Test-Path -LiteralPath $CaKey)) {
    throw 'Only one CA file exists. Restore the matching pair; do not generate over a partial CA.'
}

if (-not (Test-Path -LiteralPath $CosignKey) -and -not (Test-Path -LiteralPath $CosignPub)) {
    Write-Host 'Creating the cosign key pair. Enter and retain a strong signing-key password.'
    Invoke-Native -FilePath $Cosign -Arguments @('generate-key-pair', '--output-key-prefix', $CosignPrefix)
} elseif (-not (Test-Path -LiteralPath $CosignKey) -or -not (Test-Path -LiteralPath $CosignPub)) {
    throw 'Only one cosign key file exists. Restore the matching pair.'
}

$CaFingerprint = (& $PkiExe fingerprint --cert $CaCert).Trim()
if ($LASTEXITCODE -ne 0) { throw 'Failed to calculate the CA fingerprint.' }
Copy-Item -LiteralPath $CaCert -Destination (Join-Path $ProjectRoot 'remote-windows-admin\references\internal-ca.pem') -Force
Set-Content -LiteralPath (Join-Path $ProjectRoot 'remote-windows-admin\references\ca-fingerprint.txt') -Value $CaFingerprint -Encoding ASCII

$AgentExe = Join-Path $Stage 'agent.exe'
$UpdaterExe = Join-Path $Stage 'updater.exe'
Invoke-Native -FilePath go -Arguments @('build', '-buildvcs=false', '-trimpath', '-ldflags', "-s -w -X main.version=$Version", '-o', $AgentExe, '.\cmd\agent')
$PublicKeyBase64 = [Convert]::ToBase64String([IO.File]::ReadAllBytes($CosignPub))
Invoke-Native -FilePath go -Arguments @('build', '-buildvcs=false', '-trimpath', '-ldflags', "-s -w -X main.version=$Version -X main.embeddedPublicKeyBase64=$PublicKeyBase64", '-o', $UpdaterExe, '.\cmd\updater')

$dnsArgs = @()
$ipArgs = @($TargetIP)
$parsedTargetIP = $null
if ([Net.IPAddress]::TryParse($TargetName, [ref]$parsedTargetIP)) {
    $ipArgs += $TargetName
} else {
    $dnsArgs += $TargetName
}
$issueArgs = @('issue', '--ca-cert', $CaCert, '--ca-key', $CaKey, '--cert', (Join-Path $Stage 'tls-cert.pem'), '--key', (Join-Path $Stage 'tls-key.pem'), '--name', $TargetName)
if ($dnsArgs.Count -gt 0) { $issueArgs += @('--dns', ($dnsArgs -join ',')) }
if ($ipArgs.Count -gt 0) { $issueArgs += @('--ip', (($ipArgs | Select-Object -Unique) -join ',')) }
Invoke-Native -FilePath $PkiExe -Arguments $issueArgs

$InstallDir = 'C:\Program Files\WindowsLLMManager'
$config = [ordered]@{
    listen_address = "0.0.0.0:$Port"
    token_path = "$InstallDir\token.txt"
    tls_cert_path = "$InstallDir\tls-cert.pem"
    tls_key_path = "$InstallDir\tls-key.pem"
    trusted_proxy_ip = $TrustedProxyIP
    max_sessions = 5
    idle_session_timeout_sec = 1800
    command_timeout_sec = 120
    max_output_bytes = 4194304
    max_request_bytes = 1048576
    rate_limit_per_sec = 10
    rate_limit_burst = 20
    auth_failures_before_block = 5
    audit_log_path = "$InstallDir\logs\audit.jsonl"
    audit_max_bytes = 52428800
    kill_switch_path = "$InstallDir\KILLED"
}
$config | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $Stage 'config.json') -Encoding UTF8

$updaterConfig = [ordered]@{
    github_owner = $GitHubOwner
    github_repository = $GitHubRepository
    agent_path = "$InstallDir\agent.exe"
    service_name = 'WindowsLLMManager'
    kill_switch_path = "$InstallDir\KILLED"
    log_path = "$InstallDir\logs\updater.log"
    check_timeout_sec = 60
    service_timeout_sec = 30
    agent_asset_name = 'agent.exe'
}
$updaterConfig | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $Stage 'updater-config.json') -Encoding UTF8

$tokenMode = if ($SharedToken) { 'shared' } else { 'per-machine' }
if ($SharedToken) {
    Write-Warning 'Shared token selected: disclosure compromises every host installed from this package.'
    if (-not $SharedTokenFile) { $SharedTokenFile = Join-Path $SecretsDirectory 'shared-token.txt' }
    $SharedTokenFile = [IO.Path]::GetFullPath($SharedTokenFile)
    if ($SharedTokenFile.StartsWith($ProjectRoot, [StringComparison]::OrdinalIgnoreCase)) { throw 'SharedTokenFile must be outside the repository.' }
    if (-not (Test-Path -LiteralPath $SharedTokenFile)) {
        Write-Warning "Creating a persistent shared fleet token at $SharedTokenFile. Record it now:"
        Invoke-Native -FilePath $AgentExe -Arguments @('--gen-token', '--token-output', $SharedTokenFile)
        Invoke-Native -FilePath icacls.exe -Arguments @($SharedTokenFile, '/inheritance:r', '/grant:r', '*S-1-5-18:R', '*S-1-5-32-544:R', "$([Security.Principal.WindowsIdentity]::GetCurrent().Name):R") | Out-Null
    }
    Copy-Item -LiteralPath $SharedTokenFile -Destination (Join-Path $Stage 'token.txt')
}
$settings = [ordered]@{
    target_name = $TargetName
    target_ips = @($ipArgs | Select-Object -Unique)
    firewall_remote_address = $FirewallRemoteAddress
    token_mode = $tokenMode
    ca_sha256_fingerprint = $CaFingerprint
}
$settings | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $Stage 'install-settings.json') -Encoding UTF8

$PackageName = "windows-llm-manager-$SafeTarget-$Tag.zip"
$PackagePath = Join-Path $Dist $PackageName
Compress-Archive -Path (Join-Path $Stage '*') -DestinationPath $PackagePath -CompressionLevel Optimal
Copy-Item -LiteralPath (Join-Path $ProjectRoot 'scripts\installer.ps1') -Destination (Join-Path $Dist 'install.ps1')
$cmd = "@echo off`r`npowershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File `"%~dp0install.ps1`" -PackagePath `"%~dp0$PackageName`" %*`r`nexit /b %errorlevel%`r`n"
Set-Content -LiteralPath (Join-Path $Dist 'install.cmd') -Value $cmd -Encoding ASCII

$ReleaseDir = Join-Path $BuildRoot "release-$Tag"
if (Test-Path -LiteralPath $ReleaseDir) { Remove-Item -LiteralPath $ReleaseDir -Recurse -Force }
New-Item -ItemType Directory -Force -Path $ReleaseDir | Out-Null
$ReleaseAgent = Join-Path $ReleaseDir 'agent.exe'
Copy-Item -LiteralPath $AgentExe -Destination $ReleaseAgent
$hash = (Get-FileHash -LiteralPath $ReleaseAgent -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath "$ReleaseAgent.sha256" -Value "$hash  agent.exe" -Encoding ASCII
Invoke-Native -FilePath $Cosign -Arguments @('sign-blob', '--yes', '--key', $CosignKey, '--output-signature', "$ReleaseAgent.sig", $ReleaseAgent)

if ($Publish) {
    Invoke-Native -FilePath $Gh -Arguments @('auth', 'status', '--hostname', 'github.com')
    $arguments = @('release', 'create', $Tag, $ReleaseAgent, "$ReleaseAgent.sha256", "$ReleaseAgent.sig", '--repo', "$GitHubOwner/$GitHubRepository", '--title', "Windows LLM Manager $Tag")
    if ($ReleaseNotes) { $arguments += @('--notes', $ReleaseNotes) } else { $arguments += @('--generate-notes') }
    if ($Draft) { $arguments += '--draft' }
    if ($Prerelease) { $arguments += '--prerelease' }
    Invoke-Native -FilePath $Gh -Arguments $arguments
}

Remove-Item -LiteralPath $Stage -Recurse -Force
Write-Host ''
Write-Host "Provisioning package: $PackagePath"
Write-Host "Target: $TargetName"
Write-Host "CA SHA-256: $CaFingerprint"
Write-Warning 'The provisioning ZIP contains a host TLS private key. Transfer it securely and let install.ps1 delete it after installation.'
if (-not $Publish) { Write-Host "Signed release assets (not published): $ReleaseDir" }
