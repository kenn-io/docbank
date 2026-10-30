package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestRecipientSelection(t *testing.T) {
	t.Parallel()
	const document = `{"allocation_id":"11111111-1111-4111-8111-111111111111","preparation":"prep.json","masks":"masks.json","review":"review.json"}`
	valid := `{"contract":"native-pdf-recipient-selection/v1","documents":[` + document + `]}`
	for name, data := range map[string]string{
		"valid":                valid,
		"unknown":              strings.Replace(valid, `"contract":`, `"extra":1,"contract":`, 1),
		"duplicate key":        strings.Replace(valid, `"contract":`, `"contract":"x","contract":`, 1),
		"null":                 strings.Replace(valid, `"prep.json"`, `null`, 1),
		"empty":                strings.Replace(valid, document, "", 1),
		"duplicate allocation": strings.Replace(valid, document, document+","+document, 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "selection.json")
			require.NoError(t, os.WriteFile(path, []byte(data), 0600))
			selection, err := readRecipientSelection(path)
			if name != "valid" {
				require.Error(t, err)
				require.Equal(t, 2, commandExitCode(err, false))
				return
			}
			require.NoError(t, err)
			require.Equal(t, filepath.Join(filepath.Dir(path), "prep.json"), selection.Documents[0].Preparation)
		})
	}
}

func TestRecipientPackageRequiresFullPDFVerification(t *testing.T) {
	if runRedactionVerificationInSubprocess(t) {
		return
	}
	f := newRedactionVerifyFixture(t)
	recipe, err := processing.BuildNumberedRecipe(uuid.New(), store.BatesNamespace{NamespaceID: uuid.New().String(), Prefix: "Q", Padding: 6}, 1, f.review)
	require.NoError(t, err)
	presentation, err := recipe.Presentation()
	require.NoError(t, err)
	for _, scenario := range []string{"valid", "wrong visible number", "unsupported verifier"} {
		t.Run(scenario, func(t *testing.T) {
			pdf := redactionVerifyPDF(t, f.review, presentation, scenario)
			recorded := recipe
			if scenario == "unsupported verifier" {
				recorded.Renderer.WriterVersion = "future-writer"
			}
			receipt := numberedDownloadReceipt(t, recorded, pdf)
			var contentRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/content") {
					contentRequests.Add(1)
					_, _ = w.Write(pdf)
				} else {
					_ = json.MarshalWrite(w, receipt)
				}
			}))
			defer server.Close()
			client := daemonconn.New(server.URL, "synthetic-key")
			defer func() { _ = client.Close() }()
			selection := recipientSelection{Contract: "native-pdf-recipient-selection/v1", Documents: []recipientInput{{receipt.AllocationID, f.args[2], f.args[3], f.args[4]}}}
			checked, err := preflightRecipientPackage(t.Context(), client, selection)
			if scenario == "unsupported verifier" {
				require.Equal(t, exitGeneral, commandExitCode(recipientCLIError(err), true))
				require.ErrorIs(t, err, pdfproduction.ErrVerifierVersion)
				require.Zero(t, contentRequests.Load())
				return
			}
			require.NoError(t, err)
			require.Equal(t, processing.RecipientPackageFile{
				Path: "VOL001/TEXT/000001.txt", Size: 23, SHA256: redactionPDFHash([]byte("[REDACTED] KEEP SECRET\f")),
			}, checked.Documents[0].Text)
			destination := filepath.Join(t.TempDir(), "recipient.zip")
			cmd := newRedactionPackageCommand()
			cmd.SetOut(io.Discard)
			err = publishRecipientPackage(t.Context(), cmd, destination, true, client, checked)
			require.EqualValues(t, 1, contentRequests.Load())
			if scenario == "valid" {
				require.NoError(t, err)
				require.FileExists(t, destination)
			} else {
				require.Equal(t, exitIntegrity, commandExitCode(recipientCLIError(err), true))
				require.NoFileExists(t, destination)
				entries, err := os.ReadDir(filepath.Dir(destination))
				require.NoError(t, err)
				require.Empty(t, entries)
			}
		})
	}
}
