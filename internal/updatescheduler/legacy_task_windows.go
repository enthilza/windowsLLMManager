//go:build windows

package updatescheduler

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
)

const LegacyTaskName = "WindowsLLMManagerUpdateCheck"

func RemoveLegacyScheduledTask(ctx context.Context) (bool, error) {
	query := exec.CommandContext(ctx, "schtasks.exe", "/Query", "/TN", LegacyTaskName)
	query.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
	if err := query.Run(); err != nil {
		return false, nil
	}
	remove := exec.CommandContext(ctx, "schtasks.exe", "/Delete", "/TN", LegacyTaskName, "/F")
	remove.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
	if output, err := remove.CombinedOutput(); err != nil {
		return false, fmt.Errorf("delete legacy scheduled task: %w: %s", err, output)
	}
	return true, nil
}
