[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidateSet('health','blocklist','unblock','killswitch')][string]$Action,
    [Parameter(Mandatory = $true)][uri]$BaseUrl,
    [Parameter(Mandatory = $true)][string]$Token,
    [string]$IPAddress,
    [switch]$ConfirmArm,
    [ValidateSet('Auto','InternalCA','PublicPKI')][string]$TLSMode = 'Auto',
    [string]$CAPath,
    [string]$CAFingerprint
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '_common.ps1')
switch ($Action) {
    'health' { Invoke-WlmRequest -Method GET -Path '/health' -BaseUrl $BaseUrl -Token $Token -TLSMode $TLSMode -CAPath $CAPath -CAFingerprint $CAFingerprint }
    'blocklist' { Invoke-WlmRequest -Method GET -Path '/blocklist' -BaseUrl $BaseUrl -Token $Token -TLSMode $TLSMode -CAPath $CAPath -CAFingerprint $CAFingerprint }
    'unblock' {
        $parsedIP = $null
        if (-not [Net.IPAddress]::TryParse($IPAddress, [ref]$parsedIP)) { throw '-IPAddress must be a valid IP address.' }
        Invoke-WlmRequest -Method DELETE -Path ('/blocklist/' + [uri]::EscapeDataString($IPAddress)) -BaseUrl $BaseUrl -Token $Token -TLSMode $TLSMode -CAPath $CAPath -CAFingerprint $CAFingerprint
    }
    'killswitch' {
        if (-not $ConfirmArm) { throw 'Kill-switch is arm-only and requires explicit operator instruction. Re-run with -ConfirmArm only after that instruction.' }
        Invoke-WlmRequest -Method POST -Path '/killswitch' -BaseUrl $BaseUrl -Token $Token -TLSMode $TLSMode -CAPath $CAPath -CAFingerprint $CAFingerprint
    }
}
