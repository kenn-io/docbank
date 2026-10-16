package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestBackupListDistinguishesUnrecordedCoverage(t *testing.T) {
	var out bytes.Buffer
	recorded := &backupapp.BackupCoverage{Version: 1, Included: backupapp.CoverageIncluded{
		PhotoAssets: 2, PhotoFiles: 3, PhotoBlobBytes: 42, Albums: 4, AlbumMembers: 5,
		HiddenPhotos: 6, TrashedPhotos: 7, CheckoutDefinitions: 8, CheckoutEntries: 9, RetainedReceipts: 10,
	}, Excluded: []string{"hidden_unlock_sessions", "photo_technical_metadata"}}
	err := writeBackupList(&out, []api.BackupSnapshot{
		{ID: "historical"},
		{ID: "recorded", CreatedAt: "2026-01-01T00:00:00Z", Files: 11, Blobs: 12, BytesAdded: 13, Tag: "daily", BlobBytes: 99, Coverage: recorded},
		{ID: "recorded-again", Coverage: recorded},
		{ID: "future", CoverageWarning: "unsupported backup coverage version 2"},
	})
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 7, "header, one row per snapshot, one warning, and one shared exclusions line")
	assert.Equal(t, []string{"historical", "0", "0", "0", "-", "0", "unrecorded", "-", "-", "-", "-"}, strings.Fields(lines[1]))
	assert.Equal(t, []string{"recorded", "2026-01-01T00:00:00Z", "11", "12", "13", "daily", "99", "2/3/42", "4/5", "6/7", "8/9", "10"}, strings.Fields(lines[2]))
	assert.Contains(t, lines[4], "unsupported")
	assert.Equal(t, "future: unsupported backup coverage version 2", lines[5])
	assert.Equal(t, "excluded: hidden_unlock_sessions, photo_technical_metadata", lines[6])
	out.Reset()
	require.NoError(t, writeBackupCoverage(&out, api.BackupSnapshot{ID: "future", CoverageWarning: "unsupported backup coverage version 2"}))
	assert.Equal(t, "future coverage: unsupported; unsupported backup coverage version 2\n", out.String())
}

func TestBackupCleanupCLIPreservesPartialErrorReports(t *testing.T) {
	for _, test := range []struct {
		command    string
		withReport bool
	}{{"forget", true}, {"prune", true}, {"forget", false}} {
		command := test.command
		for _, asJSON := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/json=%t/report=%t", command, asJSON, test.withReport), func(t *testing.T) {
				const first = "20260101T000000Z-11111111111111111111111111111111"
				const second = "20260102T000000Z-22222222222222222222222222222222"
				forget := api.BackupForgetReport{Selected: []string{first, second}, Forgotten: []string{second}}
				prune := api.BackupPruneReport{PacksToRemove: []string{"pack-a", "pack-b"}, PacksToRepack: []string{"pack-a"}, BytesToRemove: 100, LiveBytesToRewrite: 20, RemovedPacks: []string{"pack-b"}, BytesRemoved: 30, BytesWritten: 25}
				repo := t.TempDir()
				setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "/api/v1/backup/"+command, r.URL.Path)
					var request struct {
						Repo        string   `json:"repo"`
						SnapshotIDs []string `json:"snapshot_ids"`
					}
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
						return
					}
					assert.Equal(t, repo, request.Repo)
					problem := api.NewError(http.StatusInternalServerError, "backup_failed", "synthetic cleanup interruption")
					if command == "forget" {
						assert.Equal(t, []string{first, second}, request.SnapshotIDs)
						if test.withReport {
							problem.BackupForget = &forget
						}
					} else {
						problem.BackupPrune = &prune
					}
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusInternalServerError)
					assert.NoError(t, json.MarshalWrite(w, problem))
				})
				args := []string{"backup", command, "--repo", repo}
				if command == "forget" {
					args = append(args, first, second)
				}
				if asJSON {
					args = append(args, "--json")
				}
				out, err := runCLI(t, args...)
				require.ErrorContains(t, err, "synthetic cleanup interruption")
				status, ok := daemonconn.ResponseStatus(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusInternalServerError, status)
				code, ok := daemonconn.ProblemCode(err)
				require.True(t, ok)
				assert.Equal(t, "backup_failed", code)
				if !test.withReport {
					assert.Empty(t, out, "a refusal has no cleanup work to report")
					return
				}
				if asJSON {
					if command == "forget" {
						var got api.BackupForgetReport
						require.NoError(t, json.Unmarshal([]byte(out), &got))
						assert.Equal(t, forget, got)
					} else {
						var got api.BackupPruneReport
						require.NoError(t, json.Unmarshal([]byte(out), &got))
						assert.Equal(t, prune, got)
					}
				} else if command == "forget" {
					assert.Equal(t, "selected 2 snapshot(s); forgotten 1\nselected snapshots: "+first+", "+second+"\nforgotten snapshots: "+second+"\n", out)
				} else {
					assert.Equal(t, "planned 2 pack removal(s), 1 rewrite(s); removed 1 pack(s), 30 byte(s); wrote 25 byte(s)\nplanned removal bytes: 100; live bytes to rewrite: 20\nplanned pack removals: pack-a, pack-b\nplanned pack rewrites: pack-a\nremoved packs: pack-b\n", out)
				}
			})
		}
	}
}
