//go:build !windows

package config

import "testing"

func privateTestConfigDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
