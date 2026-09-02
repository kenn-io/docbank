//go:build darwin || linux

package providerutil

import (
	"fmt"
	"syscall"
)

func detachManagedCommandHelper() error {
	_, err := syscall.Setsid()
	if err != nil {
		return fmt.Errorf("start detached helper session: %w", err)
	}
	return nil
}
