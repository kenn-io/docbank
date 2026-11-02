package fslink

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Kind classifies a directory entry by whether it redirects to another path.
type Kind uint8

const (
	// NotLink is a directory entry that does not redirect elsewhere.
	NotLink Kind = iota
	// Symlink is a symbolic link.
	Symlink
	// Junction is a Windows directory junction.
	Junction
	// OtherLink is another Windows name-surrogate reparse point, such as a
	// volume mount point or an unrecognized name-surrogate tag.
	OtherLink
)

// String returns a lower-case name for k.
func (k Kind) String() string {
	switch k {
	case NotLink:
		return "not-link"
	case Symlink:
		return "symlink"
	case Junction:
		return "junction"
	case OtherLink:
		return "other-link"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// ErrIsLink reports that an open refused to follow a link. Functions return
// it wrapped in an *fs.PathError naming the path. An exclusive create
// (O_CREATE|O_EXCL) never reports ErrIsLink: it fails on any existing entry,
// links included, with an error wrapping fs.ErrExist, as os.OpenFile does.
var ErrIsLink = errors.New("fslink: path is a symlink or junction")

var (
	errNotLink    = errors.New("fslink: not a symlink or junction")
	errOtherLink  = fmt.Errorf("fslink: reading %s destinations: %w", OtherLink, errors.ErrUnsupported)
	errNotLocal   = errors.New("fslink: path is not local to the root")
	errNotRegular = errors.New("fslink: not a regular file")
	errChanged    = errors.New("fslink: path changed while it was being opened")
	// errReparseInRoot reports a non-link reparse point (cloud placeholder,
	// dedup, WOF) under a root: os.Root refuses every reparse point on
	// Windows. Only the Windows lstatAt returns it.
	errReparseInRoot = fmt.Errorf("fslink: reparse point refused under a root: %w", errors.ErrUnsupported)
)

// Classify reports the link kind of path's final component without
// following it. Links in earlier components are followed. A missing path
// returns an error wrapping fs.ErrNotExist.
func Classify(path string) (Kind, error) {
	return classify(path)
}

// IsLink reports whether path's final component is any kind of link.
func IsLink(path string) (bool, error) {
	kind, err := Classify(path)
	if err != nil {
		return false, err
	}
	return kind != NotLink, nil
}

// Readlink returns the destination of the Symlink or Junction at path: a
// location in the directory tree that callers can open or join against. It
// returns an error when path is not a link, and an error wrapping
// errors.ErrUnsupported when Classify reports OtherLink. A volume mount point
// names a volume (\\?\Volume{GUID}\) rather than a directory-tree location,
// and other name-surrogate tags have no format Readlink can decode, so the
// Kind alone decides whether Readlink succeeds.
func Readlink(path string) (string, error) {
	kind, err := Classify(path)
	if err != nil {
		return "", err
	}
	switch kind {
	case NotLink:
		return "", &fs.PathError{Op: "readlink", Path: path, Err: errNotLink}
	case OtherLink:
		return "", &fs.PathError{Op: "readlink", Path: path, Err: errOtherLink}
	case Symlink, Junction:
	}
	return os.Readlink(path)
}

// CreateJunction creates a Windows directory junction at link that points to
// target. target is made absolute with filepath.Abs and must be an existing
// directory on a local volume. Creating a junction requires no special
// privilege. On other platforms it returns an error wrapping
// errors.ErrUnsupported.
func CreateJunction(target, link string) error {
	return createJunction(target, link)
}

// LinkDir creates a link at link to the directory target and reports the
// kind it created. On Unix it creates a symbolic link. On Windows it always
// creates a directory symbolic link, even when target does not exist yet, and
// falls back to a junction when the process lacks the symlink privilege. The
// junction fallback resolves target the way the symlink would have (relative
// to link's parent directory, or a rooted `\dir` on link's volume) and, like
// CreateJunction, requires target to be an existing directory.
func LinkDir(target, link string) (Kind, error) {
	return linkDir(target, link)
}

// OpenFile is like os.OpenFile but refuses to follow a link in path's final
// component, returning an error wrapping ErrIsLink. Links in earlier
// components are followed. O_TRUNC never truncates a link's destination.
// O_CREATE|O_EXCL on an existing entry, a link included, returns an error
// wrapping fs.ErrExist rather than ErrIsLink.
//
// On Windows a non-link reparse point (cloud placeholder, dedup, WOF) opens
// normally. With O_APPEND the returned handle lacks FILE_WRITE_DATA, so every
// write appends even after a Seek. Go does not know the file is in append
// mode (os.NewFile cannot set it), so File.WriteAt is not rejected as it is
// for os.OpenFile; the OS still appends its data.
func OpenFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	return openFile(path, flag, perm)
}

// OpenRegular opens path read-only without following a link in its final
// component and requires the opened file to be a regular file. Opening a
// FIFO does not block.
func OpenRegular(path string) (*os.File, error) {
	return openRegular(path)
}

// ReadFile reads the regular file at path without following a link in its
// final component.
func ReadFile(path string) (data []byte, err error) {
	file, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	return io.ReadAll(file)
}

// OpenRoot is like os.OpenRoot but refuses to follow a link in path's final
// component, returning an error wrapping ErrIsLink. Links in earlier
// components are followed. path must name a directory; a trailing separator
// does not make OpenRoot follow a final link. The returned root is compared
// by file identity with the directory that was inspected, so a link swapped
// in during the open that names a different directory fails the call. A
// link to the inspected directory itself is not detected, and is harmless:
// the root is still that directory.
//
// On Windows a non-link reparse point (cloud placeholder, dedup) opens
// normally as a root, but OpenInRoot and OpenRootNoFollow still refuse
// reparse points below it.
func OpenRoot(path string) (*os.Root, error) {
	if err := platformSupport(); err != nil {
		return nil, &fs.PathError{Op: "openroot", Path: path, Err: err}
	}
	path = trimTrailingSeparators(path)
	dir, err := openDirPath(path)
	if err != nil {
		return nil, err
	}
	// Hold the inspected directory open until the comparison, so its file
	// identity cannot be reused by a replacement.
	defer func() { _ = dir.Close() }()
	want, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	self, err := root.Open(".")
	if err == nil {
		var got fs.FileInfo
		got, err = self.Stat()
		if err == nil && !os.SameFile(want, got) {
			err = &fs.PathError{Op: "openroot", Path: path, Err: errChanged}
		}
		err = errors.Join(err, self.Close())
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// trimTrailingSeparators drops separators after the final element, which
// would make the kernel resolve a final link before a no-follow open sees
// it. It keeps a root ("/", `C:\`) and leaves every other element alone:
// filepath.Clean would also collapse "link/.." lexically, before the kernel
// resolves the link.
func trimTrailingSeparators(path string) string {
	vol := filepath.VolumeName(path)
	rest := path[len(vol):]
	for len(rest) > 1 && os.IsPathSeparator(rest[len(rest)-1]) {
		rest = rest[:len(rest)-1]
	}
	return vol + rest
}

// OpenInRoot opens name inside root like root.OpenFile, but refuses a link
// in any component of name, returning an error wrapping ErrIsLink. name must
// satisfy filepath.IsLocal. O_CREATE|O_EXCL on an existing entry, a link
// included, returns an error wrapping fs.ErrExist rather than ErrIsLink.
//
// Each component is inspected without following it, opened through root,
// and then compared with the inspected entry by file identity. The guarantee
// is that every component opened is the same file object that was inspected
// and found not to be a link, and that nothing resolves outside root. A
// component swapped for a link to a different object makes the open fail. A
// component swapped for a link to the same object is not detected; it is
// harmless because the object opened is still the one inspected. O_TRUNC is
// applied only after the identity check, and O_CREATE creates only with
// O_EXCL semantics, so a racing link never causes a truncation or a create
// through it. Residual races: a swapped-in link is still followed for the open
// itself before the identity check rejects it, so an in-root destination may
// observe an open (for example, a FIFO may block the caller), and an O_CREATE
// open fails rather than retries when an entry appears between inspection and
// creation.
//
// On Windows this builds on os.Root, which refuses every reparse point, so
// OpenInRoot refuses cloud-file placeholders, deduplicated files, and
// WOF-compressed files in any component even though OpenFile accepts them.
// Such an entry returns an error naming the path that says it is a reparse
// point refused under a root.
func OpenInRoot(root *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
	if err := platformSupport(); err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	parts, err := localParts(name)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return root.OpenFile(".", flag, perm)
	}
	parent, err := openDirs(root, parts[:len(parts)-1])
	if err != nil {
		return nil, pathError("open", name, err)
	}
	if parent != root {
		defer func() { _ = parent.Close() }()
	}
	file, err := openFinal(parent, parts[len(parts)-1], flag, perm)
	if err != nil {
		return nil, pathError("open", name, err)
	}
	return file, nil
}

// OpenRootNoFollow opens the directory name inside root as a new root,
// refusing a link in any component of name. name must satisfy
// filepath.IsLocal. The identity guarantee, residual race, and Windows
// reparse-point refusal described on OpenInRoot apply to every component.
func OpenRootNoFollow(root *os.Root, name string) (*os.Root, error) {
	if err := platformSupport(); err != nil {
		return nil, &fs.PathError{Op: "openroot", Path: name, Err: err}
	}
	parts, err := localParts(name)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return root.OpenRoot(".")
	}
	dir, err := openDirs(root, parts)
	if err != nil {
		return nil, pathError("openroot", name, err)
	}
	return dir, nil
}

func localParts(name string) ([]string, error) {
	if !filepath.IsLocal(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errNotLocal}
	}
	clean := filepath.Clean(name)
	if clean == "." {
		return nil, nil
	}
	return strings.Split(clean, string(filepath.Separator)), nil
}

// openDirs walks parts below root, returning root itself when parts is empty.
// Intermediate roots are closed; the caller owns a returned root that is not
// root.
func openDirs(root *os.Root, parts []string) (*os.Root, error) {
	dir := root
	for _, part := range parts {
		next, err := openDir(dir, part)
		if dir != root {
			_ = dir.Close()
		}
		if err != nil {
			return nil, err
		}
		dir = next
	}
	return dir, nil
}

func openDir(dir *os.Root, name string) (*os.Root, error) {
	ent, err := lstatAt(dir, name)
	if err != nil {
		return nil, err
	}
	if !ent.isDir() {
		return nil, syscall.ENOTDIR
	}
	sub, err := dir.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	self, err := sub.Open(".")
	if err == nil {
		err = checkSame(ent, self)
		err = errors.Join(err, self.Close())
	}
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	return sub, nil
}

func openFinal(dir *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
	if flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL {
		// An exclusive create fails on any existing entry, links included.
		file, err := dir.OpenFile(name, flag, perm)
		if err != nil && !errors.Is(err, fs.ErrExist) && entryExists(dir, name) {
			// os.Root on Windows reports an existing directory link as
			// "is a directory"; report every existing entry as fs.ErrExist.
			// Drop the inner PathError so pathError keeps fs.ErrExist.
			if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
				err = pathErr.Err
			}
			err = errors.Join(fs.ErrExist, err)
		}
		return file, err
	}
	ent, err := lstatAt(dir, name)
	if errors.Is(err, fs.ErrNotExist) && flag&os.O_CREATE != 0 {
		return dir.OpenFile(name, flag|os.O_EXCL, perm)
	}
	if err != nil {
		return nil, err
	}
	file, err := dir.OpenFile(name, flag&^(os.O_CREATE|os.O_TRUNC), perm)
	if err != nil {
		return nil, err
	}
	if err := checkSame(ent, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if flag&os.O_TRUNC != 0 {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

// entryExists reports whether lstatAt found an entry, a link or reparse
// point included, at name in dir. A failed lookup proves nothing, so it
// reports false.
func entryExists(dir *os.Root, name string) bool {
	_, err := lstatAt(dir, name)
	return err == nil || errors.Is(err, ErrIsLink) || errors.Is(err, errReparseInRoot)
}

func checkSame(ent entry, file *os.File) error {
	same, err := ent.sameFile(file)
	if err != nil {
		return err
	}
	if !same {
		return errChanged
	}
	return nil
}

// pathError reports err against the caller's full name, keeping the
// underlying cause rather than an inner component's PathError.
func pathError(op, name string, err error) error {
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
		err = pathErr.Err
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}
