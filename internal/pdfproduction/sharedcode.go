package pdfproduction

import (
	"fmt"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
)

// Exactly one embedded module is compiled. Keep its code-only cache for the
// process lifetime as well: retiring a large compilation for every engine can
// retain heap-system pages even after all runtime/document handles are closed.
// The exclusive backing lease serializes compilation and runtime access.
var wasmCode = wazero.NewCompilationCache()

// CodeLock is a lock that several processes can hold, such as a file lock.
type CodeLock interface {
	Lock() error
	Unlock() error
}

// wasmCodeLock, when set, is held while this process compiles or loads
// PDFium, so processes that share wasmCode's directory compile it once.
var wasmCodeLock CodeLock

// wasmCodeWarm reports that this process holds the compiled module in memory,
// so later initializations need neither the lock nor the directory.
var wasmCodeWarm atomic.Bool

// ShareCompiledCode makes engines created later compile PDFium through cache
// and hold lock while they compile or load it. Test mains call it through
// internal/pdfiumtest before any engine starts, so the test processes of one
// run compile PDFium once.
func ShareCompiledCode(cache wazero.CompilationCache, lock CodeLock) {
	wasmCode, wasmCodeLock = cache, lock
}

// lockSharedCode holds the shared code lock while this process compiles or
// loads PDFium. The returned function releases it and records whether the
// module is now in memory.
func lockSharedCode() (func(loaded bool) error, error) {
	if wasmCodeLock == nil || wasmCodeWarm.Load() {
		return func(bool) error { return nil }, nil
	}
	// A blocking lock, rather than polling, leaves the clock alone in a
	// synctest bubble, where a page deadline also covers initialization.
	if err := wasmCodeLock.Lock(); err != nil {
		return nil, fmt.Errorf("lock shared PDFium code: %w", err)
	}
	return func(loaded bool) error {
		if loaded {
			wasmCodeWarm.Store(true)
		}
		if err := wasmCodeLock.Unlock(); err != nil {
			return fmt.Errorf("unlock shared PDFium code: %w", err)
		}
		return nil
	}, nil
}
