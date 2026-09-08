package qmdexport

import (
	"math"
	"os"
	"runtime"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

func nativeMove(sourceParent *anchoredDir, name string, source *os.File, targetParent *anchoredDir, target string, replace bool) error {
	if replace {
		return nativeError(unix.Renameat(int(sourceParent.file.Fd()), name, int(targetParent.file.Fd()), target))
	}
	return nativeError(unix.RenameatxNp(int(sourceParent.file.Fd()), name, int(targetParent.file.Fd()), target, unix.RENAME_EXCL))
}

var directoryACL struct {
	once  sync.Once
	err   error
	getFD func(int32, int32) uintptr
	free  func(uintptr) int32
	errno func() *int32
}

// The held-directory equivalent of Kit's macOS regular-file ACL contract.
// Loading libc through the existing purego dependency also supports CGO=0.
func nativeDirectoryACL(file *os.File) error {
	directoryACL.once.Do(func() {
		handle, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			directoryACL.err = errPrivate
			return
		}
		for name, target := range map[string]any{"acl_get_fd_np": &directoryACL.getFD, "acl_free": &directoryACL.free, "__error": &directoryACL.errno} {
			symbol, err := purego.Dlsym(handle, name)
			if err != nil {
				directoryACL.err = errPrivate
				return
			}
			purego.RegisterFunc(target, symbol)
		}
	})
	if directoryACL.err != nil {
		return directoryACL.err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	errno := directoryACL.errno()
	*errno = 0
	fd := file.Fd()
	if fd > math.MaxInt32 {
		return errEntry
	}
	acl := directoryACL.getFD(int32(fd), 0x100)
	if acl == 0 {
		if *errno == int32(syscall.ENOENT) {
			return nil
		}
		return errPrivate
	}
	defer func() { _ = directoryACL.free(acl) }()
	// Only the null+ENOENT absence result above passes. A nonnull ACL is
	// denied regardless of its entries; do not broaden the privacy policy.
	return errPrivate
}
