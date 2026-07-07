package store

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// NormalizeName NFC-normalizes name and validates it for use as a node name.
// Names are stored as given (post-NFC) and compared case-sensitively.
func NormalizeName(name string) (string, error) {
	name = norm.NFC.String(name)
	switch {
	case name == "":
		return "", fmt.Errorf("%w: empty", ErrInvalidName)
	case name == "." || name == "..":
		return "", fmt.Errorf("%w: %q", ErrInvalidName, name)
	case strings.ContainsAny(name, "/\x00"):
		return "", fmt.Errorf("%w: %q contains '/' or NUL", ErrInvalidName, name)
	}
	return name, nil
}

// splitSuffix splits a name into (base, ext) for collision suffixing.
// The extension is the final dot-segment; a leading dot (dotfile) is part
// of the base.
func splitSuffix(name string) (base, ext string) {
	i := strings.LastIndex(name, ".")
	if i <= 0 {
		return name, ""
	}
	return name[:i], name[i:]
}

// suffixedName renders the nth collision candidate: n==1 is the original
// name, n>=2 appends " (n)" before the extension.
func suffixedName(base, ext string, n int) string {
	if n == 1 {
		return base + ext
	}
	return fmt.Sprintf("%s (%d)%s", base, n, ext)
}

// parseSuffix reports which candidate ordinal name is for the given
// base/ext, if any: base+ext -> 1, "base (n)"+ext -> n.
//
//nolint:unparam // base is used to validate the name pattern
func parseSuffix(name, base, ext string) (int, bool) {
	if name == base+ext {
		return 1, true
	}
	if !strings.HasPrefix(name, base+" (") || !strings.HasSuffix(name, ")"+ext) {
		return 0, false
	}
	inner := name[len(base)+2 : len(name)-len(ext)-1]
	n, err := strconv.Atoi(inner)
	if err != nil || n < 2 {
		return 0, false
	}
	return n, true
}
