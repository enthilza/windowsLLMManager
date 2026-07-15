Set-StrictMode -Version Latest

if (-not ('WlmPinnedCaValidator' -as [type])) {
    Add-Type -ReferencedAssemblies 'System.Net.Http.dll' -TypeDefinition @'
using System;
using System.Linq;
using System.Net.Http;
using System.Net.Security;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;

public sealed class WlmPinnedCaValidator
{
    private readonly X509Certificate2 ca;
    private readonly string fingerprint;

    public WlmPinnedCaValidator(string caPath, string expectedFingerprint)
    {
        ca = new X509Certificate2(caPath);
        fingerprint = Fingerprint(ca);
        if (!String.Equals(fingerprint, Normalize(expectedFingerprint), StringComparison.Ordinal))
            throw new InvalidOperationException("Bundled CA certificate does not match the pinned CA fingerprint.");
        Callback = Validate;
    }

    public Func<HttpRequestMessage, X509Certificate2, X509Chain, SslPolicyErrors, bool> Callback { get; private set; }

    private bool Validate(HttpRequestMessage request, X509Certificate2 certificate, X509Chain ignored, SslPolicyErrors errors)
    {
        try
        {
            if ((errors & SslPolicyErrors.RemoteCertificateNameMismatch) != 0 || certificate == null)
                return false;
            using (var chain = new X509Chain())
            {
                chain.ChainPolicy.RevocationMode = X509RevocationMode.NoCheck;
                chain.ChainPolicy.VerificationFlags = X509VerificationFlags.AllowUnknownCertificateAuthority;
                chain.ChainPolicy.ExtraStore.Add(ca);
                bool built = chain.Build(certificate);
                if (!built && !chain.ChainStatus.Any(s => s.Status == X509ChainStatusFlags.UntrustedRoot))
                    return false;
                foreach (var status in chain.ChainStatus)
                    if (status.Status != X509ChainStatusFlags.NoError && status.Status != X509ChainStatusFlags.UntrustedRoot)
                        return false;
                if (chain.ChainElements.Count == 0)
                    return false;
                var root = chain.ChainElements[chain.ChainElements.Count - 1].Certificate;
                return String.Equals(Fingerprint(root), fingerprint, StringComparison.Ordinal) && root.RawData.SequenceEqual(ca.RawData);
            }
        }
        catch { return false; }
    }

    private static string Fingerprint(X509Certificate2 certificate)
    {
        using (var sha = SHA256.Create())
            return BitConverter.ToString(sha.ComputeHash(certificate.RawData)).Replace("-", "").ToUpperInvariant();
    }

    private static string Normalize(string value) { return (value ?? "").Replace(":", "").Trim().ToUpperInvariant(); }
}
'@
}

function Get-WlmCASettings {
    param([string]$CAPath, [string]$CAFingerprint)
    if (-not $CAPath) { $CAPath = Join-Path $PSScriptRoot '..\references\internal-ca.pem' }
    if (-not $CAFingerprint) {
        $CAFingerprint = (Get-Content -LiteralPath (Join-Path $PSScriptRoot '..\references\ca-fingerprint.txt') -Raw).Trim()
    }
    if ($CAFingerprint -notmatch '^[A-Fa-f0-9]{64}$') { throw 'The internal CA fingerprint is uninitialized or invalid. Run deploy.ps1 first.' }
    if (-not (Test-Path -LiteralPath $CAPath)) { throw "Internal CA certificate not found: $CAPath" }
    return @{ Path = [IO.Path]::GetFullPath($CAPath); Fingerprint = $CAFingerprint.ToUpperInvariant() }
}

function New-WlmHttpClient {
    param(
        [Parameter(Mandatory = $true)][uri]$BaseUrl,
        [Parameter(Mandatory = $true)][string]$Token,
        [string]$CAPath,
        [string]$CAFingerprint
    )
    if ($BaseUrl.Scheme -ne 'https') { throw 'Windows LLM Manager requires HTTPS; plain HTTP is forbidden.' }
    $caSettings = Get-WlmCASettings -CAPath $CAPath -CAFingerprint $CAFingerprint
    Add-Type -AssemblyName System.Net.Http
    $handler = [Net.Http.HttpClientHandler]::new()
    $validator = [WlmPinnedCaValidator]::new($caSettings.Path, $caSettings.Fingerprint)
    $handler.ServerCertificateCustomValidationCallback = $validator.Callback
    $client = [Net.Http.HttpClient]::new($handler, $true)
    $client.BaseAddress = $BaseUrl
    $client.Timeout = [TimeSpan]::FromSeconds(180)
    $client.DefaultRequestHeaders.Authorization = [Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $Token)
    return $client
}

function Invoke-WlmRequest {
    param(
        [Parameter(Mandatory = $true)][ValidateSet('GET','POST','DELETE')][string]$Method,
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][uri]$BaseUrl,
        [Parameter(Mandatory = $true)][string]$Token,
        [object]$Body,
        [string]$CAPath,
        [string]$CAFingerprint
    )
    $client = New-WlmHttpClient -BaseUrl $BaseUrl -Token $Token -CAPath $CAPath -CAFingerprint $CAFingerprint
    try {
        $request = [Net.Http.HttpRequestMessage]::new([Net.Http.HttpMethod]::new($Method), $Path)
        if ($null -ne $Body) {
            $json = $Body | ConvertTo-Json -Depth 20 -Compress
            $request.Content = [Net.Http.StringContent]::new($json, [Text.Encoding]::UTF8, 'application/json')
        }
        try { $response = $client.SendAsync($request).GetAwaiter().GetResult() }
        catch { throw "No HTTP response from agent. After prior auth errors, assume this source may be blocklisted. $($_.Exception.Message)" }
        $text = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        if (-not $response.IsSuccessStatusCode) {
            $code = [int]$response.StatusCode
            try { $errorObject = $text | ConvertFrom-Json } catch { $errorObject = $null }
            if ($errorObject -and $errorObject.PSObject.Properties['error']) {
                $details = if ($errorObject.error.PSObject.Properties['details']) { ' Details: ' + ($errorObject.error.details | ConvertTo-Json -Depth 20 -Compress) } else { '' }
                throw "HTTP $code $($errorObject.error.code): $($errorObject.error.message)$details"
            }
            throw "HTTP ${code}: $text"
        }
        if ([int]$response.StatusCode -eq 204 -or [string]::IsNullOrWhiteSpace($text)) { return $null }
        return $text | ConvertFrom-Json
    } finally { $client.Dispose() }
}
