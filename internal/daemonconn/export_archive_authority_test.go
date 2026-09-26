package daemonconn

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
)

func TestExportArchiveAuthorityBindsReceiptAndRejectsInvalidJob(t *testing.T) {
	id := uuid.NewString()
	want := bundle.Receipt{Format: bundle.Format, PlanFingerprint: strings.Repeat("b", 64),
		SHA256: strings.Repeat("a", 64), Size: 4096, Entries: 3}
	requests := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/v1/exports/jobs/"+id+"/archive/authority" || r.Header.Get("X-Api-Key") != "synthetic-owner" {
			t.Errorf("wrong export authority request")
			http.Error(w, "wrong request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"format":"` + want.Format + `","plan_fingerprint":"` + want.PlanFingerprint +
			`","sha256":"` + want.SHA256 + `","size":4096,"entries":3}`))
	}))
	t.Cleanup(backend.Close)
	connection := New(backend.URL, "synthetic-owner")
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	got, err := connection.ExportArchiveAuthority(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, want, got)
	_, err = connection.ExportArchiveAuthority(t.Context(), "invalid")
	require.Error(t, err)
	require.Equal(t, 1, requests)
}
