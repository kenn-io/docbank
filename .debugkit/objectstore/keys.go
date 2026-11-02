package objectstore

import (
	"fmt"
	"io/fs"
	"strings"
)

// ValidateKey accepts slash-separated, relative object names without empty,
// dot, or parent segments. Backslashes, colons, and NULs are rejected. These
// rules do not depend on the host OS; local stores additionally enforce their
// filesystem's name restrictions. Validation does not resolve filesystem links.
func ValidateKey(key string) error {
	if !fs.ValidPath(key) || key == "." || strings.ContainsAny(key, "\\:\x00") {
		return fmt.Errorf("objectstore: invalid key %q", key)
	}
	return nil
}

// ValidatePrefix accepts an empty prefix, a partial key, or a key followed by
// a slash. Prefix matching compares names literally without path cleaning.
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return ValidateKey(strings.TrimSuffix(prefix, "/"))
}
