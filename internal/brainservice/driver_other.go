//go:build !linux && !darwin && !windows

package brainservice

func NewDriver(runner Runner) Driver { return newServiceDriver("unsupported", runner, nil) }
