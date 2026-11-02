//go:build !unix

package openssh

import "context"

type socketDialState uint8

const (
	socketAbsent socketDialState = iota
	socketListening
	socketStale
)

func persistentSupportError() error {
	return ErrPersistentUnsupported
}

func ensurePersistentDirectory(string) error {
	return ErrPersistentUnsupported
}

func validateControlSocket(string) (bool, error) {
	return false, ErrPersistentUnsupported
}

func inspectControlSocket(context.Context, string) (socketDialState, error) {
	return socketAbsent, ErrPersistentUnsupported
}
