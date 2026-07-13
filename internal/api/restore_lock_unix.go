//go:build unix

package api

import "go.kenn.io/docbank/internal/home"

func acquireRestoreTargetLock(target string) (restoreTargetLock, error) {
	return (home.Layout{Root: target}).TryLockExclusive()
}
