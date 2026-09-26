//go:build windows

package filepublish

import (
	"errors"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/winsecurity"
)

func makePrivateStageDir(parentPath, prefix string) (string, *os.File, error) {
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = parent.Close() }()
	for range 10 {
		component, err := newStageName(prefix)
		if err != nil {
			return "", nil, err
		}
		pin, err := winsecurity.MkdirPrivatePinnedAt(parent, component)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return filepath.Join(parentPath, component), pin, nil
	}
	return "", nil, errors.New("creating private publication stage: repeated name collisions")
}
