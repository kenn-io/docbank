package daemonconn

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/misslog"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestRecordMissBoundsFailedCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New("http://synthetic.invalid", "key")
		c.hc.Transport = failingRoundTripper(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "/api/v1/miss-log", r.URL.Path)
			require.Equal(t, http.MethodPost, r.Method)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		start := time.Now()
		c.RecordMiss(t.Context(), misslog.Request{Query: "synthetic miss"})
		require.Equal(t, time.Second, time.Since(start))
	})
}
