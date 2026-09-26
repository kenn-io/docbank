package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

func TestMCPReportCSVOpenRequiresCompanionPacketProof(t *testing.T) {
	id := strings.Repeat("a", 48)
	packet := syntheticVerifiedReportBundle(t)
	budget := report.NewBudget(16 << 20)
	t.Cleanup(func() { _ = budget.Close() })
	csv, err := report.ExtractVerifiedCSV(t.Context(), budget, bytes.NewReader(packet), int64(len(packet)))
	require.NoError(t, err)
	tampered := bytes.Clone(csv)
	tampered[0] ^= 1
	csvHash := sha256.Sum256(tampered)
	bundleHash := sha256.Sum256(packet)
	summary := report.Summary{ID: id, State: "complete", ObservedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
		Terms:     []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple"}},
		Counts:    []report.Counts{{}},
		CSVBytes:  int64(len(tampered)), CSVSHA256: hex.EncodeToString(csvHash[:]),
		BundleBytes: int64(len(packet)), BundleSHA256: hex.EncodeToString(bundleHash[:]),
	}
	var bundleReads int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-owner" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		path := "/api/v1/search-exports/" + id
		switch r.URL.Path {
		case path:
			w.Header().Set("Content-Type", "application/json")
			if marshalErr := json.MarshalWrite(w, summary); marshalErr != nil {
				t.Errorf("write synthetic summary: %v", marshalErr)
			}
		case path + "/csv", path + "/bundle":
			body := tampered
			if r.URL.Path == path+"/bundle" {
				bundleReads++
				body = packet
			}
			sum := sha256.Sum256(body)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("X-Docbank-Report-Sha256", hex.EncodeToString(sum[:]))
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-owner"), nil
	}, func(c *daemonconn.Connection) error { return c.Close() })
	signer := newReportHandleSigner()
	t.Cleanup(signer.closeAll)
	input, err := json.Marshal(map[string]string{"report_id": id, "format": "csv"})
	require.NoError(t, err)
	_, err = openReportArtifact(t.Context(), lease, signer, input)
	require.Error(t, err, "mismatched CSV evidence must not create an MCP handle")
	require.Equal(t, 1, bundleReads)
	signer.mu.Lock()
	defer signer.mu.Unlock()
	require.Empty(t, signer.spools)
	require.Zero(t, signer.reservedBytes)
}
