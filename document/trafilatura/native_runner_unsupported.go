//go:build !linux

package trafilatura

func validateNativeExecutable(string) error { return ErrIsolationUnavailable }

func newNativeRunner() (IsolatedRunner, error) {
	return nil, ErrIsolationUnavailable
}
