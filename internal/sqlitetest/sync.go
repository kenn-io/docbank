// Package sqlitetest controls SQLite settings for disposable test databases.
package sqlitetest

import "sync/atomic"

// DisableSync disables synchronization on writable connections opened by the
// built-in adapters. The process-wide setting covers direct store-test opens
// and later pool connections without changing the public adapter API.
// It defaults to false for ordinary opens; enable it only for disposable databases.
var DisableSync atomic.Bool
