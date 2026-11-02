//go:build !windows

package renamebusy

// fileBusy is false outside Windows because this sharing violation is specific
// to Windows file handles.
func fileBusy(error) bool { return false }
