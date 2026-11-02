//go:build unix

package safefileio

import (
	"os"
	"strconv"
)

// CurrentUserID returns a stable filesystem-safe identifier for the current
// Unix user.
func CurrentUserID() (string, error) {
	return strconv.Itoa(os.Getuid()), nil
}
