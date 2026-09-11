//go:build !linux

package isolate

// NewNativeRunner selects native isolation or refuses an unsupported host.
func NewNativeRunner() (IsolatedRunner, error) {
	return nil, ErrIsolationUnavailable
}
