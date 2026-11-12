package daemonconn

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocuSealRejectsInvalidDaemonResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     bool
	}{
		{"missing status fields", `{}`, true},
		{"oversized status", strings.Repeat("x", 3<<20+1), true},
		{"missing templates", `{}`, false},
		{"invalid template", `{"templates":[{"id":0,"name":"Lease","roles":[]}],"next":0}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer s.Close()
			c := New(s.URL, "synthetic-daemon-key")
			var err error
			if tc.status {
				_, err = c.DocuSealStatus(t.Context())
			} else {
				_, err = c.DocuSealTemplates(t.Context(), 0)
			}
			if err == nil || !IsResponseDecodeError(err) {
				t.Fatalf("wrong response classification: %v", err)
			}
		})
	}
}
