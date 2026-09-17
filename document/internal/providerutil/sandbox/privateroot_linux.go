//go:build linux

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	privateRootMountPath = "/tmp/root"
	privateRootSize      = 64 << 20
	runtimeFDBase        = launcherStatusFD + 1
)

func preflightRuntimeFDLimit(runtimeEntries int) error {
	if runtimeEntries < 0 || runtimeEntries > MaxRuntimeEntries {
		return ErrPrivateRootUnavailable
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("%w: query file descriptor limit: %w", ErrPrivateRootUnavailable, err)
	}
	required := uint64(runtimeFDBase+runtimeEntries) + 64
	if limit.Cur < required && limit.Max >= required {
		limit.Cur = limit.Max
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err == nil {
			return nil
		}
	}
	if limit.Cur < required {
		return fmt.Errorf("%w: need %d file descriptors, have %d", ErrPrivateRootUnavailable, required, limit.Cur)
	}
	return nil
}

func prepareRuntimeFiles(ctx context.Context, root *PrivateRoot) ([]*os.File, error) {
	// ponytail: per-file snapshot and mount cost, upgrade trigger: content-addressed runtime image.
	if err := preflightRuntimeFDLimit(len(root.Runtime)); err != nil {
		return nil, err
	}
	files := make([]*os.File, 0, len(root.Runtime))
	var total int64
	ok := false
	defer func() {
		if !ok {
			for _, file := range files {
				_ = file.Close()
			}
		}
	}()
	for _, entry := range root.Runtime {
		file, size, err := sealRuntimeFile(ctx, entry, MaxRuntimeBytes-total)
		if err != nil {
			return nil, err
		}
		total += size
		files = append(files, file)
	}
	ok = true
	return files, nil
}

func sealRuntimeFile(ctx context.Context, entry RuntimeFile, remaining int64) (*os.File, int64, error) {
	if remaining <= 0 {
		return nil, 0, errors.New("sandbox runtime byte limit exceeded")
	}
	fd, err := unix.Open(entry.SourcePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENXIO) {
			return nil, 0, fmt.Errorf("%w: %s", ErrRuntimeSpecialFile, entry.SourcePath)
		}
		return nil, 0, fmt.Errorf("%w: open runtime %s: %w", ErrPrivateRootUnavailable, entry.SourcePath, err)
	}
	source := os.NewFile(uintptr(fd), "sandbox-runtime-source")
	defer func() { _ = source.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, 0, fmt.Errorf("%w: stat runtime %s: %w", ErrPrivateRootUnavailable, entry.SourcePath, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, 0, fmt.Errorf("%w: %s", ErrRuntimeSpecialFile, entry.SourcePath)
	}
	if stat.Size < 0 || stat.Size > remaining {
		return nil, 0, errors.New("sandbox runtime byte limit exceeded")
	}
	memfd, err := unix.MemfdCreate("docbank-runtime", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: create runtime memfd: %w", ErrPrivateRootUnavailable, err)
	}
	result := os.NewFile(uintptr(memfd), "sandbox-runtime")
	valid := false
	defer func() {
		if !valid {
			_ = result.Close()
		}
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(result, hash),
		io.LimitReader(contextReader{ctx: ctx, reader: source}, remaining+1))
	if copyErr != nil {
		return nil, 0, copyErr
	}
	if written != stat.Size || written > remaining {
		return nil, 0, errors.New("sandbox runtime changed during sealing")
	}
	if hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return nil, 0, fmt.Errorf("%w: %s", ErrRuntimeIdentityMismatch, entry.SourcePath)
	}
	mode := uint32(0o400)
	if entry.Executable {
		mode = 0o500
	}
	if err := unix.Fchmod(memfd, mode); err != nil {
		return nil, 0, fmt.Errorf("%w: chmod runtime memfd: %w", ErrPrivateRootUnavailable, err)
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(result.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, 0, fmt.Errorf("%w: seal runtime memfd: %w", ErrPrivateRootUnavailable, err)
	}
	if _, err := unix.FcntlInt(result.Fd(), unix.F_GET_SEALS, 0); err != nil {
		return nil, 0, fmt.Errorf("%w: verify runtime seals: %w", ErrPrivateRootUnavailable, err)
	}
	if _, err := result.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	valid = true
	return result, written, nil
}

func installPrivateRoot(control launchControl, executableFD int) error {
	root := privateRootMountPath
	stage := filepath.Join("/tmp", fmt.Sprintf("docbank-runtime-stage-%d", os.Getpid()))
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return privateRootError("create runtime staging root", err)
	}
	if err := unix.Mount("tmpfs", stage, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV,
		fmt.Sprintf("size=%d,mode=755", MaxRuntimeBytes)); err != nil {
		return privateRootError("mount runtime staging root", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return privateRootError("create root mount point", err)
	}
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
		unix.MS_NOSUID|unix.MS_NODEV, fmt.Sprintf("size=%d,mode=755", privateRootSize)); err != nil {
		return privateRootError("mount private root", err)
	}
	if err := makePrivateRootSkeleton(root, control.Policy.PrivateRoot.WorkBytes); err != nil {
		return err
	}
	if err := mountPrivateDevices(root, devices); err != nil {
		return err
	}
	if err := attachRuntimeFiles(root, stage, control.Policy.PrivateRoot, runtimeFDBase, control.Policy.Executable); err != nil {
		return err
	}
	if err := attachExecutable(root, stage, control.Policy.Executable, executableFD); err != nil {
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
		"mnt",
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

func attachRuntimeFiles(root, stage string, private *PrivateRoot, firstFD int, executablePath string) error {
	for _, entry := range private.Runtime {
		if entry.GuestPath == executablePath {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(entry.GuestPath, "/")))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return privateRootError("create runtime parent", err)
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
		if err != nil {
			return privateRootError("create runtime target", err)
		}
		_ = file.Close()
	}
	for index, entry := range private.Runtime {
		if entry.GuestPath == executablePath {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(entry.GuestPath, "/")))
		stageTarget := filepath.Join(stage, filepath.FromSlash(strings.TrimPrefix(entry.GuestPath, "/")))
		if err := os.MkdirAll(filepath.Dir(stageTarget), 0o755); err != nil {
			return privateRootError("create runtime staging parent", err)
		}
		file, err := os.OpenFile(stageTarget, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
		if err != nil {
			return privateRootError("create runtime staging target", err)
		}
		_ = file.Close()
		if err := materializeDescriptor(firstFD+index, stageTarget, entry.Executable); err != nil {
			return privateRootError("materialize runtime file", err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return privateRootError("create runtime parent", err)
		}
		flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
		if !entry.Executable {
			flags |= unix.MS_NOEXEC
		}
		if err := unix.Mount(stageTarget, target, "", unix.MS_BIND, ""); err != nil {
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

func attachExecutable(root, stage, path string, executableFD int) error {
	target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "/")))
	stageTarget := filepath.Join(stage, ".executable")
	file, err := os.OpenFile(stageTarget, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return privateRootError("create executable staging target", err)
	}
	_ = file.Close()
	if err := materializeDescriptor(executableFD, stageTarget, true); err != nil {
		return privateRootError("materialize executable", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return privateRootError("create executable parent", err)
	}
	file, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return privateRootError("create executable target", err)
	}
	_ = file.Close()
	if err := unix.Mount(stageTarget, target, "", unix.MS_BIND, ""); err != nil {
		return privateRootError("bind executable", err)
	}
	if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID, ""); err != nil {
		return privateRootError("remount executable", err)
	}
	return nil
}

func materializeDescriptor(fd int, target string, executable bool) error {
	source, err := os.Open(procFD(fd))
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0o400)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	mode := uint32(0o400)
	if executable {
		mode = 0o500
	}
	if err := unix.Chmod(target, mode); err != nil {
		return fmt.Errorf("chmod materialized descriptor: %w", err)
	}
	return nil
}

//nolint:unused // descriptor-backed mount path for kernels that support it.
func moveDescriptorMount(fd int, target string) error {
	tree, err := unix.OpenTree(unix.AT_FDCWD, procFD(fd), unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("open descriptor tree: %w", err)
	}
	defer func() { _ = unix.Close(tree) }()
	if err := unix.MoveMount(tree, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move descriptor tree: %w", err)
	}
	return nil
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

func installPrivateLandlock() error {
	return installLandlock(privateRootLandlockPaths(), privateRootLandlockFiles(), privateRootLandlockDevices(), true)
}

func installExecLandlock() error {
	return installLandlock(execLandlockPaths(), execLandlockFiles(), execLandlockDevices(), false)
}

func installLandlock(paths, files []string, devices map[string]uint64, private bool) error {
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
	for _, path := range paths {
		if err := addLandlockPath(rulesetFD, path, readOnly); err != nil {
			return err
		}
	}
	for _, path := range files {
		if err := addLandlockPath(rulesetFD, path, unix.LANDLOCK_ACCESS_FS_READ_FILE); err != nil {
			return err
		}
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
