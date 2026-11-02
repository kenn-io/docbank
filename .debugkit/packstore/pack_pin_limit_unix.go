//go:build unix

package packstore

import "golang.org/x/sys/unix"

func platformPackedSourcePinLimit() int {
	var processLimit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &processLimit); err != nil {
		return fallbackPackedSourcePins
	}
	soft, invalid := normalizePackedSourceSoftLimit(processLimit.Cur)
	return packedSourcePinLimitForReportedSoftLimit(soft, invalid)
}
