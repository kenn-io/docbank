package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
)

// BenchmarkResolvePath includes loopback HTTP, node metadata, and response decoding.
func BenchmarkResolvePath(b *testing.B) {
	for _, depth := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			ts, s := newTestServer(b, func(d *api.Deps) {
				d.Logger = slog.New(slog.DiscardHandler)
			})
			parentPath := strings.Repeat("/folder", depth-1)
			parent, err := s.MkdirAll(b.Context(), parentPath)
			require.NoError(b, err)
			node, err := s.CreateFile(b.Context(), parent.ID, "file.pdf", testHash("content"), 17, "application/pdf")
			require.NoError(b, err)
			canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
				ContractVersion: document.SourceMetadataContractV1,
			})
			require.NoError(b, err)
			_, err = s.PublishSourceMetadata(b.Context(), node.BlobHash, testHash("extractor"), canonical)
			require.NoError(b, err)
			path := parentPath + "/file.pdf"
			endpoint := ts.URL + "/api/v1/path?path=" + url.QueryEscape(path)
			b.ReportAllocs()
			for b.Loop() {
				response, err := ts.Client().Get(endpoint)
				require.NoError(b, err)
				var got api.Node
				decodeErr := json.UnmarshalRead(response.Body, &got)
				closeErr := response.Body.Close()
				require.NoError(b, decodeErr)
				require.NoError(b, closeErr)
				require.Equal(b, http.StatusOK, response.StatusCode)
				require.Equal(b, node.ID, got.ID)
				require.Equal(b, path, got.Path)
				require.Equal(b, node.CurrentVersionID, got.CurrentVersionID)
				require.NotNil(b, got.SourceMetadata)
			}
		})
	}
}

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

// Includes the authenticated HTTP handler, complete previews and JSON decoding.
func BenchmarkDuplicatePageHTTP(b *testing.B) {
	for _, depth := range []int{0, 16} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			ts, s := newTestServer(b, func(d *api.Deps) { d.Logger = slog.New(slog.DiscardHandler) })
			ctx := b.Context()
			prefix := strings.Repeat("/folder", depth)
			parent, err := s.MkdirAll(ctx, prefix)
			require.NoError(b, err)
			run, err := s.BeginIngest(ctx, "cli", "Synthetic duplicate page")
			require.NoError(b, err)
			for group := range 50 {
				for copy := range 16 {
					name := fmt.Sprintf("group-%03d-copy-%02d.bin", group, copy)
					_, err := s.IngestFileExact(ctx, run, parent.ID, name, fmt.Sprintf("%064x", group+1),
						1024, "application/octet-stream", "/synthetic/"+name, "")
					require.NoError(b, err)
				}
			}
			endpoint := ts.URL + "/api/v1/duplicates?limit=50"
			b.ReportAllocs()
			for b.Loop() {
				response, err := ts.Client().Get(endpoint)
				require.NoError(b, err)
				var page api.DuplicatePage
				decodeErr := json.UnmarshalRead(response.Body, &page)
				closeErr := response.Body.Close()
				require.NoError(b, decodeErr)
				require.NoError(b, closeErr)
				require.Equal(b, http.StatusOK, response.StatusCode)
				require.Equal(b, 50, page.Total)
				require.Equal(b, 800, page.TotalReferences)
				require.Len(b, page.Items, 50)
				for i, group := range page.Items {
					require.Len(b, group.References, 16)
					for j, ref := range group.References {
						require.Equal(b, prefix+fmt.Sprintf("/group-%03d-copy-%02d.bin", i, j), ref.Reference.Path)
						require.Equal(b, 1, ref.CollectionCount)
						require.Len(b, ref.Collections, 1)
						require.Equal(b, run.ID(), ref.Collections[0].ID)
					}
				}
			}
		})
	}
}

// Include HTTP decoding and compare selective and broad queries in the same scope.
func BenchmarkScopedSearchHTTP(b *testing.B) {
	for _, files := range []int{100, 10000} {
		for _, depth := range []int{0, 16} {
			b.Run(fmt.Sprintf("files=%d/depth=%d", files, depth), func(b *testing.B) {
				ts, s := newTestServer(b, func(d *api.Deps) { d.Logger = slog.New(slog.DiscardHandler) })
				ctx := b.Context()
				scope, err := s.MkdirAll(ctx, "/scope")
				require.NoError(b, err)
				prefix := "/scope" + strings.Repeat("/folder", depth)
				parent, err := s.MkdirAll(ctx, prefix)
				require.NoError(b, err)
				for i := range files {
					name := fmt.Sprintf("report-%05d.bin", i)
					if i < 5 {
						name = "needle-" + name
					}
					_, err := s.CreateFile(ctx, parent.ID, name, fmt.Sprintf("%064x", i+1),
						1024, "application/octet-stream")
					require.NoError(b, err)
				}
				_, err = s.CreateFile(ctx, s.RootID(), "needle-report-outside.bin",
					fmt.Sprintf("%064x", files+1), 1024, "application/octet-stream")
				require.NoError(b, err)
				for _, query := range []string{"needle", "report"} {
					b.Run(query, func(b *testing.B) {
						endpoint := fmt.Sprintf("%s/api/v1/search?q=%s&under_node_id=%d&limit=50", ts.URL, query, scope.ID)
						count, start, namePrefix := 50, 5, ""
						if query == "needle" {
							count, start, namePrefix = 5, 0, "needle-"
						}
						b.ReportAllocs()
						for b.Loop() {
							response, err := ts.Client().Get(endpoint)
							require.NoError(b, err)
							var report api.SearchReport
							decodeErr := json.UnmarshalRead(response.Body, &report)
							closeErr := response.Body.Close()
							require.NoError(b, decodeErr)
							require.NoError(b, closeErr)
							require.Equal(b, http.StatusOK, response.StatusCode)
							require.Equal(b, query == "report", report.Truncated)
							require.Len(b, report.Hits, count)
							for i, hit := range report.Hits {
								require.Equal(b, prefix+"/"+namePrefix+fmt.Sprintf("report-%05d.bin", start+i), hit.Path)
							}
						}
					})
				}
			})
		}
	}
}
