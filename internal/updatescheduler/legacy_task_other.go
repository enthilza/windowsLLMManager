//go:build !windows

package updatescheduler

import "context"

const LegacyTaskName = "WindowsLLMManagerUpdateCheck"

func RemoveLegacyScheduledTask(context.Context) (bool, error) { return false, nil }
