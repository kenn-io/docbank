package daemon

import "testing"

// StartLivePIDHelperForTest starts a child process that lives until test cleanup.
func StartLivePIDHelperForTest(t *testing.T) int {
	t.Helper()
	return startLivePIDHelper(t)
}
