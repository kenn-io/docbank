//go:build !windows

package docbank

import (
	"os"
)

func resetSourceAttributesNoFollow(path string) (resetSourceAttributes, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return resetSourceAttributes{}, err
	}
	return resetSourceAttributes{
		directory: info.IsDir(),
		reparse:   info.Mode()&os.ModeSymlink != 0,
	}, nil
}
