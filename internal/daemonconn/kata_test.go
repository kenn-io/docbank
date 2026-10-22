package daemonconn

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/kataevidence"
)

func TestKataContextClassifiesInvalidRepliesAsDecodeErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `not-json`},
		{name: "missing issue identity", body: `{"issue":{},"passages":[]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)

			_, err := New(server.URL, "synthetic-kata-token").KataContext(t.Context(), kataevidence.ContextRequest{Issue: "docbank#abcd"})

			require.Error(t, err)
			require.True(t, IsResponseDecodeError(err), "wrong response classification: %v", err)
		})
	}
}
