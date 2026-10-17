package daemonconn

import "go.kenn.io/docbank/internal/api"

// BackupCleanupError preserves cleanup progress from a daemon error response.
// Unwrap retains the original problem code, status, and error classification.
type BackupCleanupError struct {
	Forget *api.BackupForgetReport
	Prune  *api.BackupPruneReport
	err    error
}

func (e *BackupCleanupError) Error() string { return e.err.Error() }
func (e *BackupCleanupError) Unwrap() error { return e.err }
