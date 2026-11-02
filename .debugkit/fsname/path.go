package fsname

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"
)

// PathOption relaxes CheckPath for a path form the caller expects.
type PathOption func(*pathConfig)

type pathConfig struct {
	allowUNC       bool
	allowLongPath  bool
	allowShortName bool
}

// AllowUNC accepts Windows network share paths (\\server\share\...).
func AllowUNC() PathOption { return func(c *pathConfig) { c.allowUNC = true } }

// AllowLongPath accepts the Windows long-path forms \\?\C:\... and
// \\?\UNC\server\share\..., which skip Win32 path normalization. It never
// accepts the \\.\ device namespace.
func AllowLongPath() PathOption { return func(c *pathConfig) { c.allowLongPath = true } }

// AllowShortName accepts Windows 8.3 short-name elements such as PROGRA~1.
func AllowShortName() PathOption { return func(c *pathConfig) { c.allowShortName = true } }

// CheckPath reports an error wrapping ErrNotPortable when path has a form
// that behaves differently from how it reads, or that another OS cannot use.
// It is lexical and never touches the file system.
//
// Every element other than "." and ".." must pass Check, which rejects NTFS
// stream names ("a:b"), device names, and trailing dots or spaces. On Windows
// CheckPath also rejects, unless an option allows them:
//   - network share paths (\\server\share), see AllowUNC;
//   - long-path forms (\\?\), see AllowLongPath;
//   - the device namespace (\\.\ and \??\), always;
//   - drive-relative paths (C:foo), which depend on the drive's current
//     directory, always;
//   - rooted paths without a drive (\foo), which depend on the current
//     drive, always;
//   - 8.3 short-name elements (PROGRA~1), which can name a different file
//     than the long name a caller checked, see AllowShortName.
func CheckPath(path string, opts ...PathOption) error {
	var cfg pathConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return checkPath(path, runtime.GOOS == "windows", cfg)
}

// checkPath is CheckPath with the host made explicit, so tests cover the
// Windows rules on every platform.
func checkPath(path string, windows bool, cfg pathConfig) error {
	if path == "" {
		return fmt.Errorf("%w: path is empty", ErrNotPortable)
	}
	rest := path
	isSep := func(r rune) bool { return r == '/' }
	if windows {
		var err error
		if rest, err = checkWindowsPrefix(path, cfg); err != nil {
			return err
		}
		isSep = isSeparator
	}
	for _, elem := range strings.FieldsFunc(rest, isSep) {
		if elem == "." || elem == ".." {
			continue
		}
		if err := Check(elem); err != nil {
			return fmt.Errorf("path %q: %w", path, err)
		}
		if windows && !cfg.allowShortName && isShortName(elem) {
			return fmt.Errorf("%w: path %q: %q looks like an 8.3 short name", ErrNotPortable, path, elem)
		}
	}
	return nil
}

// shortName matches the shape of the 8.3 aliases Windows generates: up to six
// characters, a tilde and a digit sequence, and an optional extension of up to
// three. isShortName also enforces the 8-character stem limit.
var shortName = regexp.MustCompile(`^[^~.]{1,6}~[0-9]+(\.[^.]{0,3})?$`)

func isShortName(elem string) bool {
	stem, _, _ := strings.Cut(elem, ".")
	return utf8.RuneCountInString(stem) <= 8 && shortName.MatchString(elem)
}

// checkWindowsPrefix validates the volume and root of a Windows path and
// returns the remainder whose elements CheckPath checks one by one.
func checkWindowsPrefix(path string, cfg pathConfig) (string, error) {
	norm := strings.ReplaceAll(path, "/", `\`)
	switch {
	case strings.HasPrefix(norm, `\\.\`), strings.HasPrefix(norm, `\??\`):
		return "", fmt.Errorf("%w: path %q uses the device namespace", ErrNotPortable, path)
	case strings.HasPrefix(norm, `\\?\`):
		if !cfg.allowLongPath {
			return "", fmt.Errorf("%w: path %q uses the \\\\?\\ long-path form", ErrNotPortable, path)
		}
		norm = norm[len(`\\?\`):]
		// Windows skips path normalization after \\?\, so "/" and "." or ".."
		// elements are taken literally and the open would fail.
		if strings.Contains(path, "/") {
			return "", fmt.Errorf("%w: path %q uses / after the \\\\?\\ long-path prefix", ErrNotPortable, path)
		}
		for elem := range strings.SplitSeq(norm, `\`) {
			if elem == "." || elem == ".." {
				return "", fmt.Errorf("%w: path %q uses %q after the \\\\?\\ long-path prefix", ErrNotPortable, path, elem)
			}
		}
		if len(norm) >= 4 && strings.EqualFold(norm[:4], `UNC\`) {
			unc := norm[4:]
			if !cfg.allowUNC {
				return "", fmt.Errorf("%w: path %q is a network share", ErrNotPortable, path)
			}
			return skipShare(unc), nil
		}
		if !isDriveRoot(norm) {
			return "", fmt.Errorf("%w: path %q is not a drive path after \\\\?\\", ErrNotPortable, path)
		}
		return norm[2:], nil
	case strings.HasPrefix(norm, `\\`):
		if !cfg.allowUNC {
			return "", fmt.Errorf("%w: path %q is a network share", ErrNotPortable, path)
		}
		return skipShare(norm[2:]), nil
	case len(norm) >= 2 && norm[1] == ':' && isASCIILetter(norm[0]):
		if !isDriveRoot(norm) {
			return "", fmt.Errorf("%w: path %q is relative to the current directory of drive %s", ErrNotPortable, path, norm[:2])
		}
		return norm[2:], nil
	case strings.HasPrefix(norm, `\`):
		return "", fmt.Errorf("%w: path %q is rooted without a drive", ErrNotPortable, path)
	}
	return norm, nil
}

// skipShare drops the server and share names of a UNC path, which follow
// host and share naming rules rather than file name rules.
func skipShare(unc string) string {
	parts := strings.SplitN(unc, `\`, 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func isDriveRoot(p string) bool {
	return len(p) >= 3 && isASCIILetter(p[0]) && p[1] == ':' && p[2] == '\\'
}

func isASCIILetter(b byte) bool { return 'a' <= b|0x20 && b|0x20 <= 'z' }
