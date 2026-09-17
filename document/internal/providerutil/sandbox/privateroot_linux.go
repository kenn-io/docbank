//go:build linux

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const privateRootSize = 64 << 20

// copyRuntimeFile verifies the bytes written into the launcher's private tmpfs.
// No untrusted process runs until the complete runtime has been verified.
func copyRuntimeFile(entry RuntimeFile, target string, remaining int64) (int64, error) {
	if remaining <= 0 {
		return 0, errors.New("sandbox runtime byte limit exceeded")
	}
	fd, err := unix.Open(entry.SourcePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENXIO) {
			return 0, fmt.Errorf("%w: %s", ErrRuntimeSpecialFile, entry.SourcePath)
		}
		return 0, privateRootError("open runtime source", err)
	}
	source := os.NewFile(uintptr(fd), "sandbox-runtime-source")
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil {
		return 0, privateRootError("stat runtime source", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%w: %s", ErrRuntimeSpecialFile, entry.SourcePath)
	}
	if info.Size() < 0 || info.Size() > remaining {
		return 0, errors.New("sandbox runtime byte limit exceeded")
	}
	mode := os.FileMode(0o400)
	if entry.Executable {
		mode = 0o500
	}
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, privateRootError("create runtime file", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, remaining+1))
	closeErr := destination.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return 0, privateRootError("copy runtime file", err)
	}
	if written != info.Size() || written > remaining || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return 0, fmt.Errorf("%w: %s", ErrRuntimeIdentityMismatch, entry.SourcePath)
	}
	return written, nil
}

func installPrivateRoot(control launchControl, executableFD int) error {
	defer func() { _ = unix.Close(executableFD) }()
	root := control.RootDirectory
	devices, err := openPrivateRootDevices()
	if err != nil {
		return err
	}
	defer func() {
		for _, fd := range devices {
			_ = unix.Close(fd)
		}
	}()
	if err := unix.Mount("tmpfs", root, "tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV, fmt.Sprintf("size=%d,mode=755", MaxRuntimeBytes+MaxExecutableBytes+privateRootSize)); err != nil {
		return privateRootError("mount private root", err)
	}
	if err := makePrivateRootSkeleton(root, control.Policy.PrivateRoot.WorkBytes); err != nil {
		return err
	}
	if err := mountPrivateDevices(root, devices); err != nil {
		return err
	}
	if err := attachRuntimeFiles(root, control.Policy); err != nil {
		return err
	}
	if err := attachExecutable(root, control.Policy.Executable, executableFD); err != nil {
		return err
	}
	if err := pivotAndDetachRoot(root); err != nil {
		return err
	}
	return nil
}

func privateRootError(subject string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrPrivateRootUnavailable, subject, err)
}

func openPrivateRootDevices() ([]int, error) {
	paths := []string{rootPath("dev", "null"), rootPath("dev", "zero"), rootPath("dev", "urandom")}
	fds := make([]int, 0, len(paths))
	for _, path := range paths {
		fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			for _, opened := range fds {
				_ = unix.Close(opened)
			}
			return nil, privateRootError("open device", err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFCHR {
			_ = unix.Close(fd)
			for _, opened := range fds {
				_ = unix.Close(opened)
			}
			return nil, fmt.Errorf("%w: device is not a character device: %s", ErrPrivateRootUnavailable, path)
		}
		fds = append(fds, fd)
	}
	return fds, nil
}

func makePrivateRootSkeleton(root string, workBytes int64) error {
	for _, directory := range []string{
		"bin", "dev", "etc", "lib", "lib64", "proc", "usr", "work", "tmp",
		"work/home", "work/home/cache", "work/out", "work/profile", "work/profile/user",
		"work/tmp", "oldroot",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(directory)), 0o755); err != nil {
			return privateRootError("create root skeleton", err)
		}
	}
	// ponytail: noexec work storage, upgrade trigger: a proved writable runtime work policy.
	if err := unix.Mount("tmpfs", filepath.Join(root, "work"), "tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC,
		fmt.Sprintf("size=%d,mode=1777", workBytes)); err != nil {
		return privateRootError("mount private work", err)
	}
	for _, directory := range []string{"home", "home/cache", "out", "profile", "profile/user", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, "work", filepath.FromSlash(directory)), 0o700); err != nil {
			return privateRootError("create work skeleton", err)
		}
	}
	if err := unix.Mount(filepath.Join(root, "work", "tmp"), filepath.Join(root, "tmp"), "", unix.MS_BIND, ""); err != nil {
		return privateRootError("alias private tmp", err)
	}
	if err := writeGeneratedEtc(root); err != nil {
		return err
	}
	return nil
}

func writeGeneratedEtc(root string) error {
	values := map[string]string{ //nolint:gosec // generated configuration, not a credential
		"passwd":        "renderer:x:0:0:renderer:/work/home:/bin/false\n",
		"nsswitch.conf": "passwd: files\ngroup: files\nhosts: files\n",
	}
	for name, value := range values {
		if err := os.WriteFile(filepath.Join(root, "etc", name), []byte(value), 0o600); err != nil {
			return privateRootError("write generated etc", err)
		}
	}
	return nil
}

func mountPrivateDevices(root string, devices []int) error {
	if err := unix.Mount("tmpfs", filepath.Join(root, "dev"), "tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=755,size=1m"); err != nil {
		return privateRootError("mount private dev", err)
	}
	for index, name := range []string{"null", "zero", "urandom"} {
		target := filepath.Join(root, "dev", name)
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return privateRootError("create private device target", err)
		}
		_ = file.Close()
		if err := unix.Mount(procFD(devices[index]), target, "", unix.MS_BIND, ""); err != nil {
			return privateRootError("bind private device", err)
		}
	}
	return nil
}

func attachRuntimeFiles(root string, policy Policy) error {
	private := policy.PrivateRoot
	// ponytail: per-file mounts preserve noexec policy; use a runtime image if measured mount cost dominates.
	var total int64
	for _, entry := range private.Runtime {
		if entry.GuestPath == policy.Executable {
			if entry.SHA256 != policy.ExecutableSHA256 {
				return ErrRuntimeIdentityMismatch
			}
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(entry.GuestPath, "/")))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return privateRootError("create runtime parent", err)
		}
		size, err := copyRuntimeFile(entry, target, MaxRuntimeBytes-total)
		if err != nil {
			return err
		}
		total += size
		flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
		if !entry.Executable {
			flags |= unix.MS_NOEXEC
		}
		if err := unix.Mount(target, target, "", unix.MS_BIND, ""); err != nil {
			return privateRootError("bind runtime file", err)
		}
		if err := unix.Mount("", target, "", flags, ""); err != nil {
			return privateRootError("remount runtime file", err)
		}
	}
	for _, link := range private.Symlinks {
		target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(link.GuestPath, "/")))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return privateRootError("create symlink parent", err)
		}
		if info, err := os.Lstat(target); err == nil && info.IsDir() {
			if err := os.Remove(target); err != nil {
				return privateRootError("remove symlink placeholder", err)
			}
		}
		if err := os.Symlink(link.Target, target); err != nil {
			return privateRootError("create runtime symlink", err)
		}
	}
	return nil
}

func attachExecutable(root, path string, executableFD int) error {
	target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "/")))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return privateRootError("create executable parent", err)
	}
	source, err := os.Open(procFD(executableFD))
	if err != nil {
		return privateRootError("open sealed executable", err)
	}
	defer func() { _ = source.Close() }()
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o500) //nolint:gosec // the pinned renderer must be executable inside the private root
	if err != nil {
		return privateRootError("create executable target", err)
	}
	_, copyErr := io.Copy(file, source)
	return errors.Join(copyErr, file.Close())
}

func procFD(fd int) string {
	return filepath.Join("/proc/self/fd", strconv.Itoa(fd))
}

func pivotAndDetachRoot(root string) error {
	if err := unix.PivotRoot(root, filepath.Join(root, "oldroot")); err != nil {
		return privateRootError("pivot private root", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return privateRootError("enter private root", err)
	}
	if err := unix.Unmount("/oldroot", unix.MNT_DETACH); err != nil {
		return privateRootError("detach old root", err)
	}
	if err := os.RemoveAll("/oldroot"); err != nil {
		return privateRootError("remove old root", err)
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_RDONLY, ""); err != nil {
		return privateRootError("mount private proc", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return privateRootError("remount private root", err)
	}
	if err := unix.Chdir("/work"); err != nil {
		return privateRootError("enter private work", err)
	}
	return nil
}

func installPrivateLandlock(executablePath string, root *PrivateRoot) error {
	paths := runtimeLandlockPaths(executablePath, root)
	return installLandlock(privateRootLandlockPaths(), privateRootLandlockFiles(), privateRootLandlockDevices(), true, paths...)
}

func runtimeLandlockPaths(executablePath string, root *PrivateRoot) []string {
	seen := map[string]struct{}{filepath.Dir(executablePath): {}}
	if root != nil {
		for _, file := range root.Runtime {
			seen[filepath.Dir(file.GuestPath)] = struct{}{}
		}
		for _, link := range root.Symlinks {
			seen[filepath.Dir(link.GuestPath)] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func installExecLandlock() error {
	return installLandlockWithWritable(execLandlockPaths(), execLandlockFiles(), execLandlockDevices(), false, nil, []string{rootPath("tmp")})
}

func installLandlock(paths, files []string, devices map[string]uint64, private bool, extraPaths ...string) error {
	return installLandlockWithWritable(paths, files, devices, private, extraPaths, nil)
}

func installLandlockWithWritable(paths, files []string, devices map[string]uint64, private bool, extraPaths, writablePaths []string) error {
	version, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION, 0, 0, 0,
	)
	if errno != 0 || version < 3 {
		return fmt.Errorf("require Landlock filesystem ABI 3: %w", errno)
	}
	handledAccess := uint64(
		unix.LANDLOCK_ACCESS_FS_EXECUTE |
			unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
			unix.LANDLOCK_ACCESS_FS_READ_FILE |
			unix.LANDLOCK_ACCESS_FS_READ_DIR |
			unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
			unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
			unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
			unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
			unix.LANDLOCK_ACCESS_FS_MAKE_REG |
			unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
			unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
			unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
			unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
			unix.LANDLOCK_ACCESS_FS_REFER |
			unix.LANDLOCK_ACCESS_FS_TRUNCATE)
	if version >= 5 {
		handledAccess |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	attribute := unix.LandlockRulesetAttr{Access_fs: handledAccess}
	//nolint:gosec // Landlock requires a pointer to this fixed ABI structure.
	rulesetFD, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attribute)), unsafe.Sizeof(attribute), 0, 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("create Landlock ruleset: %w", errno)
	}
	defer func() { _ = unix.Close(int(rulesetFD)) }()
	readOnly := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	if err := addLandlockPaths(rulesetFD, paths, readOnly, !private); err != nil {
		return err
	}
	for _, path := range extraPaths {
		if err := addLandlockPath(rulesetFD, path, readOnly); err != nil {
			return err
		}
	}
	for _, path := range writablePaths {
		writableAccess := handledAccess &^ (unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
		if err := addLandlockPath(rulesetFD, path, writableAccess); err != nil {
			return err
		}
	}
	if err := addLandlockPaths(rulesetFD, files, unix.LANDLOCK_ACCESS_FS_READ_FILE, !private); err != nil {
		return err
	}
	for path, access := range devices {
		if err := addLandlockPath(rulesetFD, path, access); err != nil {
			return err
		}
	}
	if private {
		workAccess := handledAccess &^ (unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
		for _, path := range []string{rootPath("work"), rootPath("tmp")} {
			if err := addLandlockPath(rulesetFD, path, workAccess); err != nil {
				return err
			}
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, rulesetFD, 0, 0); errno != 0 {
		return fmt.Errorf("enforce Landlock ruleset: %w", errno)
	}
	return nil
}

func addLandlockPaths(rulesetFD uintptr, paths []string, access uint64, ignoreMissing bool) error {
	for _, path := range paths {
		if err := addLandlockPath(rulesetFD, path, access); err != nil {
			if ignoreMissing && errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
	}
	return nil
}

func addLandlockPath(rulesetFD uintptr, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open Landlock path %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	attribute := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)} //nolint:gosec
	//nolint:gosec // Landlock requires a pointer to this fixed ABI structure.
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, rulesetFD, unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&attribute)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("add Landlock path %s: %w", path, errno)
	}
	return nil
}
