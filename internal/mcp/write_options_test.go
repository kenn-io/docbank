package mcp

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestWriteOptionsSeparateProcessingFromPackageMutations(t *testing.T) {
	for _, test := range []struct {
		name             string
		options          ServerOptions
		wantPackageWrite bool
	}{
		{name: "read only"},
		{name: "processing only", options: ServerOptions{AllowProcessing: true}},
		{name: "package writes only", options: ServerOptions{AllowPackageWrites: true}, wantPackageWrite: true},
		{name: "both", options: ServerOptions{AllowProcessing: true, AllowPackageWrites: true}, wantPackageWrite: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			vault := t.TempDir()
			catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, catalog.Close()) })
			blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, blobs.Close()) })
			cfg := config.Default()
			cfg.Server.APIKey = "synthetic-write-options-key"
			daemon := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
			t.Cleanup(daemon.Close)
			httpServer := httptest.NewServer(daemon.Handler())
			t.Cleanup(httpServer.Close)
			connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
			t.Cleanup(func() { require.NoError(t, connection.Close()) })
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return connection, nil
			}, func(*daemonconn.Connection) error { return nil })

			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "package.dat"),
				[]byte("þDOCIDþ\x14þNATIVEþ\r\nþDOC-Aþ\x14þA.txtþ\r\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "A.txt"), []byte("synthetic native"), 0o600))
			preflight, err := connection.API().CreatePackagePreflight(t.Context(), &apiclient.CreatePackagePreflightRequestOptions{
				Body: &api.PackagePreflightRequest{SourceKind: "root", SourceRef: root, Profile: "dat-concordance-v1", Encoding: "utf-8"},
			})
			require.NoError(t, err)
			job, err := connection.API().CreatePackageImport(t.Context(), &apiclient.CreatePackageImportRequestOptions{
				Body: &api.PackageImportRequest{PreflightID: preflight.PreflightID, Into: "/", Name: "synthetic-package", OperationID: uuid.New().String()},
			})
			require.NoError(t, err)

			server := newServerWithOptionsAndDaemon(testImplementation(), test.options, lease)
			response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "assign_package_custodian", "arguments": map[string]any{
					"package_id": job.PackageID, "raw_label": "Synthetic MCP custodian", "if_match_revision": 1,
				},
			}))
			assignments, _, err := catalog.Custodians(t.Context(), store.CustodianScope{Kind: "package", PackageID: job.PackageID}, false, 10, 0)
			require.NoError(t, err)
			if test.wantPackageWrite {
				result := decodeResult(t, response)
				require.NotEqual(t, true, result["isError"])
				require.Len(t, assignments, 1)
				assert.Equal(t, "Synthetic MCP custodian", assignments[0].RawLabel)
				assert.Equal(t, "operator_assigned", assignments[0].Basis)
			} else {
				assert.Empty(t, assignments, "package writes must require their own opt-in")
				assert.NotZero(t, decodeWireError(t, response).Code)
			}

			bates := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "ensure_bates_namespace", "arguments": map[string]any{"prefix": "CASE", "padding": 6},
			}))
			namespaces, _, _, err := catalog.BatesNamespaces(t.Context(), "", 10)
			require.NoError(t, err)
			if test.wantPackageWrite {
				require.NotEqual(t, true, decodeResult(t, bates)["isError"])
				require.Len(t, namespaces, 1)
				assert.Equal(t, "CASE", namespaces[0].Prefix)
			} else {
				assert.Empty(t, namespaces, "Bates writes must require the package-write opt-in")
				assert.NotZero(t, decodeWireError(t, bates).Code)
			}

			processing := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "start_processing", "arguments": processingToolArguments(testProcessingPlanFingerprint),
			}))
			if test.options.AllowProcessing {
				result := decodeResult(t, processing)
				assert.Equal(t, "plan_changed", objectField(t, result, "structuredContent")["code"])
			} else {
				assert.NotZero(t, decodeWireError(t, processing).Code)
			}
		})
	}
}
