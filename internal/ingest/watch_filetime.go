package ingest

// FILETIME is an unsigned count of 100 ns ticks since 1601. Validate the raw
// value before converting; Filetime.Nanoseconds overflows for sentinel values.
func watchBirthFromFiletimeTicks(ticks uint64) watchBirthStamp {
	const unixEpochTicks uint64 = 116444736000000000
	const maxFiletimeTicks uint64 = 2650467743999999999 // 9999-12-31 23:59:59.9999999 UTC
	if ticks <= unixEpochTicks || ticks > maxFiletimeTicks {
		return watchBirthStamp{}
	}
	elapsed := ticks - unixEpochTicks
	return validWatchBirth(int64(elapsed/10000000), int64(elapsed%10000000)*100)
}
