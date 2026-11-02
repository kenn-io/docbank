//go:build windows

package fslink_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/fslink"
)

func symlinkPrivilegeMissing(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}

// platformLinkMakers adds a directory junction, which needs no privilege and
// so never skips.
func platformLinkMakers() []linkMaker {
	return []linkMaker{{
		name:  "junction to dir",
		kind:  fslink.Junction,
		toDir: true,
		create: func(t *testing.T, dir, name string) string {
			t.Helper()
			target := filepath.Join(dir, "sub")
			require.NoError(t, fslink.CreateJunction(target, filepath.Join(dir, name)))
			return target
		},
	}}
}
