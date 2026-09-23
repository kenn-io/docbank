package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestNativeEmailEvidenceRejectsAvailableBodyWithoutBuildIdentity(t *testing.T) {
	s := newTestStore(t)
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	_, _, err = nativeEmailBodyBuild(t.Context(), tx, s.vaultID, EmailMetadataView{BodySearch: EmailBodySearch{State: "available"}})
	require.ErrorIs(t, err, ErrEmailCorrupt)
}

func TestNativeEmailEvidenceBindsExactRootAndReceiptOccurrences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f := newEmailFixture(t, s, "direct.eml")
	direct, err := s.PublishEmailGeneration(ctx, f.publication)
	require.NoError(t, err)
	selection := NativeEmailEvidenceSelection{NodeID: direct.Version.NodeID, VersionID: direct.Version.ID, BlobSHA256: direct.Version.BlobHash, GenerationID: direct.Generation.ID}
	got, err := s.NativeEmailEvidence(ctx, "one", selection)
	require.NoError(t, err)
	require.Equal(t, "available", got.State)
	require.Equal(t, "available", got.OccurrenceState)
	require.Equal(t, "1", got.RootMessage.Path)
	require.Equal(t, "pending", got.Metadata.BodySearch.State)
	require.Empty(t, got.Occurrences)

	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "mbox-source", Owner: "one", Description: "Synthetic MBOX"}))
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "takeout-source", Owner: "one", Description: "Synthetic Gmail Takeout"}))
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "mailbox:manual", Owner: "one", Description: "Synthetic direct archive"}))
	manualUUIDArchive := "mailbox:11111111-1111-4111-8111-111111111111"
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: manualUUIDArchive, Owner: "one", Description: "Synthetic direct UUID archive"}))
	run, err := s.BeginIngest(ctx, "mailbox", "Synthetic archive")
	require.NoError(t, err)
	var archiveSelection NativeEmailEvidenceSelection
	var archiveReceiptID string
	for _, archive := range []string{"mbox-source", "takeout-source", "mailbox:manual", manualUUIDArchive} {
		var location *MailboxLocation
		if archive == "takeout-source" {
			location = &MailboxLocation{ContainerID: "direct-container", Entry: "direct.eml", EMLSHA256: direct.Version.BlobHash, EMLSize: direct.Version.Size, Labels: []string{}}
		}
		receipt, publishErr := s.PublishMailboxTransfer(ctx, MailboxTransferPublication{Owner: "one", Run: run, Request: MailboxTransferRequest{ArchiveID: archive, Reference: "1", SHA256: direct.Version.BlobHash, Size: direct.Version.Size, Settings: "synthetic-settings", DestinationID: s.RootID(), Name: "archive.eml"}, Email: f.publication, Location: location})
		require.NoError(t, publishErr)
		require.Equal(t, "direct", receipt.OriginKind)
		require.Empty(t, receipt.OccurrenceKeys)
		require.False(t, receipt.OccurrenceOverflow)
		selected := NativeEmailEvidenceSelection{NodeID: receipt.Target.NodeID, VersionID: receipt.Target.VersionID, BlobSHA256: receipt.Target.SHA256, GenerationID: direct.Generation.ID}
		archiveSelection = selected
		archiveReceiptID = receipt.ID
		item, readErr := s.NativeEmailEvidence(ctx, "one", selected)
		require.NoError(t, readErr)
		require.Equal(t, "available", item.State)
		require.Equal(t, "1", item.RootMessage.Path)
		require.Len(t, item.Occurrences, 1)
		require.Equal(t, archive, item.Occurrences[0].ArchiveID)
		require.Equal(t, receipt.ID, item.Occurrences[0].ReceiptID)
		require.Empty(t, item.Occurrences[0].JobID)
		require.Equal(t, location, item.Occurrences[0].Location)
		require.Equal(t, direct.Version.BlobHash, item.Metadata.Version.BlobHash)
		require.NotEqual(t, direct.Version.ID, item.Metadata.Version.ID)
		if archive == "mailbox:manual" {
			_, updateErr := s.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_remove(receipt_json,'$.origin_kind') WHERE id=?`, receipt.ID)
			require.NoError(t, updateErr)
			legacyManual, readErr := s.NativeEmailEvidence(ctx, "one", selected)
			require.NoError(t, readErr)
			require.Equal(t, "available", legacyManual.State)
			require.Equal(t, "available", legacyManual.OccurrenceState)
			require.Len(t, legacyManual.Occurrences, 1)
			require.Equal(t, receipt.ID, legacyManual.Occurrences[0].ReceiptID)
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_remove(receipt_json,'$.origin_kind') WHERE id=?`, archiveReceiptID)
	require.NoError(t, err)
	legacyDirect, err := s.NativeEmailEvidence(ctx, "one", archiveSelection)
	require.NoError(t, err)
	require.Len(t, legacyDirect.Occurrences, 1)
	require.Equal(t, archiveReceiptID, legacyDirect.Occurrences[0].ReceiptID)
	otherOwnerArchive, err := s.NativeEmailEvidence(ctx, "other", archiveSelection)
	require.NoError(t, err)
	require.Equal(t, "available", otherOwnerArchive.State)
	require.Equal(t, direct.Generation.ID, otherOwnerArchive.Metadata.Generation.ID)
	require.Equal(t, "1", otherOwnerArchive.RootMessage.Path)
	require.Empty(t, otherOwnerArchive.Occurrences)

	wrongOwner, err := s.NativeEmailEvidence(ctx, "other", selection)
	require.NoError(t, err)
	require.Empty(t, wrongOwner.Occurrences)
	wrongHash := selection
	wrongHash.BlobSHA256 = fakeHash("aa")
	stale, err := s.NativeEmailEvidence(ctx, "one", wrongHash)
	require.NoError(t, err)
	require.Equal(t, "unavailable", stale.State)
	require.Equal(t, "unavailable", stale.OccurrenceState)
	require.Equal(t, "snapshot_unavailable", stale.OccurrenceReason)
	require.Nil(t, stale.Occurrences)
	wrongGeneration := selection
	wrongGeneration.GenerationID = fakeHash("bb")
	stale, err = s.NativeEmailEvidence(ctx, "one", wrongGeneration)
	require.NoError(t, err)
	require.Equal(t, "unavailable", stale.State)
	missing, err := s.NativeEmailEvidence(ctx, "one", NativeEmailEvidenceSelection{NodeID: selection.NodeID, VersionID: "missing", BlobSHA256: selection.BlobSHA256, GenerationID: selection.GenerationID})
	require.NoError(t, err)
	require.Equal(t, "missing", missing.State)
	require.Equal(t, "unavailable", missing.OccurrenceState)
	require.Equal(t, "snapshot_unavailable", missing.OccurrenceReason)
	node, err := s.NodeByID(ctx, selection.NodeID)
	require.NoError(t, err)
	_, changed, err := s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("cc"), 3, "message/rfc822")
	require.NoError(t, err)
	changedSelection := NativeEmailEvidenceSelection{NodeID: node.ID, VersionID: changed.ID, BlobSHA256: changed.BlobHash, GenerationID: direct.Generation.ID}
	stale, err = s.NativeEmailEvidence(ctx, "one", changedSelection)
	require.NoError(t, err)
	require.Equal(t, "unavailable", stale.State)
	historical, err := s.NativeEmailEvidence(ctx, "one", selection)
	require.NoError(t, err)
	require.Equal(t, "available", historical.State, "retained exact old version remains readable until withdrawal")
	_, _, err = s.Trash(ctx, selection.NodeID, UnconditionalRev)
	require.NoError(t, err)
	withdrawn, err := s.NativeEmailEvidence(ctx, "one", selection)
	require.NoError(t, err)
	require.Equal(t, "withdrawn", withdrawn.State)
	require.Equal(t, "unavailable", withdrawn.OccurrenceState)
	require.Equal(t, "snapshot_unavailable", withdrawn.OccurrenceReason)
	require.Nil(t, withdrawn.Occurrences)

	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	again, err := restored.NativeEmailEvidence(ctx, "one", selection)
	require.NoError(t, err)
	require.Equal(t, "withdrawn", again.State)
	require.Equal(t, "unavailable", again.OccurrenceState)
	require.Equal(t, "snapshot_unavailable", again.OccurrenceReason)
	restoredArchive, err := restored.NativeEmailEvidence(ctx, "one", archiveSelection)
	require.NoError(t, err)
	require.Equal(t, "available", restoredArchive.State)
	require.Len(t, restoredArchive.Occurrences, 1)
	_, err = s.db.Exec(`UPDATE email_generations SET checksum=? WHERE generation_id=?`, fakeHash("dd"), direct.Generation.ID)
	require.NoError(t, err)
	_, err = s.NativeEmailEvidence(ctx, "one", archiveSelection)
	require.ErrorIs(t, err, ErrEmailCorrupt)
}

func TestNativeEmailEvidenceRejectsReceiptTargetDrift(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f := newEmailFixture(t, s, "target.eml")
	view, err := s.PublishEmailGeneration(ctx, f.publication)
	require.NoError(t, err)
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "target-archive", Owner: "one", Description: "Synthetic target archive"}))
	run, err := s.BeginIngest(ctx, "mailbox", "Synthetic target drift")
	require.NoError(t, err)
	receipt, err := s.PublishMailboxTransfer(ctx, MailboxTransferPublication{Owner: "one", Run: run, Request: MailboxTransferRequest{ArchiveID: "target-archive", Reference: "1", SHA256: view.Version.BlobHash, Size: view.Version.Size, Settings: "synthetic-settings", DestinationID: s.RootID(), Name: "target.eml"}, Email: f.publication})
	require.NoError(t, err)
	metadata, err := s.EmailMetadata(ctx, receipt.Target.VersionID)
	require.NoError(t, err)
	selected := NativeEmailEvidenceSelection{NodeID: receipt.Target.NodeID, VersionID: receipt.Target.VersionID, BlobSHA256: receipt.Target.SHA256, GenerationID: metadata.Generation.ID}
	node, err := s.NodeByID(ctx, receipt.Target.NodeID)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	for _, tc := range []struct {
		name   string
		mutate func(*MailboxTransferReceipt)
	}{
		{name: "other node", mutate: func(r *MailboxTransferReceipt) { r.Target.NodeID += 1000 }},
		{name: "other hash", mutate: func(r *MailboxTransferReceipt) { r.Target.SHA256 = fakeHash("ab"); r.Request.SHA256 = r.Target.SHA256 }},
		{name: "other size", mutate: func(r *MailboxTransferReceipt) { r.Target.Size++; r.Request.Size = r.Target.Size }},
		{name: "future target revision", mutate: func(r *MailboxTransferReceipt) { r.TargetRevision = node.Revision + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corrupt := newTestStore(t)
			require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
			changed := receipt
			tc.mutate(&changed)
			changed.RequestDigest, err = mailboxTransferDigest(changed.Request)
			require.NoError(t, err)
			raw, marshalErr := json.Marshal(changed)
			require.NoError(t, marshalErr)
			_, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=? WHERE id=?`, raw, receipt.ID)
			require.NoError(t, updateErr)
			_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
			require.ErrorIs(t, readErr, ErrMailboxInvalid)
		})
	}
}

func TestNativeEmailEvidenceSelectsOnlyTopLevelNestedMessage(t *testing.T) {
	s := newTestStore(t)
	raw := "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nouter\r\n--m\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment; filename=forward.eml\r\n\r\nSubject: Forwarded\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<i>inner</i>\r\n--m--\r\n"
	f := newEmailSourceFixture(t, s, "nested.eml", raw)
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	require.Greater(t, len(view.Evidence.Inventory.Messages), 1)
	selected := NativeEmailEvidenceSelection{NodeID: view.Version.NodeID, VersionID: view.Version.ID, BlobSHA256: view.Version.BlobHash, GenerationID: view.Generation.ID}
	got, err := s.NativeEmailEvidence(t.Context(), "one", selected)
	require.NoError(t, err)
	require.Equal(t, "available", got.State)
	require.Equal(t, "1", got.RootMessage.Path)
	require.NotEqual(t, got.RootMessage.Path, view.Evidence.Inventory.Messages[1].Path)
}

func TestNativeEmailEvidenceDoesNotPresentPartialRootAsComplete(t *testing.T) {
	s := newTestStore(t)
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody"
	f := newEmailSourceFixture(t, s, "partial.eml", raw)
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	require.Equal(t, document.EmailInventoryPartial, view.Evidence.Inventory.State)
	require.Equal(t, new("1"), view.Evidence.Inventory.RootPath)
	require.NotEmpty(t, view.Evidence.Inventory.Messages)
	require.Equal(t, "1", view.Evidence.Inventory.Messages[0].Path)
	selected := NativeEmailEvidenceSelection{NodeID: view.Version.NodeID, VersionID: view.Version.ID, BlobSHA256: view.Version.BlobHash, GenerationID: view.Generation.ID}
	got, err := s.NativeEmailEvidence(t.Context(), "one", selected)
	require.NoError(t, err)
	require.Equal(t, "unavailable", got.State)
	require.Equal(t, "incomplete_inventory", got.Reason)
	require.Equal(t, "unavailable", got.OccurrenceState)
	require.Equal(t, "snapshot_unavailable", got.OccurrenceReason)
	require.Equal(t, view.Generation.ID, got.Metadata.Generation.ID)
	require.Equal(t, view.Generation.Checksum, got.Metadata.Generation.Checksum)
}

func TestNativeEmailEvidenceKeepsSupersededGenerationButMarksBodyUnserved(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "message.eml")
	first, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	changed := first.Evidence
	changed.Recipe.GoVersion += "-synthetic-reprocessing"
	reprocessed := f.publication
	reprocessed.CanonicalJSON, _, err = document.MarshalEmailV1(changed)
	require.NoError(t, err)
	_, err = s.PublishEmailGeneration(t.Context(), reprocessed)
	require.NoError(t, err)
	selected := NativeEmailEvidenceSelection{NodeID: first.Version.NodeID, VersionID: first.Version.ID, BlobSHA256: first.Version.BlobHash, GenerationID: first.Generation.ID}
	got, err := s.NativeEmailEvidence(t.Context(), "one", selected)
	require.NoError(t, err)
	require.Equal(t, "available", got.State)
	require.Equal(t, first.Generation.ID, got.Metadata.Generation.ID)
	require.Equal(t, "unavailable", got.Metadata.BodySearch.State)
	require.Equal(t, new("superseded"), got.Metadata.BodySearch.Reason)
	require.Nil(t, got.BodyBuild)
}

func TestNativeEmailEvidenceRetainsJobOrdinalAcrossRestore(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f := newEmailFixture(t, s, "seed.eml")
	source, err := s.ContentVersionByID(ctx, f.publication.ContentVersionID)
	require.NoError(t, err)
	container := MailboxContainerRequest{ID: "mbox", Owner: "one", SHA256: source.BlobHash, Size: source.Size, Format: "mbox"}
	_, err = s.BeginMailboxContainer(ctx, container)
	require.NoError(t, err)
	require.NoError(t, s.PutMailboxChunk(ctx, container.Owner, container.ID, MailboxChunk{Index: 0, SHA256: container.SHA256, Size: container.Size}))
	_, err = s.SealMailboxContainer(ctx, container.Owner, container.ID, container.SHA256, container.Size)
	require.NoError(t, err)
	_, err = s.BeginMailboxJob(ctx, "one", MailboxJobRequest{ID: "job", ContainerID: container.ID, ContainerSHA256: container.SHA256, Settings: MailboxSettings{DestinationID: s.RootID()}})
	require.NoError(t, err)
	job, err := s.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	archiveID := "mailbox:" + job.CollectionID
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: archiveID, Owner: "one", Description: "Synthetic MBOX"}))
	settings, err := job.Settings.Canonical()
	require.NoError(t, err)
	location := MailboxLocation{ContainerID: container.ID, Entry: "source.mbox", EntrySHA256: source.BlobHash, Sequence: 1, End: source.Size, RawSHA256: source.BlobHash, EMLSHA256: source.BlobHash, EMLSize: source.Size, Separator: "From synthetic", Labels: []string{}}
	occurrence := MailboxOccurrence{JobID: job.ID, Ordinal: 1, Location: location}
	require.NoError(t, s.StartMailboxOccurrence(ctx, job.ID, job.Claim, occurrence))
	publication := MailboxTransferPublication{Owner: "one", Request: MailboxTransferRequest{ArchiveID: archiveID, Reference: "0:1", SHA256: source.BlobHash, Size: source.Size, Settings: settings, DestinationID: s.RootID(), Name: "message.eml"}, Email: f.publication, Location: &location}
	require.NoError(t, s.CommitMailboxOccurrence(ctx, job.ID, job.Claim, occurrence, &publication))
	receipt, err := s.MailboxTransfer(ctx, "one", archiveID, "0:1")
	require.NoError(t, err)
	require.Equal(t, "job", receipt.OriginKind)
	require.Equal(t, []MailboxReceiptOccurrenceKey{{JobID: job.ID, Ordinal: 1}}, receipt.OccurrenceKeys)
	require.False(t, receipt.OccurrenceOverflow)
	var initialBindings int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM provenance_version_bindings WHERE content_version_id=?`, receipt.Target.VersionID).Scan(&initialBindings))
	require.Positive(t, initialBindings)
	directRun, err := s.BeginIngest(ctx, "mailbox", "Synthetic direct retry")
	require.NoError(t, err)
	_, err = s.PublishMailboxTransfer(ctx, MailboxTransferPublication{Owner: "one", Run: directRun, Request: receipt.Request, Email: f.publication})
	require.ErrorIs(t, err, ErrMailboxConflict)
	view, err := s.EmailMetadata(ctx, receipt.Target.VersionID)
	require.NoError(t, err)
	selected := NativeEmailEvidenceSelection{NodeID: receipt.Target.NodeID, VersionID: receipt.Target.VersionID, BlobSHA256: receipt.Target.SHA256, GenerationID: view.Generation.ID}
	got, err := s.NativeEmailEvidence(ctx, "one", selected)
	require.NoError(t, err)
	require.Equal(t, "available", got.State)
	require.Equal(t, "available", got.OccurrenceState)
	require.Len(t, got.Occurrences, 1)
	require.Equal(t, archiveID, got.Occurrences[0].ArchiveID)
	require.Equal(t, job.ID, got.Occurrences[0].JobID)
	require.Equal(t, int64(1), got.Occurrences[0].Ordinal)
	require.Equal(t, receipt.ID, got.Occurrences[0].ReceiptID)
	wrongOwner, err := s.NativeEmailEvidence(ctx, "other", selected)
	require.NoError(t, err)
	require.Equal(t, "available", wrongOwner.State)
	require.Empty(t, wrongOwner.Occurrences)

	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	restoredReceipt, err := restored.MailboxTransfer(ctx, "one", archiveID, "0:1")
	require.NoError(t, err)
	require.Equal(t, "job", restoredReceipt.OriginKind)
	require.Equal(t, receipt.OccurrenceKeys, restoredReceipt.OccurrenceKeys)
	again, err := restored.NativeEmailEvidence(ctx, "one", selected)
	require.NoError(t, err)
	require.Equal(t, got.Occurrences, again.Occurrences)
	t.Run("direct replacement does not inherit job origin", func(t *testing.T) {
		updated := newTestStore(t)
		require.NoError(t, updated.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		newSource := newEmailSourceFixture(t, updated, "replacement.eml", strings.Replace(catalogEmailSource, "Chosen synthetic body.", "Replaced synthetic body.", 1))
		newVersion, versionErr := updated.ContentVersionByID(ctx, newSource.publication.ContentVersionID)
		require.NoError(t, versionErr)
		run, beginErr := updated.BeginIngest(ctx, "mailbox", "Synthetic direct replacement")
		require.NoError(t, beginErr)
		request := receipt.Request
		request.SHA256, request.Size = newVersion.BlobHash, newVersion.Size
		request.ExpectedRevision = &receipt.TargetRevision
		replaced, publishErr := updated.PublishMailboxTransfer(ctx, MailboxTransferPublication{Owner: "one", Run: run, Request: request, Email: newSource.publication})
		require.NoError(t, publishErr)
		require.Equal(t, "direct", replaced.OriginKind)
		require.Empty(t, replaced.OccurrenceKeys)
		var bindings int
		require.NoError(t, updated.db.QueryRowContext(ctx, `SELECT count(*) FROM provenance_version_bindings WHERE content_version_id=?`, replaced.Target.VersionID).Scan(&bindings))
		require.Zero(t, bindings)
		newMetadata, metadataErr := updated.EmailMetadata(ctx, replaced.Target.VersionID)
		require.NoError(t, metadataErr)
		newSelection := NativeEmailEvidenceSelection{NodeID: replaced.Target.NodeID, VersionID: replaced.Target.VersionID, BlobSHA256: replaced.Target.SHA256, GenerationID: newMetadata.Generation.ID}
		evidence, readErr := updated.NativeEmailEvidence(ctx, "one", newSelection)
		require.NoError(t, readErr)
		require.Equal(t, "available", evidence.OccurrenceState)
		require.Len(t, evidence.Occurrences, 1)
		require.Equal(t, replaced.ID, evidence.Occurrences[0].ReceiptID)
		require.Empty(t, evidence.Occurrences[0].JobID)
		_, updateErr := updated.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_remove(receipt_json,'$.origin_kind') WHERE id=?`, replaced.ID)
		require.NoError(t, updateErr)
		legacy, readErr := updated.NativeEmailEvidence(ctx, "one", newSelection)
		require.NoError(t, readErr)
		require.Equal(t, "available", legacy.State)
		require.Equal(t, "unavailable", legacy.OccurrenceState)
		require.Equal(t, "legacy_occurrence_unproven", legacy.OccurrenceReason)
	})
	t.Run("job replacement without version binding retains job origin", func(t *testing.T) {
		updated := s
		newSource := newEmailSourceFixture(t, updated, "replacement.eml", strings.Replace(catalogEmailSource, "Chosen synthetic body.", "Replaced synthetic body.", 1))
		newVersion, versionErr := updated.ContentVersionByID(ctx, newSource.publication.ContentVersionID)
		require.NoError(t, versionErr)
		newLocation := location
		newLocation.Start = source.Size
		newLocation.End = source.Size + newVersion.Size
		newLocation.RawSHA256 = newVersion.BlobHash
		newLocation.EMLSHA256 = newVersion.BlobHash
		newLocation.EMLSize = newVersion.Size
		newOccurrence := MailboxOccurrence{JobID: job.ID, Ordinal: 2, Location: newLocation}
		require.NoError(t, updated.StartMailboxOccurrence(ctx, job.ID, job.Claim, newOccurrence))
		request := receipt.Request
		request.SHA256, request.Size = newVersion.BlobHash, newVersion.Size
		request.ExpectedRevision = &receipt.TargetRevision
		publication := MailboxTransferPublication{Owner: "one", Request: request, Email: newSource.publication, Location: &newLocation}
		require.NoError(t, updated.CommitMailboxOccurrence(ctx, job.ID, job.Claim, newOccurrence, &publication))
		replaced, receiptErr := updated.MailboxTransfer(ctx, "one", archiveID, request.Reference)
		require.NoError(t, receiptErr)
		require.Equal(t, "job", replaced.OriginKind)
		require.Equal(t, []MailboxReceiptOccurrenceKey{{JobID: job.ID, Ordinal: 2}}, replaced.OccurrenceKeys)
		var bindings int
		require.NoError(t, updated.db.QueryRowContext(ctx, `SELECT count(*) FROM provenance_version_bindings WHERE content_version_id=?`, replaced.Target.VersionID).Scan(&bindings))
		require.Zero(t, bindings)
		newMetadata, metadataErr := updated.EmailMetadata(ctx, replaced.Target.VersionID)
		require.NoError(t, metadataErr)
		newSelection := NativeEmailEvidenceSelection{NodeID: replaced.Target.NodeID, VersionID: replaced.Target.VersionID, BlobSHA256: replaced.Target.SHA256, GenerationID: newMetadata.Generation.ID}
		_, deleteErr := updated.db.ExecContext(ctx, `DELETE FROM mailbox_occurrences WHERE receipt_id=?`, replaced.ID)
		require.NoError(t, deleteErr)
		_, readErr := updated.NativeEmailEvidence(ctx, "one", newSelection)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
		_, updateErr := updated.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_remove(receipt_json,'$.origin_kind','$.occurrence_keys','$.occurrence_overflow') WHERE id=?`, replaced.ID)
		require.NoError(t, updateErr)
		legacy, readErr := updated.NativeEmailEvidence(ctx, "one", newSelection)
		require.NoError(t, readErr)
		require.Equal(t, "available", legacy.State)
		require.Equal(t, "unavailable", legacy.OccurrenceState)
		require.Equal(t, "legacy_occurrence_unproven", legacy.OccurrenceReason)
	})
	t.Run("legacy receipt without origin survives backup and fails closed on broken link", func(t *testing.T) {
		legacy := newTestStore(t)
		require.NoError(t, legacy.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		_, updateErr := legacy.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_remove(receipt_json,'$.origin_kind','$.occurrence_keys','$.occurrence_overflow') WHERE id=?`, receipt.ID)
		require.NoError(t, updateErr)
		var oldBackup bytes.Buffer
		require.NoError(t, legacy.ExportMetadata(ctx, &oldBackup))
		oldRestored := newTestStore(t)
		require.NoError(t, oldRestored.ImportMetadata(ctx, bytes.NewReader(oldBackup.Bytes())))
		oldReceipt, receiptErr := oldRestored.MailboxTransfer(ctx, "one", archiveID, "0:1")
		require.NoError(t, receiptErr)
		require.Empty(t, oldReceipt.OriginKind)
		valid, readErr := oldRestored.NativeEmailEvidence(ctx, "one", selected)
		require.NoError(t, readErr)
		require.Equal(t, "available", valid.State)
		require.Equal(t, "unavailable", valid.OccurrenceState)
		require.Equal(t, "legacy_occurrence_unproven", valid.OccurrenceReason)
		require.Empty(t, valid.Occurrences)
		_, deleteErr := oldRestored.db.ExecContext(ctx, `DELETE FROM mailbox_occurrences WHERE receipt_id=?`, receipt.ID)
		require.NoError(t, deleteErr)
		withoutLink, readErr := oldRestored.NativeEmailEvidence(ctx, "one", selected)
		require.NoError(t, readErr)
		require.Equal(t, "available", withoutLink.State)
		require.Equal(t, "unavailable", withoutLink.OccurrenceState)
		require.Equal(t, "legacy_occurrence_unproven", withoutLink.OccurrenceReason)
	})
	t.Run("missing mailbox occurrence row", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		changed, deleteErr := corrupt.db.ExecContext(ctx, `DELETE FROM mailbox_occurrences WHERE receipt_id=?`, receipt.ID)
		require.NoError(t, deleteErr)
		count, countErr := changed.RowsAffected()
		require.NoError(t, countErr)
		require.Equal(t, int64(1), count)
		_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
	})
	t.Run("normalized receipt link points elsewhere", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		require.NoError(t, corrupt.RegisterMailboxArchive(ctx, MailboxArchive{ID: "decoy-archive", Owner: "one", Description: "Synthetic unrelated EML"}))
		run, beginErr := corrupt.BeginIngest(ctx, "mailbox", "Synthetic unrelated EML")
		require.NoError(t, beginErr)
		decoy, publishErr := corrupt.PublishMailboxTransfer(ctx, MailboxTransferPublication{Owner: "one", Run: run, Request: MailboxTransferRequest{ArchiveID: "decoy-archive", Reference: "1", SHA256: source.BlobHash, Size: source.Size, Settings: "synthetic-settings", DestinationID: corrupt.RootID(), Name: "decoy.eml"}, Email: f.publication})
		require.NoError(t, publishErr)
		changed, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_occurrences SET receipt_id=? WHERE receipt_id=?`, decoy.ID, receipt.ID)
		require.NoError(t, updateErr)
		count, countErr := changed.RowsAffected()
		require.NoError(t, countErr)
		require.Equal(t, int64(1), count)
		_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
	})
	t.Run("one linked occurrence cannot hide a mismatched second row", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		require.NoError(t, corrupt.ResetMailboxClaims(ctx))
		activeJob, claimErr := corrupt.ClaimMailboxJob(ctx)
		require.NoError(t, claimErr)
		require.Equal(t, job.ID, activeJob.ID)
		second := MailboxOccurrence{JobID: job.ID, Ordinal: 2, Location: location}
		require.NoError(t, corrupt.StartMailboxOccurrence(ctx, job.ID, activeJob.Claim, second))
		secondPublication := MailboxTransferPublication{Owner: "one", Request: receipt.Request, Email: f.publication, Location: &location}
		require.NoError(t, corrupt.CommitMailboxOccurrence(ctx, job.ID, activeJob.Claim, second, &secondPublication))
		linkedReceipt, receiptErr := corrupt.MailboxTransfer(ctx, "one", archiveID, receipt.Request.Reference)
		require.NoError(t, receiptErr)
		require.Equal(t, []MailboxReceiptOccurrenceKey{{JobID: job.ID, Ordinal: 1}, {JobID: job.ID, Ordinal: 2}}, linkedReceipt.OccurrenceKeys)
		complete, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.NoError(t, readErr)
		require.Len(t, complete.Occurrences, 2)
		require.NoError(t, corrupt.RegisterMailboxArchive(ctx, MailboxArchive{ID: "decoy-archive", Owner: "one", Description: "Synthetic unrelated EML"}))
		run, beginErr := corrupt.BeginIngest(ctx, "mailbox", "Synthetic unrelated EML")
		require.NoError(t, beginErr)
		decoy, publishErr := corrupt.PublishMailboxTransfer(ctx, MailboxTransferPublication{Owner: "one", Run: run, Request: MailboxTransferRequest{ArchiveID: "decoy-archive", Reference: "1", SHA256: source.BlobHash, Size: source.Size, Settings: "synthetic-settings", DestinationID: corrupt.RootID(), Name: "decoy.eml"}, Email: f.publication})
		require.NoError(t, publishErr)
		_, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_occurrences SET receipt_id=? WHERE job_id=? AND ordinal=2`, decoy.ID, job.ID)
		require.NoError(t, updateErr)
		_, readErr = corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid, "a mismatched second row must not truncate selected occurrence coverage")
		decoyMetadata, metadataErr := corrupt.EmailMetadata(ctx, decoy.Target.VersionID)
		require.NoError(t, metadataErr)
		decoySelection := NativeEmailEvidenceSelection{NodeID: decoy.Target.NodeID, VersionID: decoy.Target.VersionID, BlobSHA256: decoy.Target.SHA256, GenerationID: decoyMetadata.Generation.ID}
		_, readErr = corrupt.NativeEmailEvidence(ctx, "one", decoySelection)
		require.ErrorIs(t, readErr, ErrMailboxInvalid, "the normalized owner of the row detects its conflicting JSON receipt")
	})
	for _, tc := range []struct {
		name          string
		jsonReceiptID string
	}{
		{name: "extra linked occurrence", jsonReceiptID: receipt.ID},
		{name: "extra normalized link conflicts with JSON", jsonReceiptID: "11111111-1111-4111-8111-111111111111"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corrupt := newTestStore(t)
			require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
			extra := occurrence
			extra.Ordinal = 2
			extra.ReceiptID = tc.jsonReceiptID
			extra.Target = &receipt.Target
			extra.Outcome = "imported"
			raw, marshalErr := json.Marshal(extra)
			require.NoError(t, marshalErr)
			_, insertErr := corrupt.db.ExecContext(ctx, `INSERT INTO mailbox_occurrences(job_id,ordinal,receipt_id,occurrence_json) VALUES(?,?,?,?)`, job.ID, extra.Ordinal, receipt.ID, raw)
			require.NoError(t, insertErr)
			_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
			require.ErrorIs(t, readErr, ErrMailboxInvalid, "a marked receipt must prove its complete normalized link set")
			_, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_remove(receipt_json,'$.origin_kind','$.occurrence_keys','$.occurrence_overflow') WHERE id=?`, receipt.ID)
			require.NoError(t, updateErr)
			legacy, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
			require.NoError(t, readErr)
			require.Equal(t, "available", legacy.State)
			require.Equal(t, "unavailable", legacy.OccurrenceState)
			require.Equal(t, "legacy_occurrence_unproven", legacy.OccurrenceReason)
		})
	}
	t.Run("marked job keeps a bounded exact key list and overflows without blocking import", func(t *testing.T) {
		large := newTestStore(t)
		require.NoError(t, large.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		tx, beginErr := large.db.BeginTx(ctx, nil)
		require.NoError(t, beginErr)
		largeReceipt := receipt
		for ordinal := int64(2); ordinal <= 1000; ordinal++ {
			linked := occurrence
			linked.Ordinal = ordinal
			linked.ReceiptID = receipt.ID
			linked.Target = &receipt.Target
			linked.Outcome = "imported"
			raw, marshalErr := json.Marshal(linked)
			require.NoError(t, marshalErr)
			_, insertErr := tx.ExecContext(ctx, `INSERT INTO mailbox_occurrences(job_id,ordinal,receipt_id,occurrence_json) VALUES(?,?,?,?)`, job.ID, ordinal, receipt.ID, raw)
			require.NoError(t, insertErr)
			largeReceipt.OccurrenceKeys = append(largeReceipt.OccurrenceKeys, MailboxReceiptOccurrenceKey{JobID: job.ID, Ordinal: ordinal})
		}
		raw, marshalErr := json.Marshal(largeReceipt)
		require.NoError(t, marshalErr)
		_, updateErr := tx.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=? WHERE id=?`, raw, receipt.ID)
		require.NoError(t, updateErr)
		largeJob := job
		largeJob.Checkpoint = 1000
		largeJob.Imported = 1000
		require.NoError(t, saveMailboxJob(ctx, tx, largeJob, false))
		require.NoError(t, tx.Commit())
		atLimit, readErr := large.NativeEmailEvidence(ctx, "one", selected)
		require.NoError(t, readErr)
		require.Equal(t, "available", atLimit.OccurrenceState)
		require.Len(t, atLimit.Occurrences, 1000)
		require.NoError(t, large.ResetMailboxClaims(ctx))
		activeJob, claimErr := large.ClaimMailboxJob(ctx)
		require.NoError(t, claimErr)
		require.Equal(t, job.ID, activeJob.ID)
		last := MailboxOccurrence{JobID: job.ID, Ordinal: 1001, Location: location}
		require.NoError(t, large.StartMailboxOccurrence(ctx, job.ID, activeJob.Claim, last))
		lastPublication := MailboxTransferPublication{Owner: "one", Request: receipt.Request, Email: f.publication, Location: &location}
		require.NoError(t, large.CommitMailboxOccurrence(ctx, job.ID, activeJob.Claim, last, &lastPublication))
		overflowReceipt, receiptErr := large.MailboxTransfer(ctx, "one", archiveID, receipt.Request.Reference)
		require.NoError(t, receiptErr)
		require.Len(t, overflowReceipt.OccurrenceKeys, 1000)
		require.True(t, overflowReceipt.OccurrenceOverflow)
		require.Equal(t, receipt.RequestDigest, overflowReceipt.RequestDigest)
		require.Equal(t, receipt.Target, overflowReceipt.Target)
		overLimit, readErr := large.NativeEmailEvidence(ctx, "one", selected)
		require.NoError(t, readErr)
		require.Equal(t, "available", overLimit.State)
		require.Equal(t, "unavailable", overLimit.OccurrenceState)
		require.Equal(t, "occurrence_limit", overLimit.OccurrenceReason)
		require.Empty(t, overLimit.Occurrences)
		var overflowBackup bytes.Buffer
		require.NoError(t, large.ExportMetadata(ctx, &overflowBackup))
		again := newTestStore(t)
		require.NoError(t, again.ImportMetadata(ctx, bytes.NewReader(overflowBackup.Bytes())))
		restoredOverflow, receiptErr := again.MailboxTransfer(ctx, "one", archiveID, receipt.Request.Reference)
		require.NoError(t, receiptErr)
		require.Equal(t, overflowReceipt.OccurrenceKeys, restoredOverflow.OccurrenceKeys)
		require.True(t, restoredOverflow.OccurrenceOverflow)
		restoredEvidence, readErr := again.NativeEmailEvidence(ctx, "one", selected)
		require.NoError(t, readErr)
		require.Equal(t, "available", restoredEvidence.State)
		require.Equal(t, "occurrence_limit", restoredEvidence.OccurrenceReason)
	})
	t.Run("normalized receipt link disagrees with occurrence JSON", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		_, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_occurrences SET occurrence_json=json_set(occurrence_json,'$.receipt_id','11111111-1111-4111-8111-111111111111') WHERE receipt_id=?`, receipt.ID)
		require.NoError(t, updateErr)
		_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
	})
	t.Run("unknown receipt origin", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		_, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_set(receipt_json,'$.origin_kind','unknown') WHERE id=?`, receipt.ID)
		require.NoError(t, updateErr)
		_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
	})
	t.Run("direct receipt marker cannot claim a linked job occurrence", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		_, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_set(receipt_json,'$.origin_kind','direct') WHERE id=?`, receipt.ID)
		require.NoError(t, updateErr)
		_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
	})
	t.Run("direct receipt marker cannot hide a lost job occurrence", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		_, deleteErr := corrupt.db.ExecContext(ctx, `DELETE FROM mailbox_occurrences WHERE receipt_id=?`, receipt.ID)
		require.NoError(t, deleteErr)
		_, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET receipt_json=json_set(receipt_json,'$.origin_kind','direct') WHERE id=?`, receipt.ID)
		require.NoError(t, updateErr)
		_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
	})
	for _, tc := range []struct {
		name   string
		mutate func(*MailboxTransferReceipt)
	}{
		{name: "missing receipt location", mutate: func(r *MailboxTransferReceipt) { r.Location = nil }},
		{name: "changed receipt settings", mutate: func(r *MailboxTransferReceipt) { r.Request.Settings = "other-settings" }},
		{name: "changed receipt reference", mutate: func(r *MailboxTransferReceipt) { r.Request.Reference = "99:99" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corrupt := newTestStore(t)
			require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
			_, deleteErr := corrupt.db.ExecContext(ctx, `DELETE FROM mailbox_occurrences WHERE receipt_id=?`, receipt.ID)
			require.NoError(t, deleteErr)
			broken := receipt
			tc.mutate(&broken)
			digest, digestErr := mailboxTransferDigest(broken.Request)
			require.NoError(t, digestErr)
			broken.RequestDigest = digest
			raw, marshalErr := json.Marshal(broken)
			require.NoError(t, marshalErr)
			_, updateErr := corrupt.db.ExecContext(ctx, `UPDATE mailbox_transfer_receipts SET source_ref=?,receipt_json=? WHERE id=?`, broken.Request.Reference, raw, receipt.ID)
			require.NoError(t, updateErr)
			_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
			require.ErrorIs(t, readErr, ErrMailboxInvalid)
		})
	}
	t.Run("missing mailbox job and occurrence rows", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		_, deleteErr := corrupt.db.ExecContext(ctx, `DELETE FROM mailbox_occurrences WHERE receipt_id=?`, receipt.ID)
		require.NoError(t, deleteErr)
		_, deleteErr = corrupt.db.ExecContext(ctx, `DELETE FROM mailbox_jobs WHERE id=?`, job.ID)
		require.NoError(t, deleteErr)
		_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
	})
	t.Run("missing mailbox container job and occurrence rows", func(t *testing.T) {
		corrupt := newTestStore(t)
		require.NoError(t, corrupt.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		_, deleteErr := corrupt.db.ExecContext(ctx, `DELETE FROM mailbox_occurrences WHERE receipt_id=?`, receipt.ID)
		require.NoError(t, deleteErr)
		_, deleteErr = corrupt.db.ExecContext(ctx, `DELETE FROM mailbox_jobs WHERE id=?`, job.ID)
		require.NoError(t, deleteErr)
		_, deleteErr = corrupt.db.ExecContext(ctx, `DELETE FROM mailbox_containers WHERE id=?`, container.ID)
		require.NoError(t, deleteErr)
		_, readErr := corrupt.NativeEmailEvidence(ctx, "one", selected)
		require.ErrorIs(t, readErr, ErrMailboxInvalid)
	})
	_, err = restored.db.Exec(`UPDATE mailbox_jobs SET job_json=? WHERE id=?`, strings.Repeat("x", 128<<10+1), job.ID)
	require.NoError(t, err)
	_, err = restored.NativeEmailEvidence(ctx, "one", selected)
	require.ErrorIs(t, err, ErrMailboxLimit)
	require.NotErrorIs(t, err, ErrMailboxInvalid)
}

func TestNativeEmailEvidenceOccurrenceLimitDoesNotTruncateAvailableCount(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f := newEmailFixture(t, s, "seed.eml")
	view, err := s.PublishEmailGeneration(ctx, f.publication)
	require.NoError(t, err)
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "synthetic-archive", Owner: "one", Description: "Synthetic archive"}))
	run, err := s.BeginIngest(ctx, "mailbox", "Synthetic occurrence boundary")
	require.NoError(t, err)
	receipt, err := s.PublishMailboxTransfer(ctx, MailboxTransferPublication{Owner: "one", Run: run, Request: MailboxTransferRequest{ArchiveID: "synthetic-archive", Reference: "0", SHA256: view.Version.BlobHash, Size: view.Version.Size, Settings: "synthetic-settings", DestinationID: s.RootID(), Name: "seed.eml"}, Email: f.publication})
	require.NoError(t, err)
	selected := NativeEmailEvidenceSelection{NodeID: receipt.Target.NodeID, VersionID: receipt.Target.VersionID, BlobSHA256: receipt.Target.SHA256, GenerationID: view.Generation.ID}
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for i := 1; i < 1000; i++ {
		clone := receipt
		clone.ID = fmt.Sprintf("synthetic-receipt-%04d", i)
		clone.Request.Reference = strconv.Itoa(i)
		clone.RequestDigest, err = mailboxTransferDigest(clone.Request)
		require.NoError(t, err)
		raw, marshalErr := json.Marshal(clone)
		require.NoError(t, marshalErr)
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_transfer_receipts(id,archive_id,source_ref,target_version_id,document_publication_id,receipt_json) VALUES(?,?,?,?,?,?)`, clone.ID, clone.Request.ArchiveID, clone.Request.Reference, clone.Target.VersionID, clone.DocumentPublicationID, raw)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	atLimit, err := s.NativeEmailEvidence(ctx, "one", selected)
	require.NoError(t, err)
	require.Equal(t, "available", atLimit.State)
	require.Equal(t, "available", atLimit.OccurrenceState)
	require.Empty(t, atLimit.OccurrenceReason)
	require.Len(t, atLimit.Occurrences, 1000)
	last := receipt
	last.ID = "synthetic-receipt-1000"
	last.Request.Reference = "1000"
	last.RequestDigest, err = mailboxTransferDigest(last.Request)
	require.NoError(t, err)
	raw, err := json.Marshal(last)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO mailbox_transfer_receipts(id,archive_id,source_ref,target_version_id,document_publication_id,receipt_json) VALUES(?,?,?,?,?,?)`, last.ID, last.Request.ArchiveID, last.Request.Reference, last.Target.VersionID, last.DocumentPublicationID, raw)
	require.NoError(t, err)
	overLimit, err := s.NativeEmailEvidence(ctx, "one", selected)
	require.NoError(t, err)
	require.Equal(t, "available", overLimit.State)
	require.Equal(t, "unavailable", overLimit.OccurrenceState)
	require.Equal(t, "occurrence_limit", overLimit.OccurrenceReason)
	require.Nil(t, overLimit.Occurrences)
}
