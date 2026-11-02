//go:build unix

package openssh

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"

	"go.kenn.io/kit/safefileio"
)

type socketDialState uint8

const (
	socketAbsent socketDialState = iota
	socketListening
	socketStale
)

func persistentSupportError() error {
	return nil
}

// ensurePersistentDirectory establishes an owner-only boundary for control
// sockets. Its parent chain must already be trusted by the caller, such as a
// path below the user's home or a private runtime directory.
func ensurePersistentDirectory(path string) error {
	return safefileio.EnsurePrivateDir(path)
}

func validateControlSocket(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, &ControlPathSecurityError{Path: path, Reason: "inspect entry", Err: err}
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false, &ControlPathSecurityError{Path: path, Reason: "entry is not a Unix socket"}
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return false, &ControlPathSecurityError{Path: path, Reason: "socket is not owned by current user"}
	}
	return true, nil
}

func inspectControlSocket(ctx context.Context, path string) (socketDialState, error) {
	exists, err := validateControlSocket(path)
	if err != nil {
		return socketAbsent, err
	}
	if !exists {
		return socketAbsent, nil
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err == nil {
		_ = conn.Close()
		return socketListening, nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return socketStale, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return socketAbsent, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return socketAbsent, errors.Join(ErrProbeIndeterminate, ctxErr, err)
	}
	return socketAbsent, errors.Join(ErrProbeIndeterminate, err)
}
