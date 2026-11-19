//go:build cgo

package mattn

/*
// mattn/go-sqlite3 compiles SQLite into this binary but does not expose
// sqlite3_config, so declare the public entry point here.
int sqlite3_config(int, ...);

static void docbank_disable_sqlite_memstatus(void) { sqlite3_config(9, 0); }

// SQLITE_CONFIG_MEMSTATUS is 9 in sqlite3.h. Turning it off must happen
// before SQLite initializes.
*/
import "C"

// mattn/go-sqlite3 builds SQLite with memory statistics on. They make every
// SQLite allocation in the process take one global mutex, so concurrent
// connections to separate databases serialize on it. Docbank reads no SQLite
// memory statistics or heap limits. If an embedding application already
// initialized SQLite, sqlite3_config leaves its settings and connections alone.
func disableMemoryStatus() {
	C.docbank_disable_sqlite_memstatus()
}
