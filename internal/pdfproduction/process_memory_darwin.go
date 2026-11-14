package pdfproduction

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// rusageInfoV2 mirrors the 160-byte struct rusage_info_v2 from
// <sys/resource.h>; only ri_phys_footprint is read.
type rusageInfoV2 struct {
	_             [72]byte // ri_uuid through ri_resident_size
	physFootprint uint64
	_             [80]byte // ri_proc_start_abstime through ri_diskio_byteswritten
}

const rusageInfoV2Flavor = 2

var procPidRusage = sync.OnceValues(func() (uintptr, error) {
	libSystem, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, fmt.Errorf("open libSystem: %w", err)
	}
	fn, err := purego.Dlsym(libSystem, "proc_pid_rusage")
	if err != nil {
		return 0, fmt.Errorf("resolve proc_pid_rusage: %w", err)
	}
	return fn, nil
})

// readProcessRSS reports the physical footprint macOS charges the worker.
// Go returns freed heap with MADV_FREE_REUSABLE, and macOS keeps those pages
// in resident_size until memory pressure reclaims them, so resident_size never
// falls after debug.FreeOSMemory. The footprint drops as soon as the runtime
// returns pages, and still counts every dirty page the worker holds, including
// compressed pages, WASM linear memory and compiled code.
func readProcessRSS(context.Context) (uint64, error) {
	fn, err := procPidRusage()
	if err != nil {
		return 0, fmt.Errorf("open worker memory accounting: %w", err)
	}
	var usage rusageInfoV2
	ret, _, errno := purego.SyscallN(fn, uintptr(os.Getpid()), rusageInfoV2Flavor, uintptr(unsafe.Pointer(&usage))) //nolint:gosec // The buffer matches the C struct layout.

	status := int32(ret) //nolint:gosec // proc_pid_rusage returns a C int.
	if status != 0 {
		return 0, fmt.Errorf("read worker memory footprint: %w", syscall.Errno(errno))
	}
	return usage.physFootprint, nil
}
