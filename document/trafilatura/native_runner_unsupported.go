//go:build !linux

package trafilatura

func newNativeRunner() (IsolatedRunner, error) {
	return nil, ErrIsolationUnavailable
}
