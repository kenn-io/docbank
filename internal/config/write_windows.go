//go:build windows

package config

import (
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
)

// createPrivateConfig passes an extended-length path so a restore target
// deeper than MAX_PATH still works.
func createPrivateConfig(path string) (*os.File, error) {
	extended, err := winsecurity.ExtendedLengthPath(path)
	if err != nil {
		return nil, err
	}
	return safefileio.CreatePrivateFile(extended) //nolint:wrapcheck // the caller adds context.
}
