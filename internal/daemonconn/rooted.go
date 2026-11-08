package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// EnsureFor acquires a daemon at a saved absolute root, independent of the
// process's current directory or DOCBANK_HOME.
func EnsureFor(ctx context.Context, root string) (*Connection, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("saved daemon root must be absolute")
	}
	res, err := EnsureDaemon(ctx, root)
	if err != nil {
		return nil, err
	}
	c, err := newProvenClientFor(ctx, res.Record)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTransientDaemonAcquisition, err)
	}
	return c, nil
}

// RunningFor discovers a compatible daemon at a saved absolute root without
// starting one or creating the vault.
func RunningFor(ctx context.Context, root string) (*Connection, bool, error) {
	if !filepath.IsAbs(root) {
		return nil, false, errors.New("saved daemon root must be absolute")
	}
	rec, _, ok, err := discoverWithOptions(ctx, root, discoverOptions(true))
	if err != nil || !ok {
		return nil, false, err
	}
	c, err := newProvenClientFor(ctx, rec)
	return c, err == nil, err
}
