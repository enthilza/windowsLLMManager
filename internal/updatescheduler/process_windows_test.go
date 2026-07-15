//go:build windows

package updatescheduler

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDetachedUpdaterSurvivesLauncherExit(t *testing.T) {
	if os.Getenv("WLM_DETACH_HELPER") == "1" {
		marker := os.Getenv("WLM_DETACH_MARKER")
		process, err := (processLauncher{}).Start(
			"cmd.exe", "/d", "/c", "ping -n 2 127.0.0.1 >nul & echo ok>"+marker,
		)
		if err != nil {
			os.Exit(2)
		}
		if os.Getenv("WLM_DETACH_WAIT") == "1" {
			if err := process.Wait(); err != nil {
				os.Exit(3)
			}
		}
		os.Exit(0)
	}

	marker := filepath.Join(t.TempDir(), "detached-updater-finished.txt")
	waitingHelper := exec.Command(os.Args[0], "-test.run=TestDetachedUpdaterSurvivesLauncherExit")
	waitingHelper.Env = append(os.Environ(), "WLM_DETACH_HELPER=1", "WLM_DETACH_WAIT=1", "WLM_DETACH_MARKER="+marker)
	if output, err := waitingHelper.CombinedOutput(); err != nil {
		t.Fatalf("waiting launcher helper failed: %v: %s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("updater child command did not create its marker while awaited: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=TestDetachedUpdaterSurvivesLauncherExit")
	helper.Env = append(os.Environ(), "WLM_DETACH_HELPER=1", "WLM_DETACH_MARKER="+marker)
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("launcher helper failed: %v: %s", err, output)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("detached updater child did not survive launcher process exit")
}
