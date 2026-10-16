//go:build windows

package docbank

import (
	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/atomicfile"
)

func renameVaultNoReplace(source, destination string) error {
	return renameVaultNoReplaceWithMove(source, destination, atomicfile.RenameNoReplace)
}

// renameVaultNoReplaceWithMove passes extended-length paths to move so a
// vault nested deeper than MAX_PATH can still be moved aside.
func renameVaultNoReplaceWithMove(
	source, destination string,
	move func(string, string) error,
) error {
	extendedSource, err := winsecurity.ExtendedLengthPath(source)
	if err != nil {
		return err
	}
	extendedDestination, err := winsecurity.ExtendedLengthPath(destination)
	if err != nil {
		return err
	}
	return move(extendedSource, extendedDestination)
}
