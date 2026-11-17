package docbank_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/docbank"
)

const pdfConfigChildEnv = "DOCBANK_TEST_PDF_CONFIG_CHILD"

// pdfcpu creates config.yml under os.UserConfigDir the first time a process
// asks it for a default configuration, and panics if that write fails. Run the
// preflight in a fresh process whose config directory is empty so an earlier
// test in this process cannot have loaded pdfcpu's defaults already.
func TestPackagePreflightCountsPDFPagesWithoutPDFConfigDirectory(t *testing.T) {
	if os.Getenv(pdfConfigChildEnv) == "1" {
		preflightPDFNatives(t)
		return
	}
	t.Parallel()

	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), os.Args[0], //nolint:gosec // os.Args[0] is the trusted current test executable.
		"-test.run=^TestPackagePreflightCountsPDFPagesWithoutPDFConfigDirectory$", "-test.count=1")
	// os.UserConfigDir reads XDG_CONFIG_HOME or HOME on Unix and AppData on Windows.
	cmd.Env = append(os.Environ(), pdfConfigChildEnv+"=1", "HOME="+home, "XDG_CONFIG_HOME="+home, "APPDATA="+home)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	var created []string
	require.NoError(t, filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == "pdfcpu" {
			created = append(created, path)
		}
		return nil
	}))
	assert.Empty(t, created, "package preflight wrote pdfcpu configuration into the user's config directory")
}

func preflightPDFNatives(t *testing.T) {
	t.Helper()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	root := t.TempDir()
	natives := filepath.Join(root, "VOL001", "NATIVES")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001", "DATA"), 0o700))
	require.NoError(t, os.MkdirAll(natives, 0o700))
	valid, err := os.ReadFile("../document/testdata/scanassessment/blank.pdf")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(natives, "DOC-A.pdf"), valid, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(natives, "DOC-B.pdf"), []byte("synthetic text, not a PDF\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "load.dat"), []byte(
		"þDOCIDþ\x14þNATIVEþ\r\n"+
			"þDOC-Aþ\x14þNATIVES/DOC-A.pdfþ\r\n"+
			"þDOC-Bþ\x14þNATIVES/DOC-B.pdfþ\r\n"), 0o600))
	mapping := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DOCID","source_ordinal":0,"canonical":"loadfile.document.id"},{"source":"NATIVE","source_ordinal":1,"canonical":"loadfile.file.native"}]}`)

	preflight, err := vault.PreflightPackage(t.Context(), docbank.PackagePreflightRequest{
		SourceKind: "root", SourceRef: root, Profile: "dat-concordance-v1", Encoding: "utf-8", Mapping: mapping,
	})
	require.NoError(t, err)
	// Only DOC-B, whose native is not a PDF, fails page counting. The header is row 1.
	require.Len(t, preflight.Diagnostics, 1)
	assert.Equal(t, "page_count_unavailable", preflight.Diagnostics[0].Code)
	assert.Equal(t, 3, preflight.Diagnostics[0].RowOrdinal)
}
