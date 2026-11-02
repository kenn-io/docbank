//go:build linux || darwin

package embedserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestORTRejectsUnavailableAPI23(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler required for the runtime fixture")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "runtime.c")
	library := filepath.Join(dir, "runtime.so")
	require.NoError(t, os.WriteFile(source, []byte(`
#include <stddef.h>
#include <stdint.h>

static const void *get_api(uint32_t version) {
    if (version != 23) __builtin_trap();
    return NULL;
}

static const char *get_version(void) { return "1.24.0"; }

static const struct {
    const void *(*get_api)(uint32_t);
    const char *(*get_version)(void);
} base = {get_api, get_version};

const void *OrtGetApiBase(void) { return &base; }
`), 0o600))
	output, err := exec.CommandContext(t.Context(), compiler, "-shared", "-fPIC", "-o", library, source).CombinedOutput()
	require.NoError(t, err, "%s", output)
	_, err = openORT(library, "unused.onnx")
	require.EqualError(t, err, "ONNX Runtime API 23 unavailable")
}
