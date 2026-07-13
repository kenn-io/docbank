package api

type restoreTargetLock interface {
	Release() error
}
