//go:build windows

package packstore

import "os"

// Windows does not expose a portable directory-entry flush through os.Root.
// Pack contents are still synced before their canonical link is created.
func syncRootDirPlatform(*os.Root) error { return nil }
