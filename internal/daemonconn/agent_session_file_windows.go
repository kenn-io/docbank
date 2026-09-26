//go:build windows

package daemonconn

import (
	"os"

	"go.kenn.io/docbank/internal/winsecurity"
)

func openPrivateAgentSessionFile(path string) (*os.File, error) {
	file, err := winsecurity.OpenRestrictedCurrentUserFile(path)
	if err != nil {
		return nil, ErrAgentSessionFile
	}
	return file, nil
}
