package emailpdf

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
)

func TestVerifyPDFRejectsEmptyTruncatedAndExcessivePages(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("%PDF-1.7\n%%EOF"), []byte("not a PDF")} {
		_, err := VerifyPDF(data)
		require.Error(t, err)
	}
	p := fpdf.New("P", "mm", "A4", "")
	p.AddPage()
	p.SetFont("Helvetica", "", 12)
	p.Text(20, 20, "Synthetic independent parser fixture")
	var b bytes.Buffer
	require.NoError(t, p.Output(&b))
	pages, err := VerifyPDF(b.Bytes())
	require.NoError(t, err)
	require.Equal(t, int64(1), pages)
	_, err = VerifyPDF(b.Bytes()[:b.Len()-20])
	require.Error(t, err)
}

func TestRuntimePinRefusesMissingAndUnpinnedChromium(t *testing.T) {
	_, err := NewRuntime(RuntimeConfig{})
	require.Error(t, err)
	_, err = NewRuntime(RuntimeConfig{Chromium: "/missing/chromium", Version: "151.0.7922.34"})
	require.Error(t, err)
}

func TestOperatorPinScriptMatchesRuntimeTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("operator pin shell helper targets the Linux renderer")
	}
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "nested fonts"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested fonts", "synthetic.ttf"), []byte("synthetic runtime inventory"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "another"), []byte("another synthetic package file"), 0600))
	want, err := TreeSHA256(root)
	require.NoError(t, err)
	got, err := exec.CommandContext(t.Context(), "bash", filepath.Join("..", "..", "scripts", "email-pdf-pins.sh"), root).Output()
	require.NoError(t, err)
	require.Equal(t, want, strings.TrimSpace(string(got)))
}
