package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoRejectsCLIBoundary(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"photos", "rejects"})
	require.NoError(t, err)
	for _, flag := range []string{"query", "confirm"} {
		require.NotNil(t, command.Flags().Lookup(flag))
	}
	for _, flag := range []string{"hidden", "coverage", "profile-fingerprint"} {
		require.Nil(t, command.Flags().Lookup(flag))
	}
	preview := api.PhotoRejectsPreflight{Photos: 1001, Files: 1001, Movable: 1, Targets: []store.PhotoRejectTarget{{AssetID: "11111111-1111-4111-8111-111111111111", Revision: 7, MemberRevision: 11}}, Mixed: []store.PhotoRejectMixed{}}
	raw, err := json.Marshal(preview)
	require.NoError(t, err)
	for _, confirm := range []string{"", "-", "file"} {
		t.Run(confirm, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.Equal(t, http.MethodPost, r.Method)
				if confirm == "" {
					assert.Equal(t, "/api/v1/photos/rejects/preflight", r.URL.Path)
					assert.NoError(t, json.MarshalWrite(w, preview))
				} else {
					assert.Equal(t, "/api/v1/photos/rejects/trash", r.URL.Path)
					var body api.MovePhotoRejectsRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &body))
					assert.Equal(t, preview.Targets, body.Targets)
					assert.False(t, body.Hidden)
					assert.NoError(t, json.MarshalWrite(w, api.PhotoRejectsMoved{Moved: []string{preview.Targets[0].AssetID}}))
				}
			}))
			defer server.Close()
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			cmd.SetOut(&out)
			cmd.SetIn(bytes.NewReader(raw))
			path := confirm
			if confirm == "file" {
				path = filepath.Join(t.TempDir(), "preview.json")
				require.NoError(t, os.WriteFile(path, raw, 0600))
			}
			require.NoError(t, runPhotoRejects(cmd, daemonconn.New(server.URL, "synthetic-key"), api.PhotoRejectsRequest{Query: api.QueryPayload(`{}`)}, path))
			if confirm == "" {
				assert.JSONEq(t, string(raw), out.String())
			} else {
				assert.JSONEq(t, `{"moved":["11111111-1111-4111-8111-111111111111"]}`, out.String())
			}
		})
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetIn(strings.NewReader(`{"digest":"old"}`))
	require.ErrorContains(t, runPhotoRejects(cmd, nil, api.PhotoRejectsRequest{}, "-"), "decoding rejects preview")
}
