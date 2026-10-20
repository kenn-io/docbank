// Package sqlitetest controls SQLite settings for disposable test databases.
package sqlitetest

import "sync/atomic"

// DisableSync disables synchronization on writable connections opened by the
// built-in adapters. Only test setup should enable it.
var DisableSync atomic.Bool
