[CmdletBinding()]
param(
    [ValidatePattern('^v?\d+\.\d+\.\d+$')]
    [string]$Version = '',
    [string]$GitHubOwner = 'enthilza',
    [string]$GitHubRepository = 'windowsLLMManager',
    [string]$SecretsDirectory = (Join-Path $env:USERPROFILE '.windows-llm-manager-secrets'),
    [switch]$Publish,
    [switch]$Draft,
    [switch]$Prerelease,
    [string]$ReleaseNotes = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$ProjectRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$versionPath = Join-Path $ProjectRoot 'VERSION'
if (-not (Test-Path -LiteralPath $versionPath)) { throw 'VERSION is missing.' }
$repositoryVersion = (Get-Content -LiteralPath $versionPath -Raw).Trim().TrimStart('v')
if (-not $Version) {
    $Version = $repositoryVersion
}
$Version = $Version.TrimStart('v')
$Tag = "v$Version"
if ($Publish -and $Version -ne $repositoryVersion) {
    throw "VERSION contains '$repositoryVersion'. Update and commit VERSION before publishing $Tag."
}

$headCommit = $null
if ($Publish) {
    $dirty = @(& git -C $ProjectRoot status --porcelain)
    if ($LASTEXITCODE -ne 0) { throw 'Unable to inspect the Git worktree.' }
    if ($dirty.Count -gt 0) { throw 'Refusing to publish from a dirty worktree. Commit and push the release source first.' }
    $headCommit = (& git -C $ProjectRoot rev-parse HEAD).Trim()
    $remoteCommit = (& git -C $ProjectRoot rev-parse '@{upstream}').Trim()
    if ($LASTEXITCODE -ne 0 -or $headCommit -ne $remoteCommit) { throw 'The current commit is not pushed to its upstream branch.' }
}

$SecretsDirectory = [IO.Path]::GetFullPath($SecretsDirectory)
if ($SecretsDirectory.StartsWith($ProjectRoot, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'SecretsDirectory must be outside the project repository.'
}
$Tools = Join-Path $SecretsDirectory 'tools'
$CosignPrefix = Join-Path $SecretsDirectory 'cosign'
$CosignKey = "$CosignPrefix.key"
$CosignPub = "$CosignPrefix.pub"
$RequiredCosignVersion = 'v3.1.1'
$ReleaseDir = Join-Path $ProjectRoot "build\release-$Tag"

function Invoke-Native {
    param([Parameter(Mandatory = $true)][string]$FilePath, [Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$FilePath failed with exit code $LASTEXITCODE" }
}

function Protect-SecretsDirectory {
    New-Item -ItemType Directory -Force -Path $SecretsDirectory, $Tools | Out-Null
    $account = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    Invoke-Native -FilePath icacls.exe -Arguments @($SecretsDirectory, '/inheritance:r', '/grant:r', "$account`:(OI)(CI)F", '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F') | Out-Null
}

function Find-OrInstallCosign {
    $local = Join-Path $Tools 'cosign.exe'
    if (Test-Path -LiteralPath $local) {
        try {
            $installedVersion = (& $local version --json | ConvertFrom-Json).gitVersion
            if ($LASTEXITCODE -eq 0 -and $installedVersion -eq $RequiredCosignVersion) { return $local }
        } catch {
            Write-Warning 'The cached cosign executable could not be identified and will be replaced.'
        }
    }
    Write-Host "Downloading cosign $RequiredCosignVersion and verifying its published SHA-256 checksum."
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/sigstore/cosign/releases/tags/$RequiredCosignVersion" -Headers @{ 'User-Agent' = 'WindowsLLMManager-Release' }
    $binaryAsset = $release.assets | Where-Object name -eq 'cosign-windows-amd64.exe' | Select-Object -First 1
    $checksumAsset = $release.assets | Where-Object name -Match '^cosign.*checksums\.txt$' | Select-Object -First 1
    if (-not $binaryAsset -or -not $checksumAsset) { throw 'The pinned cosign release does not contain the expected Windows binary/checksum assets.' }
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

function Find-GitHubCLI {
    $existing = Get-Command gh.exe -ErrorAction SilentlyContinue
    if ($existing) { return $existing.Source }
    $candidates = @(
        (Join-Path $env:ProgramFiles 'GitHub CLI\gh.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\GitHub CLI\gh.exe')
    )
    $found = $candidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
    if (-not $found) { throw 'GitHub CLI is required for -Publish. Install gh and run gh auth login.' }
    return $found
}

Protect-SecretsDirectory
$Cosign = Find-OrInstallCosign
if (-not (Test-Path -LiteralPath $CosignKey) -and -not (Test-Path -LiteralPath $CosignPub)) {
    Write-Host 'Creating the cosign key pair. Enter and retain a strong signing-key password.'
    Invoke-Native -FilePath $Cosign -Arguments @('generate-key-pair', '--output-key-prefix', $CosignPrefix)
} elseif (-not (Test-Path -LiteralPath $CosignKey) -or -not (Test-Path -LiteralPath $CosignPub)) {
    throw 'Only one cosign key file exists. Restore the matching pair.'
}

if (Test-Path -LiteralPath $ReleaseDir) { Remove-Item -LiteralPath $ReleaseDir -Recurse -Force }
New-Item -ItemType Directory -Force -Path $ReleaseDir | Out-Null
$ReleaseAgent = Join-Path $ReleaseDir 'agent.exe'
$env:GOTELEMETRY = 'off'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
Invoke-Native -FilePath go -Arguments @('build', '-buildvcs=false', '-trimpath', '-ldflags', "-s -w -X main.version=$Version", '-o', $ReleaseAgent, '.\cmd\agent')
$hash = (Get-FileHash -LiteralPath $ReleaseAgent -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath "$ReleaseAgent.sha256" -Value "$hash  agent.exe" -Encoding ASCII
Invoke-Native -FilePath $Cosign -Arguments @(
    'sign-blob', '--yes', '--key', $CosignKey,
    '--use-signing-config=false', '--new-bundle-format=false', '--tlog-upload=false',
    '--output-signature', "$ReleaseAgent.sig", $ReleaseAgent
)

if ($Publish) {
    $Gh = Find-GitHubCLI
    Invoke-Native -FilePath $Gh -Arguments @('auth', 'status', '--hostname', 'github.com')
    $arguments = @('release', 'create', $Tag, $ReleaseAgent, "$ReleaseAgent.sha256", "$ReleaseAgent.sig", '--repo', "$GitHubOwner/$GitHubRepository", '--target', $headCommit, '--title', "Windows LLM Manager $Tag")
    if ($ReleaseNotes) { $arguments += @('--notes', $ReleaseNotes) } else { $arguments += @('--generate-notes') }
    if ($Draft) { $arguments += '--draft' }
    if ($Prerelease) { $arguments += '--prerelease' }
    Invoke-Native -FilePath $Gh -Arguments $arguments
    Write-Host "Published universal release: https://github.com/$GitHubOwner/$GitHubRepository/releases/tag/$Tag"
} else {
    Write-Host "Signed universal release assets: $ReleaseDir"
    Write-Host 'Nothing was published. Add -Publish after reviewing the files.'
}
