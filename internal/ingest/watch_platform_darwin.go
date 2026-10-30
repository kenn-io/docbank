//go:build darwin

package ingest

import "syscall"

func unixWatchBirth(stat *syscall.Stat_t) watchBirthStamp {
	return validWatchBirth(stat.Birthtimespec.Sec, stat.Birthtimespec.Nsec)
}
