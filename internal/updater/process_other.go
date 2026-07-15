//go:build !windows

package updater

import "os/exec"

func prepareHiddenCommand(_ *exec.Cmd) {}
