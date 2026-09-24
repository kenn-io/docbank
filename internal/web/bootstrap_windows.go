//go:build windows

package web

import (
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
)

// createBootstrapFile passes an extended-length path so a vault deeper than
// MAX_PATH still works.
func createBootstrapFile(path string) (*os.File, error) {
	extended, err := winsecurity.ExtendedLengthPath(path)
	if err != nil {
		return nil, err
	}
	return safefileio.CreatePrivateFile(extended) //nolint:wrapcheck // the caller adds context.
}
