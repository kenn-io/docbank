//go:build !unix

package api

type noopRestoreTargetLock struct{}

func (noopRestoreTargetLock) Release() error { return nil }

func acquireRestoreTargetLock(string) (restoreTargetLock, error) {
	return noopRestoreTargetLock{}, nil
}
