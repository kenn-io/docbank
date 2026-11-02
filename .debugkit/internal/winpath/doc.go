// Package winpath prepares Windows paths for direct Win32 file API calls.
//
// The os package adds the \\?\ long-path prefix to long paths before it calls
// Win32, so os.OpenFile accepts paths past MAX_PATH even where the system
// long-path setting is off. Code that calls CreateFile, MoveFileEx or similar
// APIs itself must do the same, or a path that os accepts fails in kit.
package winpath
