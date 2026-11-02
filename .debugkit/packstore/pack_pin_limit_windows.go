//go:build windows

package packstore

// Windows handles are not governed by a small POSIX-style per-process soft
// limit, so use the target-derived ceiling while remaining explicitly bounded.
func platformPackedSourcePinLimit() int { return maxPackedSourcePins }
