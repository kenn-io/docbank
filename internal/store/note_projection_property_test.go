//go:build !darwin || arm64

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
	"hegel.dev/go/hegel"
)

func TestNoteParseRecordValidationProperty(t *testing.T) {
	note, err := notemarkdown.Parse(t.Context(), []byte("# Synthetic\n"))
	require.NoError(t, err)
	artifact, err := notemarkdown.CanonicalArtifact(t.Context(), note)
	require.NoError(t, err)
	digest := sha256.Sum256(artifact)
	valid := NoteParseRecord{ContentVersionID: noteOperationID(t), Hash: note.SourceSHA256, Size: int64(note.SourceBytes), Recipe: notemarkdown.Recipe, Checksum: hex.EncodeToString(digest[:]), State: "ready", Artifact: artifact}
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.Integers(0, 9))
		record := valid
		switch kind {
		case 1:
			record.Hash = fakeHash("ab")
		case 2:
			record.Size++
		case 3:
			record.Checksum = fakeHash("cd")
		case 4:
			record.Recipe = "unknown"
		case 5:
			record.ContentVersionID = "missing-version"
		case 6:
			record.Reason = "unexpected"
		case 7:
			record.State = "pending"
		case 8:
			record.State = "failed"
			record.Reason = "invalid_markdown"
			record.Artifact = nil
			record.Checksum = ""
		case 9:
			record.State = "limit"
			record.Reason = "parser_limit"
			record.Artifact = nil
			record.Checksum = ""
		}
		err := validateNoteParse(ht.Context(), record)
		if kind == 0 || kind >= 8 {
			require.NoError(ht, err)
		} else {
			require.ErrorIs(ht, err, ErrInvalidNoteProjection)
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}
