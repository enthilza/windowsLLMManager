//go:build !windows

package updatescheduler

import "os/exec"

type processLauncher struct{}

func (processLauncher) Start(path string, args ...string) (Process, error) {
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
