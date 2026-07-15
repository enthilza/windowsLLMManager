[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidateSet('open','exec','close','restart','info')][string]$Action,
    [Parameter(Mandatory = $true)][uri]$BaseUrl,
    [Parameter(Mandatory = $true)][string]$Token,
    [string]$SessionId,
    [string]$Command,
    [ValidateSet('json_object','lines')][string]$Format = 'lines',
    [string]$CAPath,
    [string]$CAFingerprint
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '_common.ps1')
if ($Action -ne 'open' -and -not $SessionId) { throw '-SessionId is required for this action.' }
switch ($Action) {
    'open'    { Invoke-WlmRequest -Method POST -Path '/session' -BaseUrl $BaseUrl -Token $Token -CAPath $CAPath -CAFingerprint $CAFingerprint }
    'exec'    {
        if (-not $Command) { throw '-Command is required for exec.' }
        Invoke-WlmRequest -Method POST -Path "/session/$SessionId/exec" -BaseUrl $BaseUrl -Token $Token -Body @{ command = $Command; format = $Format } -CAPath $CAPath -CAFingerprint $CAFingerprint
    }
    'close'   { Invoke-WlmRequest -Method DELETE -Path "/session/$SessionId" -BaseUrl $BaseUrl -Token $Token -CAPath $CAPath -CAFingerprint $CAFingerprint }
    'restart' { Invoke-WlmRequest -Method POST -Path "/session/$SessionId/restart" -BaseUrl $BaseUrl -Token $Token -CAPath $CAPath -CAFingerprint $CAFingerprint }
    'info'    { Invoke-WlmRequest -Method GET -Path "/session/$SessionId/info" -BaseUrl $BaseUrl -Token $Token -CAPath $CAPath -CAFingerprint $CAFingerprint }
}
