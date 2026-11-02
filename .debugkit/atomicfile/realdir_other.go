//go:build !windows

package atomicfile

import (
	"path/filepath"
	"strings"
)

// resolveLinkDest returns the path named by a link in linkDir whose
// destination is dest, resolved the way the kernel resolves it: a relative
// dest starts at linkDir's real directory, and every ".." applies after the
// link before it is resolved. filepath.Join would apply ".." lexically first,
// so `hop/../target` with hop a directory symlink would name the wrong file.
// Instead the destination is kept unclean, split before its last element,
// and its parent part resolved with filepath.EvalSymlinks, which resolves
// each link before applying the ".." after it.
func resolveLinkDest(linkDir, dest string) (string, error) {
	full := dest
	if !filepath.IsAbs(dest) {
		parent, err := filepath.EvalSymlinks(linkDir)
		if err != nil {
			return "", err
		}
		full = parent + "/" + dest
	}
	i := strings.LastIndexByte(full, '/')
	parent, base := full[:i+1], full[i+1:]
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, base), nil
}

// canonicalPath returns path with its parent resolved the way the kernel
// resolves it and made absolute. filepath.Dir would clean the caller's path
// first, so `a/hop/../x` with hop a directory symlink would name a sibling
// of a instead of the directory the kernel reaches; staging, link
// resolution and the link recheck at Commit all work from this form.
func canonicalPath(path string) (string, error) {
	parent, base := ".", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		parent, base = path[:i+1], path[i+1:]
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, base), nil
}
