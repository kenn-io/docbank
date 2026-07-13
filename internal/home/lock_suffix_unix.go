//go:build unix && !darwin

package home

import "path/filepath"

func launchSuffixKey(_ string, suffix string) (string, error) {
	return filepath.ToSlash(suffix), nil
}
