[CmdletBinding()]
param(
    [ValidatePattern('^v?\d+\.\d+\.\d+$')]
    [string]$Version = '',

    [string]$GitHubOwner = 'enthilza',

    [string]$GitHubRepository = 'windowsLLMManager',

    [string]$TargetName,

    [string[]]$TargetIP = @(),
    [string]$ManifestPath = '',
    [string]$TrustedProxyIP = '',
    [string]$FirewallRemoteAddress = 'LocalSubnet',
    [ValidateRange(1, 65535)]
    [int]$Port = 8443,
    [string]$SecretsDirectory = (Join-Path $env:USERPROFILE '.windows-llm-manager-secrets'),
    [string]$OutputDirectory = '',
    [switch]$IncludeToken,
    [switch]$SharedToken,
    [string]$SharedTokenFile = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProjectRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))

if (-not $Version) {
    $versionPath = Join-Path $ProjectRoot 'VERSION'
    if (-not (Test-Path -LiteralPath $versionPath)) { throw 'VERSION is missing. Restore it or pass -Version explicitly.' }
    $Version = (Get-Content -LiteralPath $versionPath -Raw).Trim()
}

$interactive = $PSBoundParameters.Count -eq 0
if ($interactive) {
    Write-Host 'Windows LLM Manager - vytvorenie instalacneho balika'
    Write-Host ''

    while ($true) {
        $tokenAnswer = (Read-Host 'Vygenerovat token a vlozit ho do balika? [a/N]').Trim().ToLowerInvariant()
        if ($tokenAnswer -in @('', 'n', 'nie', 'no')) { break }
        if ($tokenAnswer -in @('a', 'ano', 'y', 'yes')) { $IncludeToken = $true; break }
        Write-Warning 'Zadaj A pre ano alebo N pre nie.'
    }

    while ($true) {
        $ipText = (Read-Host 'IP adresa cieloveho Windows PC').Trim()
        $parsedIP = $null
        if ([Net.IPAddress]::TryParse($ipText, [ref]$parsedIP) -and $parsedIP.AddressFamily -eq [Net.Sockets.AddressFamily]::InterNetwork) {
            $TargetName = $parsedIP.ToString()
            $TargetIP = @($TargetName)
            break
        }
        Write-Warning 'Zadaj platnu IPv4 adresu, napriklad 192.168.1.50.'
    }
    Write-Host ''
}

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
            SecretsDirectory = $SecretsDirectory; OutputDirectory = $OutputDirectory
        }
        if ($IncludeToken) { $child.IncludeToken = $true }
        if ($SharedToken) { $child.SharedToken = $true; $child.SharedTokenFile = $SharedTokenFile }
        & $PSCommandPath @child
    }
    return
}
if (-not $TargetName) { throw 'Specify -TargetName or -ManifestPath.' }
if ($IncludeToken -and $SharedToken) { throw 'Use either -IncludeToken or -SharedToken, not both.' }

$SecretsDirectory = [IO.Path]::GetFullPath($SecretsDirectory)
if ($SecretsDirectory.StartsWith($ProjectRoot, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'SecretsDirectory must be outside the project repository.'
}
if (-not $OutputDirectory) { $OutputDirectory = Join-Path $ProjectRoot 'deployPackage' }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
if ($OutputDirectory.StartsWith($ProjectRoot, [StringComparison]::OrdinalIgnoreCase)) {
    Write-Warning 'The provisioning package is inside the project. Git ignores deployPackage, but a sync client such as Nextcloud may still upload its TLS private key and packaged token.'
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
$RequiredCosignVersion = 'v3.1.1'

function Invoke-Native {
    param([Parameter(Mandatory = $true)][string]$FilePath, [Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$FilePath failed with exit code $LASTEXITCODE"
    }
}

function Protect-SecretsDirectory {
    New-Item -ItemType Directory -Force -Path $SecretsDirectory, $Tools | Out-Null
    $account = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    Invoke-Native -FilePath icacls.exe -Arguments @($SecretsDirectory, '/inheritance:r', '/grant:r', "$account`:(OI)(CI)F", '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F') | Out-Null
}

function Protect-OutputDirectory {
    New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
    $account = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    Invoke-Native -FilePath icacls.exe -Arguments @($OutputDirectory, '/inheritance:r', '/grant:r', "$account`:(OI)(CI)F", '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F') | Out-Null
}

function Find-OrInstallCosign {
    $local = Join-Path $Tools 'cosign.exe'
    if (Test-Path -LiteralPath $local) {
        try {
            $installedVersion = (& $local version --json | ConvertFrom-Json).gitVersion
            if ($LASTEXITCODE -eq 0 -and $installedVersion -eq $RequiredCosignVersion) { return $local }
        } catch {
            Write-Warning "The cached cosign executable could not be identified and will be replaced."
        }
    }

    Write-Host "Downloading cosign $RequiredCosignVersion and verifying its published SHA-256 checksum."
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/sigstore/cosign/releases/tags/$RequiredCosignVersion" -Headers @{ 'User-Agent' = 'WindowsLLMManager-Deploy' }
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

Protect-SecretsDirectory
Protect-OutputDirectory
$Cosign = Find-OrInstallCosign

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

$CodexHome = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $env:USERPROFILE '.codex' }
$SkillInstallations = @(
    [pscustomobject]@{ Agent = 'Codex'; Path = (Join-Path ([IO.Path]::GetFullPath($CodexHome)) 'skills\remote-windows-admin') },
    [pscustomobject]@{ Agent = 'Claude Code'; Path = (Join-Path $env:USERPROFILE '.claude\skills\remote-windows-admin') }
)
foreach ($installation in $SkillInstallations) {
    if (Test-Path -LiteralPath (Join-Path $installation.Path 'SKILL.md')) {
        try {
            $InstalledReferences = Join-Path $installation.Path 'references'
            New-Item -ItemType Directory -Force -Path $InstalledReferences | Out-Null
            Copy-Item -LiteralPath $CaCert -Destination (Join-Path $InstalledReferences 'internal-ca.pem') -Force
            Set-Content -LiteralPath (Join-Path $InstalledReferences 'ca-fingerprint.txt') -Value $CaFingerprint -Encoding ASCII
            Write-Host "Updated installed $($installation.Agent) skill CA trust: $($installation.Path)"
        } catch {
            Write-Warning "Unable to update the installed $($installation.Agent) skill CA trust: $($_.Exception.Message)"
        }
    }
}

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

$tokenMode = if ($SharedToken) { 'shared' } elseif ($IncludeToken) { 'packaged' } else { 'per-machine' }
if ($IncludeToken) {
    Invoke-Native -FilePath $AgentExe -Arguments @('--gen-token', '--token-output', (Join-Path $Stage 'token.txt')) | Out-Null
} elseif ($SharedToken) {
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
Copy-Item -LiteralPath (Join-Path $ProjectRoot 'scripts\rotate-token.cmd') -Destination (Join-Path $Stage 'rotate-token.cmd')
Copy-Item -LiteralPath (Join-Path $ProjectRoot 'scripts\rotate-token.ps1') -Destination (Join-Path $Stage 'rotate-token.ps1')

$PackageName = "windows-llm-manager-$SafeTarget-$Tag.zip"
$PackagePath = Join-Path $Dist $PackageName
Compress-Archive -Path (Join-Path $Stage '*') -DestinationPath $PackagePath -CompressionLevel Optimal
Copy-Item -LiteralPath (Join-Path $ProjectRoot 'scripts\installer.ps1') -Destination (Join-Path $Dist 'install.ps1')
$cmd = "@echo off`r`npowershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File `"%~dp0install.ps1`" -PackagePath `"%~dp0$PackageName`" %*`r`nexit /b %errorlevel%`r`n"
Set-Content -LiteralPath (Join-Path $Dist 'install.cmd') -Value $cmd -Encoding ASCII

Remove-Item -LiteralPath $Stage -Recurse -Force
Write-Host ''
Write-Host "Provisioning package: $PackagePath"
Write-Host "Target IP: $TargetName"
Write-Host "Token: $(if ($IncludeToken) { 'included in package' } elseif ($SharedToken) { 'shared token included' } else { 'generated by install.cmd on target' })"
Write-Host "CA SHA-256: $CaFingerprint"
Write-Warning 'The provisioning ZIP contains a host TLS private key. Transfer it securely and let install.ps1 delete it after installation.'
