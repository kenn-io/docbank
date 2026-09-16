package loadfile

import (
	"errors"
	"strings"
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
		stem, _, _ := strings.Cut(strings.ToUpper(part), ".")
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
			(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9') {
			return "", ErrUnsafeReference
		}
	}
	return value, nil
}
