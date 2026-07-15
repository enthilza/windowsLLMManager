//go:build !windows

package updater

import (
	"context"
	"errors"
)

func stopService(context.Context, string) error {
	return errors.New("Windows service control is unavailable")
}
func startService(context.Context, string) error {
	return errors.New("Windows service control is unavailable")
}
