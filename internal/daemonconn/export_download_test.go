package daemonconn_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func nativeArchive(t *testing.T) ([]byte, bundle.Receipt) {
	t.Helper()
	body := []byte("synthetic original\n")
	h := sha256.Sum256(body)
	d := bundle.Document{NodeID: 2, VersionID: "c0a7ac5a-a410-44ee-9ad1-17200487e37b", SHA256: hex.EncodeToString(h[:]), Size: int64(len(body)), Name: "synthetic.txt", Path: "/synthetic.txt", MediaType: "text/plain"}
	d.Roles = []bundle.Role{{Role: "original", Status: "available", Path: "documents/2/" + d.VersionID + "/original", SHA256: d.SHA256, Size: d.Size}}
	plan := bundle.Plan{Format: bundle.Format, ID: "0d845f53-bacc-48ae-ac2e-0cefd5e2958c", VaultID: "620e0094-c014-45d3-8974-851476415a29", Toolchain: "synthetic", Total: 1, RoleEntries: 1, RoleBytes: d.Size, Roles: []bundle.RolePolicy{{Role: "original"}}}
	walk := func(visit func(bundle.Document) error) error { return visit(d) }
	var err error
	plan.Fingerprint, err = bundle.Fingerprint(plan, walk)
	require.NoError(t, err)
	f, err := os.Create(filepath.Join(t.TempDir(), "bundle.zip"))
	require.NoError(t, err)
	open := func(bundle.Role) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	receipt, err := bundle.Write(t.Context(), f, plan, walk, open, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	raw, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	return raw, receipt
}

func TestNativeExportDownloadVerifiesArchiveAndReceipts(t *testing.T) {
	raw, receipt := nativeArchive(t)
	for _, name := range []string{"valid", "wrong fingerprint", "ticket receipt", "truncated", "extra bytes", "corrupt", "invalid URL", "wrong job", "unfinished", "closed file", "canceled", "transport"} {
		t.Run(name, func(t *testing.T) {
			job := bundle.Job{ID: "c0a7ac5a-a410-44ee-9ad1-17200487e37b", State: "completed", Fingerprint: receipt.PlanFingerprint, Receipt: new(receipt)}
			ticket := apiclient.TicketOutputBody{URL: "/api/daemon/web-download/file?ticket=synthetic-ticket-secret", Receipt: receipt}
			body := bytes.Clone(raw)
			switch name {
			case "wrong fingerprint":
				job.Fingerprint = strings.Repeat("a", 64)
				job.Receipt.PlanFingerprint = job.Fingerprint
				ticket.Receipt = *job.Receipt
			case "ticket receipt":
				ticket.Receipt.Entries++
			case "truncated":
				body = body[:len(body)-1]
			case "extra bytes":
				body = append(body, 0)
			case "corrupt":
				body[bytes.Index(body, []byte("synthetic original"))] ^= 1
			case "invalid URL":
				ticket.URL = "https://example.com/?ticket=synthetic-ticket-secret"
			case "wrong job":
				job.ID = "another-job"
			case "unfinished":
				job.State = "running"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/download"):
					assert.NoError(t, json.MarshalWrite(w, ticket))
				case r.URL.Path == "/api/daemon/web-download/file":
					if name == "canceled" {
						cancel()
						<-r.Context().Done()
						return
					}
					if name == "transport" {
						h, ok := w.(http.Hijacker)
						if !assert.True(t, ok) {
							return
						}
						c, _, err := h.Hijack()
						if assert.NoError(t, err) {
							_ = c.Close()
						}
						return
					}
					_, _ = w.Write(body)
				default:
					assert.NoError(t, json.MarshalWrite(w, job))
				}
			}))
			defer server.Close()
			f, err := os.Create(filepath.Join(t.TempDir(), "stage"))
			require.NoError(t, err)
			defer func() { _ = f.Close() }()
			if name == "closed file" {
				require.NoError(t, f.Close())
			}
			got, err := daemonconn.New(server.URL, "synthetic-key").DownloadExportArchiveTo(ctx, "c0a7ac5a-a410-44ee-9ad1-17200487e37b", f)
			switch name {
			case "valid":
				require.NoError(t, err)
				require.Equal(t, receipt, got)
				stored, err := os.ReadFile(f.Name())
				require.NoError(t, err)
				require.Equal(t, raw, stored)
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
			case "closed file":
				require.ErrorIs(t, err, os.ErrClosed)
			case "unfinished", "transport":
				require.Error(t, err)
				require.NotErrorIs(t, err, daemonconn.ErrIntegrity)
			default:
				require.ErrorIs(t, err, daemonconn.ErrIntegrity)
			}
			if err != nil {
				require.NotContains(t, err.Error(), "synthetic-ticket-secret")
			}
		})
	}
}
