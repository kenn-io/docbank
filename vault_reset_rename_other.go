//go:build !windows

package docbank

import (
	"fmt"

	"go.kenn.io/kit/atomicfile"
)

func renameVaultNoReplace(source, destination string) error {
	if err := atomicfile.RenameNoReplace(source, destination); err != nil {
		return fmt.Errorf("rename without replacing: %w", err)
	}
	return nil
}
