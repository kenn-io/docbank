package loadfile

import (
	"errors"
	"strings"

	"go.kenn.io/docbank/internal/filename"
)

var ErrUnsafeReference = errors.New("package_reference_unsafe: load-file reference escapes the approved root")

func portablePackagePath(value string) (string, error) {
	value = strings.ReplaceAll(value, `\`, "/")
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, ":\x00") {
		return "", ErrUnsafeReference
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return "", ErrUnsafeReference
		}
		if filename.IsWindowsDeviceName(part) {
			return "", ErrUnsafeReference
		}
	}
	return value, nil
}
