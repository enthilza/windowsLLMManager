package updatescheduler

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

type fakeProcess struct{ done <-chan struct{} }

func (p fakeProcess) Wait() error {
	<-p.done
	return nil
}

type fakeLauncher struct {
	mu      sync.Mutex
	starts  int
	path    string
	args    []string
	process Process
}

func (l *fakeLauncher) Start(path string, args ...string) (Process, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts++
	l.path = path
	l.args = append([]string(nil), args...)
	return l.process, nil
}

func (l *fakeLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts
}

func TestSchedulerSkipsOverlappingUpdater(t *testing.T) {
	done := make(chan struct{})
	launcher := &fakeLauncher{process: fakeProcess{done: done}}
	s := newWithLauncher("updater.exe", "updater-config.json", time.Minute, log.New(io.Discard, "", 0), launcher)
	s.launch()
	s.launch()
	if launcher.count() != 1 {
		t.Fatalf("expected one updater process, got %d", launcher.count())
	}
	close(done)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.launch()
	if launcher.count() != 2 {
		t.Fatalf("expected a second updater after completion, got %d", launcher.count())
	}
	if launcher.path != "updater.exe" || len(launcher.args) != 3 || launcher.args[0] != "--check-only" || launcher.args[1] != "--config" || launcher.args[2] != "updater-config.json" {
		t.Fatalf("unexpected launch: path=%q args=%v", launcher.path, launcher.args)
	}
}

func TestSchedulerLaunchesOnConfiguredInterval(t *testing.T) {
	done := make(chan struct{})
	close(done)
	launcher := &fakeLauncher{process: fakeProcess{done: done}}
	s := newWithLauncher("updater.exe", "updater-config.json", 20*time.Millisecond, log.New(io.Discard, "", 0), launcher)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	if launcher.count() != 0 {
		t.Fatal("scheduler launched before the configured interval")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if launcher.count() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scheduler did not launch on the configured interval")
}
