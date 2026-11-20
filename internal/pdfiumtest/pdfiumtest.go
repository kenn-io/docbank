// Package pdfiumtest lets the test processes of one run share compiled PDFium
// code.
package pdfiumtest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"github.com/tetratelabs/wazero"

	"go.kenn.io/docbank/internal/pdfproduction"
)

// codeDirEnv names the directory where the test processes of one run share
// compiled PDFium code. scripts/with-test-tmpdir.sh creates it for each run.
const codeDirEnv = "DOCBANK_TEST_WASM_CACHE_DIR"

// ShareCompiledCode makes this test process compile PDFium through the run's
// shared directory, when one is named. Compiling the module takes seconds of
// CPU, and a test run starts dozens of processes that need it. The first
// process compiles it under a file lock while the others wait, and they then
// load the compiled code. Call it from TestMain before any test runs.
func ShareCompiledCode() {
	dir := os.Getenv(codeDirEnv)
	if dir == "" {
		return
	}
	cache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		panic(fmt.Sprintf("open the shared PDFium code directory named by %s: %v", codeDirEnv, err))
	}
	pdfproduction.ShareCompiledCode(cache, flock.New(filepath.Join(dir, "compile.lock")))
}
