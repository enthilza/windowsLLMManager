param(
    [Parameter(Mandatory = $true)][ValidateSet('Submit','Status','Cancel')][string]$Action,
    [Parameter(Mandatory = $true)][uri]$BaseUrl,
    [Parameter(Mandatory = $true)][string]$Token,
    [string]$JobId,
    [string]$Command,
    [ValidateSet('json_object','lines')][string]$Format = 'lines',
    [ValidateRange(1, 2147483647)][int]$TimeoutSec = 7200,
    [ValidateSet('Auto','InternalCA','PublicPKI')][string]$TLSMode = 'Auto',
    [string]$CAPath,
    [string]$CAFingerprint
)
. (Join-Path $PSScriptRoot '_common.ps1')

switch ($Action) {
    'Submit' {
        if ([string]::IsNullOrWhiteSpace($Command)) { throw '-Command is required for Submit.' }
        Invoke-WlmRequest -Method POST -Path '/jobs' -BaseUrl $BaseUrl -Token $Token -Body @{
            command = $Command
            format = $Format
            timeout_sec = $TimeoutSec
        } -TLSMode $TLSMode -CAPath $CAPath -CAFingerprint $CAFingerprint
    }
    'Status' {
        if ([string]::IsNullOrWhiteSpace($JobId)) { throw '-JobId is required for Status.' }
        Invoke-WlmRequest -Method GET -Path ("/jobs/{0}" -f [uri]::EscapeDataString($JobId)) -BaseUrl $BaseUrl -Token $Token -TLSMode $TLSMode -CAPath $CAPath -CAFingerprint $CAFingerprint
    }
    'Cancel' {
        if ([string]::IsNullOrWhiteSpace($JobId)) { throw '-JobId is required for Cancel.' }
        Invoke-WlmRequest -Method DELETE -Path ("/jobs/{0}" -f [uri]::EscapeDataString($JobId)) -BaseUrl $BaseUrl -Token $Token -TLSMode $TLSMode -CAPath $CAPath -CAFingerprint $CAFingerprint
    }
}
