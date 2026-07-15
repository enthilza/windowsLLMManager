//go:build windows

package powershell

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"windowsllmmanager/internal/api"
)

func TestRunnerLines(t *testing.T) {
	runner := NewRunner(1024 * 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, "Write-Output 'alpha'; Write-Output 'beta'", api.FormatLines)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := json.Unmarshal(result.Output, &lines); err != nil {
		t.Fatal(err)
	}
	if !result.Success || len(lines) != 2 || lines[0] != "alpha" || lines[1] != "beta" {
		t.Fatalf("unexpected result: %+v lines=%v", result, lines)
	}
}

func TestRunnerJSON(t *testing.T) {
	runner := NewRunner(1024 * 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, "[pscustomobject]@{Name='agent';Count=2}", api.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var objects []map[string]any
	if err := json.Unmarshal(result.Output, &objects); err != nil {
		t.Fatalf("invalid JSON output %s: %v", result.Output, err)
	}
	if len(objects) != 1 || objects[0]["Name"] != "agent" {
		t.Fatalf("unexpected objects: %#v", objects)
	}
}

func TestRunnerSeparatesPowerShellErrors(t *testing.T) {
	runner := NewRunner(1024 * 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, "Write-Error 'boom'; Write-Output 'after'", api.FormatLines)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || len(result.Stderr) == 0 || result.Stderr[0] == "" {
		t.Fatalf("PowerShell error was not separated or did not fail execution: %+v", result)
	}
	var lines []string
	_ = json.Unmarshal(result.Output, &lines)
	if len(lines) != 1 || lines[0] != "after" {
		t.Fatalf("stdout was not preserved: %v", lines)
	}
}

func TestRunnerReportsNativeExitCode(t *testing.T) {
	runner := NewRunner(1024 * 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, "cmd.exe /c exit 7", api.FormatLines)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.ExitCode != 7 {
		t.Fatalf("unexpected native exit result: %+v", result)
	}
}

func TestRunnerTruncatesOutput(t *testing.T) {
	runner := NewRunner(1024)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, "'x' * 4096", api.FormatLines)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.RawOutput) > 1024 {
		t.Fatalf("output limit was not enforced: truncated=%t raw=%d output=%d", result.Truncated, len(result.RawOutput), len(result.Output))
	}
}

func TestSessionPersistsState(t *testing.T) {
	session, err := newSession("test", 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Abort()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := session.Exec(ctx, "$WLMTestValue = 41", api.FormatLines); err != nil {
		t.Fatal(err)
	}
	result, err := session.Exec(ctx, "$WLMTestValue + 1", api.FormatLines)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	_ = json.Unmarshal(result.Output, &lines)
	if len(lines) != 1 || lines[0] != "42" {
		t.Fatalf("session did not preserve state: %v", lines)
	}
}

func TestSessionExitRestarts(t *testing.T) {
	session, err := newSession("test", 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Abort()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := session.Exec(ctx, "exit 7", api.FormatLines)
	if !errors.Is(err, ErrShellExited) || !result.SessionRestarted {
		t.Fatalf("expected exited shell and restart, got result=%+v err=%v", result, err)
	}
}

func TestRunnerTimeout(t *testing.T) {
	runner := NewRunner(1024 * 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result, err := runner.Run(ctx, "Start-Sleep -Seconds 5", api.FormatLines)
	if !errors.Is(err, ErrTimedOut) || !result.TimedOut {
		t.Fatalf("expected timeout, got result=%+v err=%v", result, err)
	}
}

func TestRunnerKillAllStopsActiveOneShot(t *testing.T) {
	runner := NewRunner(1024 * 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, "Start-Sleep -Seconds 30", api.FormatLines)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	runner.KillAll()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("killed one-shot command unexpectedly returned success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("KillAll did not stop the active one-shot command")
	}
}

func TestRunnerBlockRejectsNewOneShot(t *testing.T) {
	runner := NewRunner(1024 * 1024)
	runner.BlockAndKillAll()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := runner.Run(ctx, "Get-Date", api.FormatLines); !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected blocked error, got %v", err)
	}
}
