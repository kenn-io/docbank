package mediatest_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestEncryptedPDFFixturesWithoutUserConfig(t *testing.T) {
	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", root)
	case "darwin":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}
	configDir, err := os.UserConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	// A file at the config-directory path makes accidental host config access fail.
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "pdfcpu"), []byte("synthetic config blocker"), 0o600))
	for _, bits := range []int{40, 128, 256} {
		data, err := mediatest.EncryptedPDF(t.Context(), "open-secret", "owner-secret", bits)
		require.NoError(t, err)
		configuration := model.NewStatelessConfiguration()
		configuration.UserPW = "open-secret"
		require.NoError(t, api.Validate(t.Context(), bytes.NewReader(data), configuration, nil))
	}
}
