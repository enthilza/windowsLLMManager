[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][uri]$BaseUrl,
    [Parameter(Mandatory = $true)][string]$Token,
    [Parameter(Mandatory = $true)][string]$CheckCommand,
    [ValidateSet('json_object','lines')][string]$Format = 'lines',
    [string]$CAPath,
    [string]$CAFingerprint
)
$ErrorActionPreference = 'Stop'
& (Join-Path $PSScriptRoot 'ps_exec.ps1') -BaseUrl $BaseUrl -Token $Token -Command $CheckCommand -Format $Format -CAPath $CAPath -CAFingerprint $CAFingerprint
