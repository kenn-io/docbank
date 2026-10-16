package checkout

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestCheckoutRejectsStartupCleanupDescendantBindMount(t *testing.T) {
	for _, name := range []string{"web-launch", "web-downloads"} {
		t.Run(name, func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "bytes")
			owned := filepath.Join(s.VaultRoot, name, "nested", "checkout")
			require.NoError(t, os.MkdirAll(owned, 0o700))
			destination := t.TempDir()
			bindMountForTest(t, owned, destination)
			assertStartupCleanupCheckoutRejected(t, s, a, destination)
		})
	}
}

func bindMountForTest(t *testing.T, source, target string) {
	t.Helper()
	err := unix.Mount(source, target, "", unix.MS_BIND, "")
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("bind mounts are not permitted in this test environment: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, unix.Unmount(target, unix.MNT_DETACH))
	})
}
