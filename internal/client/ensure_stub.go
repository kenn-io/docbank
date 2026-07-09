//go:build !unix

package client

import (
	"context"
	"errors"

	kitdaemon "go.kenn.io/kit/daemon"
)

var errUnsupported = errors.New(
	"docbank requires a Unix-like OS: daemon discovery uses process signaling")

// Find fails: daemon discovery is Unix-only.
func Find(context.Context, string) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
	return kitdaemon.RuntimeRecord{}, kitdaemon.PingInfo{}, false, errUnsupported
}

// Ensure fails: daemon auto-start is Unix-only.
func Ensure(context.Context) (*Client, error) {
	return nil, errUnsupported
}

// EnsureDaemon fails: daemon auto-start is Unix-only.
func EnsureDaemon(context.Context, string) (EnsureResult, error) {
	return EnsureResult{}, errUnsupported
}

// WithLaunchLock fails: daemon auto-start is Unix-only.
func WithLaunchLock(context.Context, string, func() error) error {
	return errUnsupported
}

// Start fails: daemon auto-start is Unix-only.
func Start(context.Context, string) (kitdaemon.RuntimeRecord, error) {
	return kitdaemon.RuntimeRecord{}, errUnsupported
}

// StartAnyVersion fails: daemon auto-start is Unix-only.
func StartAnyVersion(context.Context, string) (kitdaemon.RuntimeRecord, error) {
	return kitdaemon.RuntimeRecord{}, errUnsupported
}

// Stop fails: daemon discovery is Unix-only.
func Stop(context.Context, string) (bool, error) {
	return false, errUnsupported
}
