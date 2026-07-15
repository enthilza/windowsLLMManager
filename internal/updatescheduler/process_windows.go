//go:build windows

package updatescheduler

import (
	"os/exec"
	"syscall"
)

type processLauncher struct{}

func (processLauncher) Start(path string, args ...string) (Process, error) {
	const (
		detachedProcess        = 0x00000008
		createNewProcessGroup  = 0x00000200
		createBreakawayFromJob = 0x01000000
	)
	cmd := exec.Command(path, args...)
	// The updater must survive when it stops the parent Windows service, including
	// when a service host or test harness placed the agent in a kill-on-close job.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
