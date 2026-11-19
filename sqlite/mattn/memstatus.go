//go:build cgo

package mattn

/*
// mattn/go-sqlite3 compiles SQLite into this binary but does not expose
// sqlite3_config, so declare the public entry point here.
int sqlite3_config(int, ...);

static int docbank_disable_sqlite_memstatus(void) { return sqlite3_config(9, 0); }

// SQLITE_CONFIG_MEMSTATUS is 9 in sqlite3.h. Turning it off must happen
// before SQLite initializes.
*/
import "C"

import "fmt"

// mattn/go-sqlite3 builds SQLite with memory statistics on. They make every
// SQLite allocation in the process take one global mutex, so concurrent
// connections to separate databases serialize on it. Docbank reads no SQLite
// memory statistics or heap limits. sqlite3_config only works before SQLite
// initializes, which happens on the first open; this package's init runs
// before any caller can open a connection through the adapter.
func init() {
	if rc := C.docbank_disable_sqlite_memstatus(); rc != 0 {
		panic(fmt.Sprintf("disable SQLite memory statistics: sqlite3_config returned %d", int(rc)))
	}
}
