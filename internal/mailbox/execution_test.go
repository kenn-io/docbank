package mailbox

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func queuedArchive(t *testing.T, f *Service, raw []byte, format string) store.MailboxJobRequest {
	t.Helper()
	ctx := t.Context()
	c := store.MailboxContainerRequest{ID: "source", Owner: "one", SHA256: hashBytes(raw), Size: int64(len(raw)), Format: format}
	_, err := f.Store.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, f.UploadChunk(ctx, c.Owner, c.ID, 0, c.SHA256, c.Size, bytes.NewReader(raw)))
	_, err = f.Seal(ctx, c.Owner, c.ID)
	require.NoError(t, err)
	r := store.MailboxJobRequest{ID: "job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: store.MailboxSettings{DestinationID: f.Store.RootID()}}
	_, err = f.Store.BeginMailboxJob(ctx, c.Owner, r)
	require.NoError(t, err)
	return r
}

func TestMailboxEmptyZIPEntriesDoNotHideMessages(t *testing.T) {
	f := mailboxFixture(t)
	var raw bytes.Buffer
	z := zip.NewWriter(&raw)
	for i, content := range []string{"", separator + "Subject: Synthetic\n\nBody\n", ""} {
		w, err := z.Create(fmt.Sprintf("%d.mbox", i))
		require.NoError(t, err)
		_, err = io.WriteString(w, content)
		require.NoError(t, err)
	}
	require.NoError(t, z.Close())
	r := queuedArchive(t, f, raw.Bytes(), "zip")
	preview, err := f.Preview(t.Context(), "one", r.ContainerID, "mboxrd")
	require.NoError(t, err)
	require.Len(t, preview.Samples, 1)
	j, err := f.Store.ClaimMailboxJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.RunJob(t.Context(), j))
	j, err = f.Store.MailboxJob(t.Context(), "one", j.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", j.State)
	require.Equal(t, int64(1), j.Imported)
}

func TestMailboxEmptyZIPArchiveCompletes(t *testing.T) {
	f := mailboxFixture(t)
	r := queuedArchive(t, f, syntheticZIP(t, "empty.mbox", 0600, ""), "zip")
	preview, err := f.Preview(t.Context(), "one", r.ContainerID, "mboxrd")
	require.NoError(t, err)
	require.Empty(t, preview.Samples)
	j, err := f.Store.ClaimMailboxJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.RunJob(t.Context(), j))
	j, err = f.Store.MailboxJob(t.Context(), "one", j.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", j.State)
	require.True(t, j.ScannedTail)
	require.Zero(t, j.Checkpoint)
	require.NoError(t, f.Store.ValidateMetadata(t.Context()))
}

func TestMailboxPreviewSamplesBeforeWorkerVerifiesWholeZIP(t *testing.T) {
	f := mailboxFixture(t)
	var raw bytes.Buffer
	z := zip.NewWriter(&raw)
	w, err := z.Create("messages.mbox")
	require.NoError(t, err)
	_, err = io.WriteString(w, strings.Repeat(separator+"Subject: Sample\n\nBody\n", 5))
	require.NoError(t, err)
	w, err = z.Create("unrelated.txt")
	require.NoError(t, err)
	_, err = io.WriteString(w, "synthetic unrelated file")
	require.NoError(t, err)
	require.NoError(t, z.Close())
	data := raw.Bytes()
	central := bytes.Index(data, []byte{'P', 'K', 1, 2})
	central += bytes.Index(data[central+4:], []byte{'P', 'K', 1, 2}) + 4
	binary.LittleEndian.PutUint32(data[central+16:], 0)
	r := queuedArchive(t, f, data, "zip")
	preview, err := f.Preview(t.Context(), "one", r.ContainerID, "mboxrd")
	require.NoError(t, err, "preview must not decompress unrelated entries")
	require.Len(t, preview.Samples, 3)
	j, err := f.Store.ClaimMailboxJob(t.Context())
	require.NoError(t, err)
	require.ErrorIs(t, f.RunJob(t.Context(), j), zip.ErrChecksum)
	j, err = f.Store.MailboxJob(t.Context(), "one", j.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", j.State)
	require.Zero(t, j.Imported, "all entries must verify before publication")
}

func TestMailboxPreviewBoundsBytesBeforeNextSeparator(t *testing.T) {
	f := mailboxFixture(t)
	raw := syntheticZIP(t, "messages.mbox", 0600, separator+"Subject: Large sample\n\n"+strings.Repeat("x", 17<<20))
	r := queuedArchive(t, f, raw, "zip")
	preview, err := f.Preview(t.Context(), "one", r.ContainerID, "mboxrd")
	require.NoError(t, err)
	require.True(t, preview.HasMore)
	require.Empty(t, preview.Samples, "an incomplete message must not appear as a complete sample")
}

func TestMailboxMalformedHeadersRetainDecoderEvidenceAndLabels(t *testing.T) {
	f := mailboxFixture(t)
	queuedArchive(t, f, []byte(separator+"Subject: Synthetic\nMalformed header\nX-Gmail-Labels: Inbox,\n Project\nContent-Type: text/plain\n\nBody\n"), "mbox")
	j, err := f.Store.ClaimMailboxJob(t.Context())
	require.NoError(t, err)
	f.Mutate = func(ctx context.Context, fn func() error) error {
		current, err := f.Store.MailboxJob(ctx, "one", j.ID)
		if err != nil {
			return err
		}
		if current.Pending == 1 {
			spools, err := filepath.Glob(filepath.Join(f.Spool, "docbank-email-*"))
			require.NoError(t, err)
			require.Len(t, spools, 2, "the scanner source and decoder inventory are the only spools")
		}
		return fn()
	}
	require.NoError(t, f.RunJob(t.Context(), j))
	occurrences, err := f.Store.MailboxOccurrences(t.Context(), "one", j.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, occurrences, 1)
	require.Equal(t, "imported", occurrences[0].Outcome)
	require.Equal(t, []string{"Inbox", "Project"}, occurrences[0].Location.Labels)
	files, err := os.ReadDir(f.Spool)
	require.NoError(t, err)
	for _, file := range files {
		require.NotContains(t, file.Name(), "docbank-email-", "all message spools must close")
	}
}

func TestMailboxContinuationAtEntryEndDoesNotRespoolCommittedMessages(t *testing.T) {
	for _, format := range []string{"mbox", "zip"} {
		t.Run(format, func(t *testing.T) {
			f := mailboxFixture(t)
			raw := []byte(separator + "Subject: Committed\n\nBody\n")
			if format == "zip" {
				raw = syntheticZIP(t, "messages.mbox", 0600, string(raw))
			}
			request := queuedArchive(t, f, raw, format)
			job, err := f.Store.ClaimMailboxJob(t.Context())
			require.NoError(t, err)
			require.NoError(t, f.runJob(t.Context(), job, 1))
			partial, err := f.Store.MailboxJob(t.Context(), "one", job.ID)
			require.NoError(t, err)
			require.Equal(t, "partial", partial.State)
			if format == "zip" {
				require.Equal(t, []string{hashBytes([]byte(separator + "Subject: Committed\n\nBody\n"))}, partial.EntryHashes)
			}
			_, err = f.Store.ResumeMailboxJob(t.Context(), "one", request, true)
			require.NoError(t, err)
			job, err = f.Store.ClaimMailboxJob(t.Context())
			require.NoError(t, err)
			// The committed occurrence already identifies EOF; no new spool is
			// needed to discover it, even if message scratch storage is unavailable.
			f.Spool = filepath.Join(f.Spool, "not-a-directory")
			require.NoError(t, os.WriteFile(f.Spool, nil, 0600))
			require.NoError(t, f.RunJob(t.Context(), job))
			complete, err := f.Store.MailboxJob(t.Context(), "one", job.ID)
			require.NoError(t, err)
			require.Equal(t, "complete", complete.State)
			require.Equal(t, int64(1), complete.Imported)
			require.NoError(t, f.Store.ValidateMetadata(t.Context()))
		})
	}
}
