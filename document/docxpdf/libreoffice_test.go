package docxpdf

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthoredSamplesCarryNoPersonalMetadata(t *testing.T) {
	for _, name := range []string{"word-page-breaks.docx", "libreoffice-page-breaks.docx"} {
		t.Run(name, func(t *testing.T) {
			archive, err := zip.OpenReader(filepath.Join("testdata", name))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, archive.Close()) })
			core := zipEntry(t, archive, "docProps/core.xml")
			var properties struct {
				Creator        string `xml:"creator"`
				LastModifiedBy string `xml:"lastModifiedBy"`
			}
			require.NoError(t, xml.Unmarshal(core, &properties))
			require.Empty(t, properties.Creator)
			require.Empty(t, properties.LastModifiedBy)
			app := zipEntry(t, archive, "docProps/app.xml")
			var application struct {
				Template string `xml:"Template"`
				Company  string `xml:"Company"`
			}
			require.NoError(t, xml.Unmarshal(app, &application))
			require.Empty(t, application.Template)
			require.Empty(t, application.Company)
			if name == "word-page-breaks.docx" {
				require.Contains(t, string(app), "<Pages>1</Pages>")
			}
		})
	}
}

func TestLibreOfficeRendersAuthoredSamples(t *testing.T) {
	policy := libreOfficePolicy(t)
	for _, name := range []string{"word-page-breaks.docx", "libreoffice-page-breaks.docx"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name))
			require.NoError(t, err)
			source, _ := sourceFor(t, data)
			result, err := Convert(t.Context(), source, policy)
			require.NoError(t, err)
			require.Equal(t, 11, result.Receipt().Pages)
			t.Logf("%s pages=%d pdf_bytes=%d", name, result.Receipt().Pages, result.Receipt().PDFBytes)
		})
	}
}

func TestLibreOfficeCancellationRemovesWork(t *testing.T) {
	policy := libreOfficePolicy(t)
	before := conversionDirectories(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	data, err := os.ReadFile(filepath.Join("testdata", "word-page-breaks.docx"))
	require.NoError(t, err)
	source, _ := sourceFor(t, data)
	done := make(chan conversionResult, 1)
	go func() {
		result, err := Convert(ctx, source, policy)
		done <- conversionResult{result: result, err: err}
	}()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if conversionRendererStarted(t) {
			t.Log("LibreOffice profile appeared before cancellation")
			cancel()
			break
		}
		select {
		case result := <-done:
			t.Fatalf("LibreOffice conversion ended before cancellation: %v", result.err)
		case <-deadline.C:
			t.Fatal("LibreOffice conversion work directory did not appear")
		case <-ticker.C:
		}
	}
	result := <-done
	require.Nil(t, result.result)
	require.ErrorIs(t, result.err, context.Canceled)
	require.Equal(t, before, conversionDirectories(t))
}

func conversionRendererStarted(t *testing.T) bool {
	t.Helper()
	for _, directory := range conversionDirectories(t) {
		entries, err := os.ReadDir(filepath.Join(os.TempDir(), directory, "profile"))
		if err == nil && len(entries) > 0 {
			return true
		}
	}
	return false
}

func libreOfficePolicy(t *testing.T) Policy {
	t.Helper()
	executable := os.Getenv("DOCBANK_DOCXPDF_SOFFICE")
	if executable == "" {
		t.Skip("DOCBANK_DOCXPDF_SOFFICE is not set")
	}
	sha := os.Getenv("DOCBANK_DOCXPDF_SOFFICE_SHA256")
	if sha == "" {
		t.Skip("DOCBANK_DOCXPDF_SOFFICE_SHA256 is not set")
	}
	limits := DefaultLimits()
	policy, err := NewPolicy(Renderer{
		Executable: executable, ExecutableSHA256: sha,
		RuntimeIdentity: "LibreOffice operator install",
	}, limits)
	require.NoError(t, err)
	return policy
}

func zipEntry(t *testing.T, archive *zip.ReadCloser, name string) []byte {
	t.Helper()
	for _, entry := range archive.File {
		if entry.Name != name {
			continue
		}
		reader, err := entry.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		return data
	}
	t.Fatalf("missing DOCX entry %q", name)
	return nil
}
