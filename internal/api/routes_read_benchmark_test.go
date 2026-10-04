package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
)

// BenchmarkContentDownload includes the real HTTP handler and verified blob
// reader. Fixture creation and packing are outside the measured interval.
func BenchmarkContentDownload(b *testing.B) {
	for _, size := range []int{1 << 10, 8 << 20} {
		for _, packed := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/packed=%t", size, packed), func(b *testing.B) {
				ts, s := newTestServer(b, func(d *api.Deps) {
					d.Logger = slog.New(slog.DiscardHandler)
				})
				block := make([]byte, 1024)
				_, err := rand.NewChaCha8([32]byte{1}).Read(block)
				require.NoError(b, err)
				content := bytes.Repeat(block, size/len(block))
				sum := sha256.Sum256(content)
				wantDigest := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
				hash, storedSize, err := s.Blobs.Write(bytes.NewReader(content))
				require.NoError(b, err)
				node, err := s.CreateFile(b.Context(), s.RootID(), "content.bin",
					hash, storedSize, "application/octet-stream")
				require.NoError(b, err)
				if packed {
					result, err := s.Blobs.Maintainer().Pack(b.Context(), packstore.PackOptions{})
					require.NoError(b, err)
					require.Equal(b, 1, result.BlobsPacked)
				}
				url := fmt.Sprintf("%s/api/v1/nodes/%d/content", ts.URL, node.ID)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for b.Loop() {
					response, err := ts.Client().Get(url)
					require.NoError(b, err)
					written, err := io.Copy(io.Discard, response.Body)
					closeErr := response.Body.Close()
					require.NoError(b, err)
					require.NoError(b, closeErr)
					require.Equal(b, http.StatusOK, response.StatusCode)
					require.Equal(b, int64(size), written)
					require.Equal(b, wantDigest, response.Trailer.Get("Content-Digest"))
				}
			})
		}
	}
}
