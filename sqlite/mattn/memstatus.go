//go:build cgo

package mattn

/*
// mattn/go-sqlite3 compiles SQLite into this binary but does not expose
// sqlite3_config, so declare the public entry point here.
int sqlite3_config(int, ...);

static int docbank_disable_sqlite_memstatus(void) { return sqlite3_config(9, 0); }

// SQLITE_CONFIG_MEMSTATUS is 9 in sqlite3.h.
*/
import "C"

import (
	"fmt"
	"sync"
)

// sqliteMisuse is SQLITE_MISUSE, which sqlite3_config returns once SQLite
// has initialized.
const sqliteMisuse = 21

// disableMemoryStatusOnce runs on the first Open through Driver, so only
// processes that open a vault through this adapter change the setting.
var disableMemoryStatusOnce = sync.OnceValue(disableMemoryStatus)

// mattn/go-sqlite3 builds SQLite with memory statistics on. They make every
// SQLite allocation in the process take one global mutex, so concurrent
// connections to separate databases serialize on it. Docbank reads no SQLite
// memory statistics or heap limits. The setting is process-wide and also turns
// off PRAGMA soft_heap_limit and hard_heap_limit enforcement, so it waits for
// the first Docbank open. If the application already initialized SQLite,
// sqlite3_config leaves its settings and connections alone.
func disableMemoryStatus() error {
	switch rc := C.docbank_disable_sqlite_memstatus(); rc {
	case 0, sqliteMisuse:
		return nil
	default:
		return fmt.Errorf("disabling SQLite memory statistics: sqlite3_config returned %d", rc)
	}
}
