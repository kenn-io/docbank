//go:build !unix && !windows

package fslink_test

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
)

// The root itself needs no link check, but these platforms cannot check any
// component, so even "." must fail closed.
func TestRootOpensFailClosed(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	file, err := fslink.OpenInRoot(root, ".", os.O_RDONLY, 0)
	if file != nil {
		_ = file.Close()
	}
	require.ErrorIs(t, err, errors.ErrUnsupported)

	sub, err := fslink.OpenRootNoFollow(root, ".")
	if sub != nil {
		_ = sub.Close()
	}
	require.ErrorIs(t, err, errors.ErrUnsupported)
}
