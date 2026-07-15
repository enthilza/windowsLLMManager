package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"windowsllmmanager/internal/api"
	"windowsllmmanager/internal/powershell"
)

type fakeRunner struct {
	done <-chan struct{}
}

func (f fakeRunner) Run(ctx context.Context, _ string, format string) (api.ExecutionResult, error) {
	select {
	case <-ctx.Done():
		return api.ExecutionResult{Success: false, ExitCode: -1, Format: format, TimedOut: true}, powershell.ErrTimedOut
	case <-f.done:
		return api.ExecutionResult{Success: true, ExitCode: 0, Format: format}, nil
	}
}

func TestJobCompletesAndRetainsResult(t *testing.T) {
	done := make(chan struct{})
	m := NewManager(fakeRunner{done: done}, 1, 2, time.Hour)
	created, err := m.Submit("Get-Date", api.FormatLines, time.Second, nil)
	if err != nil || created.Status != StatusRunning {
		t.Fatalf("unexpected submit: %+v err=%v", created, err)
	}
	close(done)
	final := waitTerminal(t, m, created.JobID)
	if final.Status != StatusCompleted || final.Execution == nil || !final.Execution.Success {
		t.Fatalf("unexpected final snapshot: %+v", final)
	}
}

func TestJobCancelAndLimit(t *testing.T) {
	never := make(chan struct{})
	m := NewManager(fakeRunner{done: never}, 1, 2, time.Hour)
	created, err := m.Submit("Start-Sleep 30", api.FormatLines, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit("Get-Date", api.FormatLines, time.Minute, nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("expected job limit, got %v", err)
	}
	cancelling, err := m.Cancel(created.JobID, "operator")
	if err != nil || cancelling.Status != StatusCancelling {
		t.Fatalf("unexpected cancel: %+v err=%v", cancelling, err)
	}
	final := waitTerminal(t, m, created.JobID)
	if final.Status != StatusCancelled || final.CancelReason != "operator" || final.Execution == nil || final.Execution.TimedOut {
		t.Fatalf("unexpected cancelled snapshot: %+v", final)
	}
}

func TestJobTimesOut(t *testing.T) {
	never := make(chan struct{})
	m := NewManager(fakeRunner{done: never}, 1, 2, time.Hour)
	created, err := m.Submit("Start-Sleep 30", api.FormatLines, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, m, created.JobID)
	if final.Status != StatusTimedOut || final.Execution == nil || !final.Execution.TimedOut {
		t.Fatalf("unexpected timeout snapshot: %+v", final)
	}
}

func TestBlockedManagerRejectsNewJobs(t *testing.T) {
	done := make(chan struct{})
	m := NewManager(fakeRunner{done: done}, 1, 2, time.Hour)
	m.BlockAndCancelAll("kill_switch")
	if _, err := m.Submit("Get-Date", api.FormatLines, time.Second, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected blocked error, got %v", err)
	}
}

func waitTerminal(t *testing.T, m *Manager, id string) Snapshot {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := m.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Status != StatusRunning && snapshot.Status != StatusCancelling {
			return snapshot
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not reach a terminal state")
	return Snapshot{}
}
