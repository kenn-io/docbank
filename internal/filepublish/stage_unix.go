//go:build !windows

package filepublish

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func makePrivateStageDir(parent, prefix string) (string, *os.File, error) {
	for range 10 {
		name, err := newStageName(prefix)
		if err != nil {
			return "", nil, err
		}
		dir := filepath.Join(parent, name)
		err = os.Mkdir(dir, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("creating private publication stage: %w", err)
		}
		return dir, nil, nil
	}
	return "", nil, errors.New("creating private publication stage: repeated name collisions")
}
