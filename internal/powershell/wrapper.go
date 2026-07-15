package powershell

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const frameChunkChars = 3072

type frameMeta struct {
	Success   bool   `json:"success"`
	ExitCode  int    `json:"exit_code"`
	Format    string `json:"format"`
	Truncated bool   `json:"truncated"`
}

type framedResult struct {
	Meta       frameMeta
	Output     []byte
	Stderr     []byte
	RawPrelude string
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func buildWrapper(command, format, id string, maxBytes int) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(command))
	begin := "___WLM_BEGIN_" + id + "___"
	end := "___WLM_END_" + id + "___"
	errName := "wlm-" + id + ".err"

	// The user command is data, never PowerShell source inside this fixed wrapper.
	// Dot-sourcing preserves variables, functions and location in persistent sessions.
	parts := []string{
		fmt.Sprintf("try{$__wlm_buf_%s=$Host.UI.RawUI.BufferSize;if($__wlm_buf_%s.Width -lt 200){$__wlm_buf_%s.Width=200;$Host.UI.RawUI.BufferSize=$__wlm_buf_%s}}catch{}", id, id, id, id),
		fmt.Sprintf("$__wlm_cmd_%s=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'))", id, encoded),
		fmt.Sprintf("$__wlm_err_%s=Join-Path ([IO.Path]::GetTempPath()) '%s'", id, errName),
		fmt.Sprintf("$__wlm_ok_%s=$true", id),
		fmt.Sprintf("$__wlm_exit_%s=0", id),
		fmt.Sprintf("$__wlm_trunc_%s=$false", id),
		fmt.Sprintf("$__wlm_caught_%s=''", id),
		"$global:LASTEXITCODE=0",
		fmt.Sprintf("try{$__wlm_sb_%s=[ScriptBlock]::Create($__wlm_cmd_%s);$__wlm_out_%s=@(. $__wlm_sb_%s 2> $__wlm_err_%s 3>&1 4>&1 5>&1 6>&1);$__wlm_psok_%s=$?;$__wlm_native_%s=$LASTEXITCODE}catch{$__wlm_ok_%s=$false;$__wlm_exit_%s=1;$__wlm_out_%s=@();$__wlm_caught_%s=($_|Out-String)}", id, id, id, id, id, id, id, id, id, id, id),
		fmt.Sprintf("if(-not $__wlm_psok_%s){$__wlm_ok_%s=$false;if($__wlm_exit_%s -eq 0){$__wlm_exit_%s=1}}", id, id, id, id),
		fmt.Sprintf("if($__wlm_native_%s -is [int] -and $__wlm_native_%s -ne 0){$__wlm_ok_%s=$false;$__wlm_exit_%s=[int]$__wlm_native_%s}", id, id, id, id, id),
		fmt.Sprintf("$__wlm_stderr_%s=if(Test-Path -LiteralPath $__wlm_err_%s){[IO.File]::ReadAllText($__wlm_err_%s)}else{''}", id, id, id),
		fmt.Sprintf("if($__wlm_caught_%s){$__wlm_stderr_%s+=$__wlm_caught_%s}", id, id, id),
		fmt.Sprintf("if(-not [string]::IsNullOrWhiteSpace($__wlm_stderr_%s)){$__wlm_ok_%s=$false;if($__wlm_exit_%s -eq 0){$__wlm_exit_%s=1}}", id, id, id, id),
	}
	if format == "json_object" {
		parts = append(parts, fmt.Sprintf("try{$__wlm_text_%s=Microsoft.PowerShell.Utility\\ConvertTo-Json -InputObject $__wlm_out_%s -Depth 20 -Compress}catch{$__wlm_ok_%s=$false;$__wlm_exit_%s=1;$__wlm_stderr_%s+=($_|Out-String);$__wlm_text_%s='null'}", id, id, id, id, id, id))
	} else {
		parts = append(parts, fmt.Sprintf("$__wlm_text_%s=(($__wlm_out_%s|ForEach-Object{$_|Out-String -Stream}) -join \"`n\")", id, id))
	}
	parts = append(parts,
		fmt.Sprintf("$__wlm_ob_%s=[Text.Encoding]::UTF8.GetBytes([string]$__wlm_text_%s)", id, id),
		fmt.Sprintf("$__wlm_eb_%s=[Text.Encoding]::UTF8.GetBytes([string]$__wlm_stderr_%s)", id, id),
		fmt.Sprintf("if($__wlm_ob_%s.Length -gt %d){$__wlm_ob_%s=[byte[]]$__wlm_ob_%s[0..%d];$__wlm_trunc_%s=$true}", id, maxBytes, id, id, maxBytes-1, id),
		fmt.Sprintf("if($__wlm_eb_%s.Length -gt %d){$__wlm_eb_%s=[byte[]]$__wlm_eb_%s[0..%d];$__wlm_trunc_%s=$true}", id, maxBytes, id, id, maxBytes-1, id),
		fmt.Sprintf("$__wlm_meta_%s=@{success=$__wlm_ok_%s;exit_code=$__wlm_exit_%s;format='%s';truncated=$__wlm_trunc_%s}|Microsoft.PowerShell.Utility\\ConvertTo-Json -Compress", id, id, id, format, id),
		fmt.Sprintf("[Console]::Out.WriteLine('%s')", begin),
		fmt.Sprintf("$__wlm_mb_%s=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($__wlm_meta_%s));[Console]::Out.WriteLine('M:'+$__wlm_mb_%s)", id, id, id),
		chunkEmitter(id, "O", "ob"),
		chunkEmitter(id, "E", "eb"),
		fmt.Sprintf("[Console]::Out.WriteLine('%s')", end),
		fmt.Sprintf("if(Test-Path -LiteralPath $__wlm_err_%s){Remove-Item -LiteralPath $__wlm_err_%s -Force -ErrorAction SilentlyContinue}", id, id),
		cleanupVariables(id),
	)
	return strings.Join(parts, ";")
}

func chunkEmitter(id, prefix, variable string) string {
	return fmt.Sprintf("$__wlm_b64_%s_%s=[Convert]::ToBase64String($__wlm_%s_%s);for($__wlm_i_%s_%s=0;$__wlm_i_%s_%s -lt $__wlm_b64_%s_%s.Length;$__wlm_i_%s_%s+=%d){[Console]::Out.WriteLine('%s:'+$__wlm_b64_%s_%s.Substring($__wlm_i_%s_%s,[Math]::Min(%d,$__wlm_b64_%s_%s.Length-$__wlm_i_%s_%s)))}", prefix, id, variable, id, prefix, id, prefix, id, prefix, id, prefix, id, frameChunkChars, prefix, prefix, id, prefix, id, frameChunkChars, prefix, id, prefix, id)
}

func cleanupVariables(id string) string {
	// Best-effort cleanup only; random names prevent collisions if cleanup itself fails.
	return fmt.Sprintf("Get-Variable -Name '*%s*' -ErrorAction SilentlyContinue|Remove-Variable -Force -ErrorAction SilentlyContinue", id)
}

func decodeMeta(encoded string) (frameMeta, error) {
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return frameMeta{}, err
	}
	var meta frameMeta
	err = json.Unmarshal(b, &meta)
	return meta, err
}
