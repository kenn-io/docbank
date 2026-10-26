//go:build unix && !linux && !darwin

package ingest

import "syscall"

func unixWatchBirth(_ *syscall.Stat_t) watchBirthStamp { return watchBirthStamp{} }
