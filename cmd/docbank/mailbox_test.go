package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

type mailboxCancelReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *mailboxCancelReader) Read(p []byte) (int, error) {
	r.reads++
	r.cancel()
	return copy(p, "synthetic"), nil
}

func TestMailboxHashStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &mailboxCancelReader{cancel: cancel}
	_, err := hashMailboxSource(ctx, reader, 1<<30)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, reader.reads)
	_, err = hashMailboxSource(t.Context(), strings.NewReader("changed"), 3)
	require.Error(t, err)
}

func TestMailboxUploadReusesMatchingChunks(t *testing.T) {
	const hash = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	for _, tc := range []struct {
		name         string
		chunks       []store.MailboxChunk
		wantUploads  int
		wantConflict bool
	}{
		{name: "missing", wantUploads: 1},
		{name: "saved", chunks: []store.MailboxChunk{{Index: 0, SHA256: hash, Size: 3}}},
		{name: "different hash", chunks: []store.MailboxChunk{{Index: 0, SHA256: strings.Repeat("a", 64), Size: 3}}, wantConflict: true},
		{name: "different size", chunks: []store.MailboxChunk{{Index: 0, SHA256: hash, Size: 2}}, wantConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "abc" || r.URL.Path != "/api/v1/mailbox/containers/source/chunks/0" {
					t.Errorf("unexpected upload %s: %q, %v", r.URL.Path, body, err)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, store.MailboxChunk{Index: 0, SHA256: hash, Size: 3})
			}))
			defer server.Close()
			container := store.MailboxContainer{ID: "source", SHA256: hash, Size: 3, Format: "mbox", State: "uploading", Chunks: tc.chunks}
			err := uploadMailboxChunks(t.Context(), daemonconn.New(server.URL, "test-key"), strings.NewReader("abc"), container, io.Discard)
			if tc.wantConflict {
				require.ErrorIs(t, err, store.ErrMailboxConflict)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantUploads, int(uploads.Load()))
		})
	}
}

func TestMailboxImportReusesSourceWithSeparateJob(t *testing.T) {
	setupVaultHome(t)
	_, err := runCLI(t, "mkdir", "/Second")
	require.NoError(t, err)
	source := writeSourceFile(t, "synthetic.mbox", "From sender@example.test Sat Sep 12 10:00:00 2026\nSubject: Synthetic\n\nSynthetic message.\n")
	var jobs []store.MailboxJob
	for _, flags := range [][]string{{}, {}, {"--job-id", "second", "--dest", "/Second"}} {
		command := newMailboxCommand()
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(io.Discard)
		command.SetArgs(append([]string{"import", source, "--id", "source"}, flags...))
		require.NoError(t, command.ExecuteContext(t.Context()))
		var job store.MailboxJob
		require.NoError(t, json.Unmarshal(out.Bytes(), &job))
		jobs = append(jobs, job)
	}
	require.Equal(t, jobs[0].ID, jobs[1].ID)
	require.Equal(t, jobs[0].CollectionID, jobs[1].CollectionID)
	require.Equal(t, "second", jobs[2].ID)
	require.Equal(t, jobs[0].ContainerID, jobs[2].ContainerID)
	require.NotEqual(t, jobs[0].Settings.DestinationID, jobs[2].Settings.DestinationID)
	require.NotEqual(t, jobs[0].CollectionID, jobs[2].CollectionID)
}
