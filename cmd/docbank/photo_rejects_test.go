package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
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
	for _, flag := range []string{"query", "confirm", "coverage", "profile-fingerprint"} {
		require.NotNil(t, command.Flags().Lookup(flag))
	}
	for _, flag := range []string{"hidden"} {
		require.Nil(t, command.Flags().Lookup(flag))
	}
	require.NoError(t, command.Flags().Set("query", "{}"))
	require.NoError(t, command.Flags().Set("confirm", "-"))
	require.ErrorContains(t, command.ValidateFlagGroups(), "query confirm")
	for _, name := range []string{"query", "confirm"} {
		require.NoError(t, command.Flags().Lookup(name).Value.Set(command.Flags().Lookup(name).DefValue))
		command.Flags().Lookup(name).Changed = false
	}
	preview := api.PhotoRejectsPreflight{Photos: 1001, Files: 1001, Targets: []store.PhotoRejectTarget{{AssetID: "11111111-1111-4111-8111-111111111111", Revision: 7, MemberRevision: 11}}, Mixed: []store.PhotoRejectMixed{}}
	for group := range store.MaxPhotoRejectsMixed {
		mixed := store.PhotoRejectMixed{AssetID: fmt.Sprintf("22222222-2222-4222-8222-%012d", group)}
		for i := range 256 {
			flag := "reject"
			if i == 0 {
				flag = "pick"
			}
			mixed.Members = append(mixed.Members, store.PhotoRejectMember{FileID: fmt.Sprintf("33333333-3333-4333-8333-%012d", group*256+i), Name: strings.Repeat("a", 250) + ".raw", Flag: flag})
		}
		preview.Mixed = append(preview.Mixed, mixed)
	}
	preview.Unchanged, preview.MixedCount = len(preview.Mixed), len(preview.Mixed)
	raw, err := json.Marshal(preview)
	require.NoError(t, err)
	require.Greater(t, len(raw), 1<<20)
	for _, confirm := range []string{"", "-", "file"} {
		t.Run(confirm, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.Equal(t, http.MethodPost, r.Method)
				if confirm == "" {
					assert.Equal(t, "/api/v1/photos/rejects/preflight", r.URL.Path)
					var body api.PhotoRejectsRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &body))
					assert.Equal(t, api.WorkspaceQueryCoverage{Configuration: "configured", ProfileFingerprint: strings.Repeat("a", 64)}, body.Coverage)
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
			request := api.PhotoRejectsRequest{Query: api.QueryPayload(`{}`), Coverage: api.WorkspaceQueryCoverage{Configuration: "configured", ProfileFingerprint: strings.Repeat("a", 64)}}
			require.NoError(t, runPhotoRejects(cmd, daemonconn.New(server.URL, "synthetic-key"), request, path))
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
	path := filepath.Join(t.TempDir(), "newer-preview.json")
	require.NoError(t, os.WriteFile(path, append([]byte(`{"unknown":true,`), raw[1:]...), 0600))
	require.ErrorContains(t, runPhotoRejects(cmd, nil, api.PhotoRejectsRequest{}, path), "unknown")
}
