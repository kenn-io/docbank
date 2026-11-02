//go:build !unix && !windows

package packstore

func platformPackedSourcePinLimit() int { return fallbackPackedSourcePins }
