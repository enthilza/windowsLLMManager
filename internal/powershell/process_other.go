//go:build !windows

package powershell

import "os/exec"

func prepareCommand(_ *exec.Cmd) {}
func attachKillJob(cmd *exec.Cmd) (func() error, error) {
	return func() error { return cmd.Process.Kill() }, nil
}
