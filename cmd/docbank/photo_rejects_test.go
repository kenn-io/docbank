package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPhotoRejectsCLIBoundary(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"photos", "rejects"})
	require.NoError(t, err)
	for _, flag := range []string{"query", "confirm", "hidden", "coverage", "profile-fingerprint"} {
		require.NotNil(t, command.Flags().Lookup(flag))
	}
	for _, confirm := range []bool{false, true} {
		for _, hidden := range []bool{false, true} {
			if hidden && !confirm {
				continue
			}
			t.Run(strings.Join([]string{map[bool]string{false: "preview", true: "confirm"}[confirm], map[bool]string{false: "library", true: "hidden"}[hidden]}, "/"), func(t *testing.T) {
				digest := strings.Repeat("a", 64)
				photos := 1
				if confirm && !hidden {
					photos = 0
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/api/v1/photos/hidden/unlock" {
						var body api.PhotoHiddenPasscodeRequest
						require.NoError(t, json.UnmarshalRead(r.Body, &body))
						assert.Equal(t, "synthetic-passcode", body.Passcode)
						w.Header().Set("Set-Cookie", "docbank-hidden-test=synthetic-token; Path=/")
						_, _ = w.Write([]byte(`{}`))
						return
					}
					path := "/api/v1/photos/rejects/preflight"
					if confirm {
						path = "/api/v1/photos/rejects/trash"
					}
					assert.Equal(t, path, r.URL.Path)
					assert.Equal(t, http.MethodPost, r.Method)
					var body api.PhotoRejectsRequest
					require.NoError(t, json.UnmarshalRead(r.Body, &body))
					assert.Equal(t, hidden, body.Hidden)
					assert.Equal(t, "unconfigured", body.Coverage.Configuration)
					assert.Equal(t, confirm, body.Digest == digest)
					if hidden {
						assert.Contains(t, r.Header.Get("Cookie"), "docbank-hidden-test=synthetic-token")
					}
					assert.NoError(t, json.MarshalWrite(w, api.PhotoRejectsPreflight{Digest: digest, Photos: photos, Files: photos, Unchanged: 1001}))
				}))
				defer server.Close()
				var out bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetContext(t.Context())
				cmd.SetOut(&out)
				cmd.SetIn(strings.NewReader("synthetic-passcode\n"))
				request := api.PhotoRejectsRequest{Query: api.QueryPayload(`{}`), Hidden: hidden, Coverage: api.WorkspaceQueryCoverage{Configuration: "unconfigured"}}
				if confirm {
					request.Digest = digest
				}
				require.NoError(t, runPhotoRejects(cmd, daemonconn.New(server.URL, "synthetic-key"), request))
				assert.Contains(t, out.String(), `"photos":`+map[int]string{0: "0", 1: "1"}[photos])
				assert.Contains(t, out.String(), `"moved":`+map[bool]string{false: "false", true: "true"}[confirm && photos > 0])
			})
		}
	}
}
