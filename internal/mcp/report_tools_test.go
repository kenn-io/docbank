package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

func syntheticVerifiedReportBundle(t *testing.T) []byte {
	t.Helper()
	budget := report.NewBudget(16 << 20)
	t.Cleanup(func() { _ = budget.Close() })
	frame := report.Frame{VaultID: "synthetic-vault", GenerationKind: "native",
		ObservedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		Request: report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict",
			Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
				Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}}
	result, err := report.Calculate(t.Context(), budget, frame)
	require.NoError(t, err)
	packet, err := report.BuildBundle(t.Context(), budget, result)
	require.NoError(t, err)
	return packet
}

func syntheticLargeVerifiedReportArtifacts(t *testing.T) ([]report.Term, []report.Counts, []byte, []byte) {
	t.Helper()
	terms := make([]report.Term, 80)
	for index := range terms {
		terms[index] = report.Term{Number: index + 1, Expression: strings.Repeat("alpha", 700),
			Syntax: "simple", Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}
	}
	frame := report.Frame{VaultID: "synthetic-vault", GenerationKind: "native",
		ObservedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		Request:    report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict", Terms: terms}}
	budget := report.NewBudget(32 << 20)
	t.Cleanup(func() { _ = budget.Close() })
	result, err := report.Calculate(t.Context(), budget, frame)
	require.NoError(t, err)
	packet, err := report.BuildBundle(t.Context(), budget, result)
	require.NoError(t, err)
	var csv bytes.Buffer
	require.NoError(t, report.WriteCSV(t.Context(), &csv, result))
	require.Greater(t, csv.Len(), maxReportChunkBytes)
	return terms, result.Counts, csv.Bytes(), packet
}

func TestReportArtifactSDKOpenDownloadIsVerifiedOwnerBoundAndBounded(t *testing.T) {
	id := strings.Repeat("a", 48)
	terms, counts, csv, packet := syntheticLargeVerifiedReportArtifacts(t)
	content := map[string][]byte{
		"csv":    csv,
		"bundle": packet,
	}
	var activeKey atomic.Value
	activeKey.Store("owner-a")
	var revoked, withdrawn, summaryFailure, corruptBody, corruptHeader, invalidBundle atomic.Bool
	var masterRequests atomic.Int32
	var acquisitions atomic.Int32
	var largeBytes atomic.Int64
	var largeRequests atomic.Int32
	var summaryRequests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "master-key" {
			masterRequests.Add(1)
		}
		if r.Header.Get("X-Api-Key") != "owner-a" || revoked.Load() {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusGone)
			_, _ = fmt.Fprint(w, `{"title":"Gone","status":410,"code":"report_unavailable"}`)
			return
		}
		if r.URL.Path == "/api/v1/search-exports/"+id {
			if summaryFailure.Load() {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, `{"title":"Busy","status":503,"code":"temporary_failure"}`)
				return
			}
			if withdrawn.Load() {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusGone)
				_, _ = fmt.Fprint(w, `{"title":"Gone","status":410,"code":"visibility_changed"}`)
				return
			}
			summaryRequests.Add(1)
			csvSum := sha256.Sum256(content["csv"])
			bundleSum := sha256.Sum256(content["bundle"])
			csvDigest := hex.EncodeToString(csvSum[:])
			if corruptHeader.Load() {
				csvDigest = strings.Repeat("0", 64)
			}
			summary := report.Summary{ID: id, State: "complete",
				ObservedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
				ExpiresAt:  time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC),
				Terms:      terms, Counts: counts, CSVBytes: int64(len(content["csv"])),
				BundleBytes: int64(len(content["bundle"])), CSVSHA256: csvDigest,
				BundleSHA256: hex.EncodeToString(bundleSum[:])}
			w.Header().Set("Content-Type", "application/json")
			_ = json.MarshalWrite(w, summary)
			return
		}
		format := strings.TrimPrefix(r.URL.Path, "/api/v1/search-exports/"+id+"/")
		body, ok := content[format]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if format == "bundle" && invalidBundle.Load() {
			body = []byte("synthetic invalid report bundle")
		}
		if format == "csv" {
			largeRequests.Add(1)
		}
		sum := sha256.Sum256(body)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if corruptHeader.Load() {
			w.Header().Set("X-Docbank-Report-Sha256", strings.Repeat("0", 64))
		} else {
			w.Header().Set("X-Docbank-Report-Sha256", hex.EncodeToString(sum[:]))
		}
		if corruptBody.Load() {
			changed := bytes.Clone(body)
			changed[0] ^= 1
			body = changed
		}
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()
		for len(body) > 0 {
			part := body[:min(len(body), 4096)]
			written, writeErr := w.Write(part)
			if format == "csv" {
				largeBytes.Add(int64(written))
			}
			if writeErr != nil || written == 0 {
				return
			}
			body = body[written:]
			if format == "csv" {
				time.Sleep(100 * time.Microsecond)
			}
		}
	}))
	t.Cleanup(backend.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		acquisitions.Add(1)
		key, ok := activeKey.Load().(string)
		if !ok {
			return nil, errors.New("synthetic owner key is unavailable")
		}
		return daemonconn.New(backend.URL, key), nil
	}, func(client *daemonconn.Connection) error { return client.Close() })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	t.Cleanup(server.reports.closeAll)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, serverTransport) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-client"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	call := func(name string, args map[string]any) (map[string]any, bool, error) {
		t.Helper()
		result, callErr := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
		if callErr != nil {
			return nil, false, fmt.Errorf("calling %s: %w", name, callErr)
		}
		encoded, marshalErr := json.Marshal(result.StructuredContent)
		require.NoError(t, marshalErr)
		var output map[string]any
		require.NoError(t, json.Unmarshal(encoded, &output))
		return output, result.IsError, nil
	}
	openBundleHandle := func() string {
		t.Helper()
		opened, failed, err := call("open_report_artifact", map[string]any{
			"report_id": id, "format": "bundle",
		})
		require.NoError(t, err)
		require.False(t, failed)
		handle, ok := opened["handle"].(string)
		require.True(t, ok)
		return handle
	}
	requireNoSpools := func(reason string) {
		t.Helper()
		server.reports.mu.Lock()
		count, reserved := len(server.reports.spools), server.reports.reservedBytes
		server.reports.mu.Unlock()
		require.Zero(t, count, reason)
		require.Zero(t, reserved, reason)
	}
	var csvHandle string
	for _, format := range []string{"csv", "bundle"} {
		opened, isError, err := call("open_report_artifact", map[string]any{"report_id": id, "format": format})
		require.NoError(t, err)
		require.False(t, isError)
		require.Equal(t, "private", opened["cacheScope"])
		handle, ok := opened["handle"].(string)
		require.True(t, ok)
		if format == "csv" {
			csvHandle = handle
			second, secondFailed, secondErr := call("open_report_artifact", map[string]any{"report_id": id, "format": format})
			require.NoError(t, secondErr)
			require.False(t, secondFailed, "two opens of the same artifact must have distinct live handles")
			require.NotEqual(t, handle, second["handle"])
			_, secondFailed, secondErr = call("download_report_artifact", map[string]any{
				"handle": second["handle"], "offset": 0, "max_bytes": 1, "close": true,
			})
			require.NoError(t, secondErr)
			require.False(t, secondFailed)

			largest, failed, err := call("download_report_artifact", map[string]any{
				"handle": handle, "offset": 0, "max_bytes": maxReportChunkBytes,
			})
			require.NoError(t, err)
			require.False(t, failed)
			encoded, ok := largest["data_base64"].(string)
			require.True(t, ok)
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			require.NoError(t, err)
			require.Len(t, decoded, maxReportChunkBytes)
		}
		var got []byte
		size, ok := opened["size"].(float64)
		require.True(t, ok)
		for offset := int64(0); ; {
			closeHandle := format == "bundle" && offset+maxReportChunkBytes >= int64(size)
			chunk, failed, err := call("download_report_artifact", map[string]any{
				"handle": handle, "offset": offset, "max_bytes": maxReportChunkBytes, "close": closeHandle,
			})
			require.NoError(t, err)
			require.False(t, failed)
			encoded, ok := chunk["data_base64"].(string)
			require.True(t, ok)
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			require.NoError(t, err)
			got = append(got, decoded...)
			require.Equal(t, closeHandle, chunk["closed"])
			nextOffset, ok := chunk["next_offset"].(float64)
			require.True(t, ok)
			offset = int64(nextOffset)
			if chunk["eof"] == true {
				break
			}
		}
		require.Equal(t, content[format], got)
		require.Equal(t, opened["sha256"], fmt.Sprintf("%x", sha256.Sum256(got)))
		if format == "csv" {
			assert.Equal(t, int32(2), largeRequests.Load(),
				"chunk authorization must not start another artifact stream")
			assert.Greater(t, summaryRequests.Load(), int32(2),
				"the test must exercise multiple metadata rechecks")
			require.Less(t, largeBytes.Load(), int64(len(got))*3,
				"chunk downloads must not stream the complete artifact repeatedly")
			t.Logf("synthetic CSV: %d bytes, %d GETs, %d backend bytes served",
				len(got), largeRequests.Load(), largeBytes.Load())
		}
		if format == "bundle" {
			server.reports.mu.Lock()
			remaining := len(server.reports.spools)
			server.reports.mu.Unlock()
			require.Equal(t, 1, remaining, "explicit close must release the bundle spool")
		}
	}
	invalidBundle.Store(true)
	_, _, err = call("open_report_artifact", map[string]any{"report_id": id, "format": "bundle"})
	require.Error(t, err, "an invalid bundle with a matching transport hash must not return a handle")
	server.reports.mu.Lock()
	remainingAfterInvalid := len(server.reports.spools)
	server.reports.mu.Unlock()
	require.Equal(t, 1, remainingAfterInvalid, "invalid bundle must not leave a private spool")
	invalidBundle.Store(false)

	server.reports.mu.Lock()
	reservedBeforeCapacity := server.reports.reservedBytes
	server.reports.reservedBytes = maxReportSpoolBytes
	server.reports.mu.Unlock()
	capacity, failed, err := call("open_report_artifact", map[string]any{"report_id": id, "format": "csv"})
	server.reports.mu.Lock()
	server.reports.reservedBytes = reservedBeforeCapacity
	server.reports.mu.Unlock()
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, "report_capacity", capacity["code"])

	_, _, err = call("download_report_artifact", map[string]any{
		"handle": csvHandle, "offset": 0, "max_bytes": maxReportChunkBytes + 1,
	})
	require.Error(t, err, "an over-limit SDK request must be rejected")

	replacement := "A"
	if strings.HasSuffix(csvHandle, replacement) {
		replacement = "B"
	}
	forged := csvHandle[:len(csvHandle)-1] + replacement
	invalid, failed, err := call("download_report_artifact", map[string]any{
		"handle": forged, "offset": 0, "max_bytes": 16,
	})
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, "report_unavailable", invalid["code"])

	corruptBody.Store(true)
	_, _, err = call("open_report_artifact", map[string]any{"report_id": id, "format": "csv"})
	require.Error(t, err, "an unverified body must not create an open handle")
	corruptBody.Store(false)

	corruptHeader.Store(true)
	changed, failed, err := call("download_report_artifact", map[string]any{
		"handle": csvHandle, "offset": 0, "max_bytes": 16,
	})
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, "report_unavailable", changed["code"])
	require.NotContains(t, changed, "data_base64", "changed SHA authority must not return a partial chunk")
	requireNoSpools("changed artifact authority must release the private spool")
	corruptHeader.Store(false)
	visibilityHandle := openBundleHandle()
	withdrawn.Store(true)
	withheld, failed, err := call("download_report_artifact", map[string]any{
		"handle": visibilityHandle, "offset": 0, "max_bytes": 16,
	})
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, "visibility_changed", withheld["code"])
	require.NotContains(t, withheld, "data_base64")
	requireNoSpools("withdrawn report visibility must release the private spool")
	withdrawn.Store(false)

	closeHandle := openBundleHandle()
	summaryFailure.Store(true)
	_, _, err = call("download_report_artifact", map[string]any{
		"handle": closeHandle, "offset": 0, "max_bytes": 16, "close": true,
	})
	require.Error(t, err, "temporary daemon failure must not return a chunk")
	requireNoSpools("close=true must release the private spool even if recheck fails")
	summaryFailure.Store(false)

	ownerHandle := openBundleHandle()
	revokedHandle := openBundleHandle()

	activeKey.Store("owner-b")
	lease.mu.Lock()
	current := leasedDaemonClient{daemonconn: lease.daemonconn, generation: lease.generation}
	lease.mu.Unlock()
	lease.discard(current)
	denied, failed, err := call("download_report_artifact", map[string]any{
		"handle": ownerHandle, "offset": 0, "max_bytes": 16,
	})
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, "report_unavailable", denied["code"])
	server.reports.mu.Lock()
	_, ownerHandleStillOpen := server.reports.spools[ownerHandle]
	server.reports.mu.Unlock()
	require.False(t, ownerHandleStillOpen, "wrong owner must release its local handle")
	denied, failed, err = call("open_report_artifact", map[string]any{"report_id": id, "format": "csv"})
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, "report_unavailable", denied["code"])

	activeKey.Store("owner-a")
	lease.mu.Lock()
	current = leasedDaemonClient{daemonconn: lease.daemonconn, generation: lease.generation}
	lease.mu.Unlock()
	lease.discard(current)
	revoked.Store(true)
	denied, failed, err = call("download_report_artifact", map[string]any{
		"handle": revokedHandle, "offset": 0, "max_bytes": 16,
	})
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, "report_unavailable", denied["code"])
	requireNoSpools("revoked owner must release the private spool")
	denied, failed, err = call("open_report_artifact", map[string]any{"report_id": id, "format": "csv"})
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, "report_unavailable", denied["code"])
	require.Zero(t, masterRequests.Load())
	require.Equal(t, int32(3), acquisitions.Load(), "authorization denial must not reacquire a broader daemon client")
}

func TestReportHandleRejectsExpiredAndMutatedAuthority(t *testing.T) {
	signer := newReportHandleSigner()
	base := reportHandle{ID: strings.Repeat("a", 48), Format: "csv", Size: 7,
		SHA256: strings.Repeat("b", 64), Expires: 1}
	token, err := signer.sign(&base)
	require.NoError(t, err)
	_, err = signer.verify(token)
	require.ErrorIs(t, err, errReportHandleUnavailable)
	_, err = signer.verify(strings.Repeat("x", maxReportHandleBytes+1))
	require.ErrorIs(t, err, errReportHandleUnavailable)
}
