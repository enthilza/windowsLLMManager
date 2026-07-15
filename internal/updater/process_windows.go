//go:build windows

package updater

import (
	"os/exec"
	"syscall"
)

func prepareHiddenCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
}
