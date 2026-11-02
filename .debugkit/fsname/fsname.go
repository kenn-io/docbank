package fsname

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/pathologize"
)

// ErrNotPortable reports a name that is not safe to use on every modern
// operating system and file system.
var ErrNotPortable = errors.New("fsname: name is not portable")

// consoleDeviceNames are Windows device names that pathologize does not
// reserve. Like other device names they are reserved with any extension.
var consoleDeviceNames = []string{"CONIN$", "CONOUT$"}

// Check reports an error wrapping ErrNotPortable unless name is a single path
// element that Clean would leave unchanged. It rejects empty names, "." and
// "..", separators, characters invalid on any modern OS, invalid UTF-8,
// reserved device names with or without an extension, leading or trailing
// spaces, trailing dots, and names longer than 255 bytes.
func Check(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: name is empty", ErrNotPortable)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q is not a name", ErrNotPortable, name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", ErrNotPortable, name)
	}
	if cleaned := Clean(name); cleaned != name {
		return fmt.Errorf("%w: %q (portable form %q)", ErrNotPortable, name, cleaned)
	}
	return nil
}

// Clean returns a portable version of a single name. It never returns an
// empty name. See pathologize.Clean for the rewriting rules; Clean also
// defuses the Windows console device names CONIN$ and CONOUT$ the same way,
// by appending an underscore to the reserved part.
func Clean(name string) string {
	// Appending "_" can push a name past pathologize's length limit, so
	// repeat until neither step changes the name, as pathologize.Clean does.
	for {
		cleaned := defuseConsoleName(pathologize.Clean(name))
		if cleaned == name {
			return cleaned
		}
		name = cleaned
	}
}

// Join cleans every element of parts, which may themselves contain "/" or
// "\" separators, drops empty, "." and ".." elements, and joins the result
// with the host separator. The result is always a non-empty local path in the
// sense of filepath.IsLocal, suitable for fslink.OpenInRoot.
func Join(parts ...string) string {
	var elems []string
	for _, part := range parts {
		for _, elem := range strings.FieldsFunc(part, isSeparator) {
			if elem == "." || elem == ".." {
				continue
			}
			elems = append(elems, Clean(elem))
		}
	}
	if len(elems) == 0 {
		return Clean("")
	}
	return filepath.Join(elems...)
}

func isSeparator(r rune) bool { return r == '/' || r == '\\' }

// defuseConsoleName appends "_" to a console device name that makes up the
// part of name before its first dot, ignoring case and trailing spaces, which
// Windows ignores when it matches device names.
func defuseConsoleName(name string) string {
	stem, rest, hasExt := strings.Cut(name, ".")
	trimmed := strings.TrimRight(stem, " ")
	for _, device := range consoleDeviceNames {
		if !strings.EqualFold(trimmed, device) {
			continue
		}
		if hasExt {
			return trimmed + "_." + rest
		}
		return trimmed + "_"
	}
	return name
}
