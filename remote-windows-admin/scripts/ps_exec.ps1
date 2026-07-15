[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][uri]$BaseUrl,
    [Parameter(Mandatory = $true)][string]$Token,
    [Parameter(Mandatory = $true)][string]$Command,
    [Parameter(Mandatory = $true)][ValidateSet('json_object','lines')][string]$Format,
    [string]$CAPath,
    [string]$CAFingerprint
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '_common.ps1')
Invoke-WlmRequest -Method POST -Path '/exec' -BaseUrl $BaseUrl -Token $Token -Body @{ command = $Command; format = $Format } -CAPath $CAPath -CAFingerprint $CAFingerprint
