package push_test

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/push"
	"go.kenn.io/docbank/internal/store"
)

// BenchmarkPushFolder includes hashing, authenticated HTTP, durable publication
// and metadata. Each iteration owns a fresh vault; fixture setup and teardown
// are excluded. Use disk-backed TMPDIR to measure durable writes.
func BenchmarkPushFolder(b *testing.B) {
	source := b.TempDir()
	random := rand.NewChaCha8([32]byte{1})
	content := make([]byte, 4096)
	for i := range 128 {
		_, err := random.Read(content)
		require.NoError(b, err)
		require.NoError(b, os.WriteFile(filepath.Join(source,
			fmt.Sprintf("file-%04d.bin", i)), content, 0o600))
	}
	for _, resume := range []bool{false, true} {
		b.Run(fmt.Sprintf("resume=%t", resume), func(b *testing.B) {
			b.SetBytes(128 * 4096)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				func() {
					vault := b.TempDir()
					defer func() { require.NoError(b, os.RemoveAll(vault)) }()
					s, err := store.Open(filepath.Join(vault, "vault.db"))
					require.NoError(b, err)
					defer func() { require.NoError(b, s.Close()) }()
					blobs, err := blob.NewWithOptions(store.NewPackCatalog(s),
						filepath.Join(vault, "blobs"), blob.ManagedOptions())
					require.NoError(b, err)
					defer func() { require.NoError(b, blobs.Close()) }()
					cfg := config.Default()
					cfg.Server.APIKey = "synthetic-key"
					server := api.NewServer(api.Deps{
						Store: s, Blobs: blobs, VaultRoot: vault, Cfg: cfg,
						Logger: slog.New(slog.DiscardHandler),
					})
					defer server.Close()
					httpServer := httptest.NewServer(server.Handler())
					defer httpServer.Close()
					connection, err := daemonconn.NewPushConnection(httpServer.URL, "synthetic-key")
					require.NoError(b, err)
					defer func() { require.NoError(b, connection.Close()) }()
					if resume {
						report, err := push.Run(b.Context(), connection, options(source))
						require.NoError(b, err)
						require.Equal(b, push.Report{Added: 128}, report)
					}
					b.StartTimer()
					report, err := push.Run(b.Context(), connection, options(source))
					b.StopTimer()
					require.NoError(b, err)
					if resume {
						require.Equal(b, push.Report{Skipped: 128}, report)
					} else {
						require.Equal(b, push.Report{Added: 128}, report)
					}
				}()
				b.StartTimer()
			}
		})
	}
}
