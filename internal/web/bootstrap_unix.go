//go:build !windows

package web

import (
	"os"

	"go.kenn.io/kit/safefileio"
)

func createBootstrapFile(path string) (*os.File, error) {
	return safefileio.CreatePrivateFile(path) //nolint:wrapcheck // the caller adds context.
}
