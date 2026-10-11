package daemonconn

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func clientCitation(t *testing.T) document.ResolvedTextCitation {
	t.Helper()
	vault, err := uuid.Parse("11111111-1111-4111-8111-111111111111")
	require.NoError(t, err)
	version, err := uuid.Parse("22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	return document.ResolvedTextCitation{
		Citation: document.TextCitation{Version: 1, VaultUID: vault, NodeID: 7,
			ContentVersionID: version, ContentSHA256: strings.Repeat("a", 64),
			RenditionAttachmentID: strings.Repeat("b", 64), BuildID: strings.Repeat("c", 64),
			RenditionSHA256: strings.Repeat("d", 64), Start: 1, End: 4},
		Text: "é界🙂", TextBytes: 9, TextSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("é界🙂"))),
	}
}

func TestTextCitationClient(t *testing.T) {
	t.Parallel()
	want := clientCitation(t)
	valid, err := json.Marshal(want)
	require.NoError(t, err)
	changed := func(edit func(*document.ResolvedTextCitation)) string {
		got := want
		edit(&got)
		encoded, err := json.Marshal(got)
		require.NoError(t, err)
		return string(encoded)
	}
	cases := []struct {
		name, body string
		success    bool
	}{
		{"valid", string(valid), true},
		{"schema annotation", strings.TrimSuffix(string(valid), "}") + `,"$schema":"synthetic"}`, true},
		{"identity", changed(func(r *document.ResolvedTextCitation) { r.Citation.NodeID++ }), false},
		{"quote", changed(func(r *document.ResolvedTextCitation) {
			r.Text = "abcdefghi"
			r.TextSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(r.Text)))
		}), false},
		{"bytes", changed(func(r *document.ResolvedTextCitation) { r.TextBytes++ }), false},
		{"digest", changed(func(r *document.ResolvedTextCitation) {
			r.TextSHA256 = strings.Repeat("0", 64)
		}), false},
		{"extra", strings.TrimSuffix(string(valid), "}") + `,"extra":true}`, false},
		{"invalid UTF8", strings.Replace(string(valid), "é界🙂", "\xff", 1), false},
		{"oversized", strings.Repeat(" ", 1<<20) + string(valid), false},
		{"truncated JSON", string(valid[:len(valid)-1]), false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "POST", r.Method)
				assert.Equal(t, "/api/v1/text-citations/resolve", r.URL.Path)
				assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
				var got document.TextCitation
				assert.NoError(t, json.UnmarshalRead(r.Body, &got))
				assert.Equal(t, want.Citation, got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			got, err := New(server.URL, "synthetic-key").ResolveTextCitation(t.Context(), want.Citation)
			require.Equal(t, 1, calls)
			if test.success {
				require.NoError(t, err)
				require.Equal(t, want, got)
			} else {
				require.Error(t, err)
				require.Empty(t, got.Text)
			}
		})
	}
}

func TestTextCitationClientTransport(t *testing.T) {
	t.Parallel()
	want := clientCitation(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "500")
		_, _ = io.WriteString(w, `{"citation":`)
	}))
	defer server.Close()
	_, err := New(server.URL, "synthetic-key").ResolveTextCitation(t.Context(), want.Citation)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NotErrorIs(t, err, document.ErrCitationIntegrity)
}
