package daemonconn_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

func TestTermReportWriteInvalidReply(t *testing.T) {
	for _, operation := range []string{"create", "revise"} {
		for _, reply := range []string{"invalid summary", "wrong parent", "known conflict"} {
			t.Run(operation+"/"+reply, func(t *testing.T) {
				id := strings.Repeat("a", 48)
				request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
					Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
						Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}}}
				summary := report.Summary{ID: strings.Repeat("b", 48), State: "needs_review", Terms: request.Terms,
					ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)}
				if reply == "invalid summary" {
					summary.ID = "invalid"
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Content-Type", "application/json")
					if reply == "known conflict" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"status":409,"title":"Conflict","code":"report_selection_changed"}`))
						return
					}
					_ = json.MarshalWrite(w, summary)
				}))
				defer server.Close()
				client := daemonconn.New(server.URL, "synthetic-key")
				var err error
				if operation == "create" {
					_, err = client.CreateTermReport(t.Context(), request)
				} else {
					_, err = client.ReviseTermReport(t.Context(), id, nil)
				}
				require.Equal(t, 1, calls)
				if reply == "known conflict" {
					facts, ok := daemonconn.ExtractProblemFacts(err)
					require.True(t, ok)
					require.Equal(t, "report_selection_changed", facts.Code)
				} else if reply == "wrong parent" && operation == "create" {
					require.NoError(t, err)
				} else {
					require.True(t, daemonconn.IsResponseDecodeError(err), "wrong classification: %v", err)
				}
			})
		}
	}
}

func TestTermReportDownloadBindsSummaryAndPacket(t *testing.T) {
	client, catalog := newClient(t, serverKey)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "alpha.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2020-01-01", End: "2100-01-01"}}}}
	summary, err := client.CreateTermReport(t.Context(), request)
	require.NoError(t, err)
	stream, err := client.OpenTermReport(t.Context(), summary.ID, "bundle")
	require.NoError(t, err)
	packet, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	request.Terms[0].Expression = "beta"
	other, err := client.CreateTermReport(t.Context(), request)
	require.NoError(t, err)
	otherStream, err := client.OpenTermReport(t.Context(), other.ID, "bundle")
	require.NoError(t, err)
	otherPacket, err := io.ReadAll(otherStream)
	require.NoError(t, err)
	require.NoError(t, otherStream.Close())
	for _, name := range []string{"valid", "different packet", "changed bytes", "short", "long",
		"needs review", "budget", "closed file", "read only file", "invalid packet"} {
		t.Run(name, func(t *testing.T) {
			advertised := summary
			body := bytes.Clone(packet)
			headerSize, headerDigest := summary.BundleBytes, summary.BundleSHA256
			switch name {
			case "different packet":
				body = otherPacket
				headerSize = other.BundleBytes
				headerDigest = other.BundleSHA256
			case "changed bytes":
				body[0] ^= 1
			case "short":
				body = body[:len(body)-1]
			case "long":
				body = append(body, 0)
				headerSize++
			case "needs review":
				advertised.State = "needs_review"
				advertised.Counts = nil
				advertised.CSVBytes = 0
				advertised.BundleBytes = 0
			case "invalid packet":
				body = []byte("not a ZIP")
				sum := sha256.Sum256(body)
				headerSize = int64(len(body))
				headerDigest = hex.EncodeToString(sum[:])
				advertised.BundleBytes = headerSize
				advertised.BundleSHA256 = headerDigest
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/bundle") {
					w.Header().Set("Content-Length", strconv.FormatInt(headerSize, 10))
					w.Header().Set("X-Docbank-Report-Sha256", headerDigest)
					_, _ = w.Write(body)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, advertised)
			}))
			defer server.Close()
			file, err := os.Create(filepath.Join(t.TempDir(), "stage.zip"))
			require.NoError(t, err)
			defer func() { _ = file.Close() }()
			if name == "closed file" {
				require.NoError(t, file.Close())
			}
			if name == "read only file" {
				require.NoError(t, file.Close())
				file, err = os.Open(file.Name())
				require.NoError(t, err)
			}
			budgetSize := int64(64 << 20)
			if name == "budget" {
				budgetSize = 1
			}
			budget := report.NewBudget(budgetSize)
			defer func() { _ = budget.Close() }()
			result, err := daemonconn.New(server.URL, "synthetic-key").DownloadTermReportTo(t.Context(), summary.ID, file, budget)
			require.Zero(t, budget.Used())
			switch name {
			case "valid":
				require.NoError(t, err)
				require.Equal(t, summary.BundleBytes, result.Size)
				require.Equal(t, summary.BundleSHA256, result.SHA256)
				require.True(t, result.Verification.InternallyConsistent)
				require.False(t, result.Verification.SourceVerified)
				saved, err := os.ReadFile(file.Name())
				require.NoError(t, err)
				require.Equal(t, packet, saved)
				archive, err := zip.NewReader(bytes.NewReader(saved), int64(len(saved)))
				require.NoError(t, err)
				members, err := archive.Open("members.jsonl")
				require.NoError(t, err)
				var member struct {
					Identity report.Identity `json:"identity"`
					Hits     []bool          `json:"hits"`
				}
				require.NoError(t, json.UnmarshalRead(members, &member))
				require.NoError(t, members.Close())
				require.Equal(t, node.ID, member.Identity.NodeID)
				// The name alpha.txt is searchable even without extracted body text.
				require.Equal(t, []bool{true}, member.Hits)
			case "needs review":
				require.ErrorIs(t, err, daemonconn.ErrReportReviewRequired)
			case "budget":
				require.ErrorIs(t, err, report.ErrBudgetExhausted)
			case "closed file":
				require.ErrorIs(t, err, os.ErrClosed)
			case "read only file":
				var fileErr *os.PathError
				require.ErrorAs(t, err, &fileErr)
				require.NotErrorIs(t, err, daemonconn.ErrIntegrity)
			default:
				require.ErrorIs(t, err, daemonconn.ErrIntegrity)
			}
		})
	}
}

func TestTermReportStreamRejectsExtraBytes(t *testing.T) {
	sum := sha256.Sum256([]byte("packet"))
	stream := daemonconn.TermReportStream{ReadCloser: io.NopCloser(strings.NewReader("packet!")),
		Size: 6, SHA256: hex.EncodeToString(sum[:])}
	_, err := stream.CopyVerified(io.Discard)
	require.ErrorIs(t, err, daemonconn.ErrIntegrity)
}
