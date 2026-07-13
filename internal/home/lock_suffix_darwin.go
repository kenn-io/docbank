//go:build darwin

package home

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

const pathconfCaseSensitive = 11 // Darwin's _PC_CASE_SENSITIVE.

func launchSuffixKey(ancestor, suffix string) (string, error) {
	caseSensitive, err := unix.Pathconf(ancestor, pathconfCaseSensitive)
	if err != nil {
		return "", fmt.Errorf("checking daemon launch filesystem semantics: %w", err)
	}
	// Darwin volumes compare canonically equivalent Unicode spellings as one
	// name. Most also compare case-insensitively; preserve case only when the
	// volume reports that it distinguishes it.
	key := norm.NFD.String(filepath.ToSlash(suffix))
	if caseSensitive == 0 {
		key = strings.ToLower(key)
	}
	return key, nil
}
