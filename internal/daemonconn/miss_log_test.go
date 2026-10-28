package daemonconn_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/misslog"
)

func TestRecordMissBoundsFailedCall(t *testing.T) {
	deadline := make(chan time.Duration, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/miss-log", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		_, _ = io.Copy(io.Discard, r.Body)
		start := time.Now()
		<-r.Context().Done()
		deadline <- time.Since(start)
	}))
	defer ts.Close()
	start := time.Now()
	daemonconn.New(ts.URL, "key").RecordMiss(t.Context(), misslog.Request{Query: "synthetic miss"})
	require.Less(t, time.Since(start), 2*time.Second)
	<-deadline
}
