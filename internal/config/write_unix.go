//go:build !windows

package config

import (
	"os"

	"go.kenn.io/kit/safefileio"
)

func createPrivateConfig(path string) (*os.File, error) {
	return safefileio.CreatePrivateFile(path) //nolint:wrapcheck // the caller adds context.
}
