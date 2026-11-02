package safefileio

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
)

const (
	darwinACLExtended   = 0x00000100
	darwinACLFirstEntry = 0
)

type darwinACLAPI struct {
	getFD    uintptr
	getEntry func(uintptr, int32, *uintptr) int32
	free     func(uintptr) int32
}

var (
	darwinACLOnce sync.Once
	darwinACL     darwinACLAPI
	errDarwinACL  error
)

// ValidatePrivateCurrentUserFile verifies that an open current-user-owned file
// has private mode bits and no macOS extended ACL.
func ValidatePrivateCurrentUserFile(file *os.File) error {
	return validatePrivateCurrentUserFile(file, validateDarwinExtendedACL)
}

func validateDarwinExtendedACL(file *os.File) error {
	darwinACLOnce.Do(loadDarwinACL)
	if errDarwinACL != nil {
		return errDarwinACL
	}
	// Capture errno before resuming Go: even a locked thread's errno can be
	// changed by the runtime between the native call and a later read.
	acl, _, errno := purego.SyscallN(darwinACL.getFD, file.Fd(), darwinACLExtended)
	runtime.KeepAlive(file)
	if acl == 0 {
		// Darwin errno is a C int; the native trampoline returns a word.
		callErr := syscall.Errno(uint32(errno))
		if callErr == syscall.ENOENT {
			return nil
		}
		return fmt.Errorf("read extended ACL: %w", callErr)
	}
	defer func() { _ = darwinACL.free(acl) }()
	var entry uintptr
	result := darwinACL.getEntry(acl, darwinACLFirstEntry, &entry)
	switch result {
	case 0:
		return errors.New("safefileio: file has a macOS extended ACL")
	default:
		return errors.New("read extended ACL entry failed")
	}
}

func loadDarwinACL() {
	handle, err := purego.Dlopen(
		"/usr/lib/libSystem.B.dylib",
		purego.RTLD_NOW|purego.RTLD_LOCAL,
	)
	if err != nil {
		errDarwinACL = fmt.Errorf("load macOS ACL API: %w", err)
		return
	}
	darwinACL.getFD, err = purego.Dlsym(handle, "acl_get_fd_np")
	if err != nil {
		errDarwinACL = fmt.Errorf("load macOS ACL function acl_get_fd_np: %w", err)
		return
	}
	for name, target := range map[string]any{
		"acl_get_entry": &darwinACL.getEntry,
		"acl_free":      &darwinACL.free,
	} {
		symbol, symbolErr := purego.Dlsym(handle, name)
		err = symbolErr
		if err != nil {
			errDarwinACL = fmt.Errorf("load macOS ACL function %s: %w", name, err)
			return
		}
		purego.RegisterFunc(target, symbol)
	}
}
