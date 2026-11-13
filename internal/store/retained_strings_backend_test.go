package store

import (
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"testing"
	"time"
)

func seedNULMailbox(t *testing.T, s *Store) *Store {
	t.Helper()
	ctx := t.Context()
	req := MailboxContainerRequest{ID: "container\x00id", Owner: "owner\x00id", SHA256: fakeHash("a3"), Size: 3, Format: "mbox"}
	_, err := s.BeginMailboxContainer(ctx, req)
	require.NoError(t, err)
	require.NoError(t, s.RecordRenditionBlob(ctx, req.SHA256, 3, BlobPhysical{Encoding: "raw", StoredBytes: 3, Created: true}))
	require.NoError(t, s.PutMailboxChunk(ctx, req.Owner, req.ID, MailboxChunk{SHA256: req.SHA256, Size: 3}))
	_, err = s.SealMailboxContainer(ctx, req.Owner, req.ID, req.SHA256, 3)
	require.NoError(t, err)
	job, err := s.BeginMailboxJob(ctx, req.Owner, MailboxJobRequest{ID: "job\x00id", ContainerID: req.ID, ContainerSHA256: req.SHA256, Settings: MailboxSettings{DestinationID: s.RootID()}})
	require.NoError(t, err)
	claim, err := s.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.Equal(t, job.ID, claim.ID)
	require.NoError(t, s.CommitMailboxOccurrence(ctx, claim.ID, claim.Claim, MailboxOccurrence{JobID: job.ID, Ordinal: 1, Outcome: "rejected", Reason: "message\x00error", Location: MailboxLocation{ContainerID: req.ID, Entry: "inbox", EntrySHA256: req.SHA256, Sequence: 1, Start: 0, End: 3, Separator: "From synthetic", RawSHA256: req.SHA256, Labels: []string{}}}, nil))
	require.NoError(t, s.FinishMailboxJob(ctx, claim.ID, claim.Claim, "partial", "partial\x00reason", true))
	return s
}

func checkNULMailbox(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	container, err := s.MailboxContainer(ctx, "owner\x00id", "container\x00id")
	require.NoError(t, err)
	require.Len(t, container.Chunks, 1)
	jobs, err := s.MailboxJobs(ctx, "owner\x00id", "", 1)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "job\x00id", jobs[0].ID)
	require.Equal(t, "partial\x00reason", jobs[0].Reason)
	tail, err := s.MailboxJobs(ctx, "owner\x00id", jobs[0].ID, 1)
	require.NoError(t, err)
	require.Empty(t, tail)
	occurrences, err := s.MailboxOccurrences(ctx, container.Owner, jobs[0].ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, occurrences, 1)
	require.Equal(t, "message\x00error", occurrences[0].Reason)
}

func seedNULXMP(t *testing.T, s *Store) *Store {
	t.Helper()
	_, _, raw := xmpSidecarPhoto(t, s)
	work, _, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
	require.NoError(t, err)
	sidecar := work.Sidecar.Node
	run, err := s.BeginIngest(t.Context(), "photo-import", "outside metadata")
	require.NoError(t, err)
	_, _, err = s.IngestFileWithMembership(t.Context(), run, s.RootID(), sidecar.Name, sidecar.BlobHash, sidecar.Size, "application/rdf+xml", "/source\x00/capture.xmp", "")
	require.NoError(t, err)
	work, _, err = s.ClassifyPhotoXMP(t.Context(), raw.ID)
	require.NoError(t, err)
	steps := work.Steps()
	for i := range steps {
		steps[i].Packet = PhotoXMPPacket{Keywords: []string{}}
	}
	plan, mask := PlanPhotoXMP(work, steps, "")
	require.Zero(t, mask)
	require.Zero(t, plan.Conflicts)
	_, err = s.PublishPhotoXMP(t.Context(), plan, "", 0, BlobPhysical{})
	require.NoError(t, err)
	work, _, err = s.ClassifyPhotoXMP(t.Context(), raw.ID)
	require.NoError(t, err)
	require.Len(t, work.History.Outside, 1)
	_, err = s.PublishPhotoXMP(t.Context(), PhotoXMPPlan{Work: work, Refusal: "failure\x00reason", State: PhotoXMPStateInvalid}, "", 0, BlobPhysical{})
	require.NoError(t, err)
	return s
}

func checkNULXMP(t *testing.T, s *Store) {
	t.Helper()
	var file string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT file_id FROM photo_files WHERE role='raw'`).Scan(&file))
	work, _, err := s.ClassifyPhotoXMP(t.Context(), file)
	require.NoError(t, err)
	require.Len(t, work.History.Outside, 1)
	require.Equal(t, "/source\x00/capture.xmp", work.History.Outside[0].Path)
	require.Equal(t, "failure\x00reason", work.receipt.Reason)
	targets, err := s.PhotoXMPTargetsAfter(t.Context(), "", 10)
	require.NoError(t, err)
	require.Empty(t, targets)
}

func checkRetainedStringNativeWritesPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	t.Run("mailbox", func(t *testing.T) {
		t.Parallel()
		s := seedNULMailbox(t, newPostgresQueryStore(t))
		checkNULMailbox(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("MIME", func(t *testing.T) {
		t.Parallel()
		s := seedNULMIME(t, newPostgresQueryStore(t))
		checkNULMIME(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("saved-query", func(t *testing.T) {
		t.Parallel()
		s := seedNULSavedQuery(t, newPostgresQueryStore(t))
		checkNULSavedQuery(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("events", func(t *testing.T) {
		t.Parallel()
		s := seedNULEvents(t, newPostgresQueryStore(t))
		checkNULEvents(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("watch", func(t *testing.T) {
		t.Parallel()
		s := seedNULWatch(t, newPostgresQueryStore(t))
		checkNULWatch(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("package-header", func(t *testing.T) {
		t.Parallel()
		s := seedNULPackageHeader(t, newPostgresQueryStore(t))
		checkNULPackageHeader(t, s)
		var preflightID string
		require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT preflight_id FROM package_import_jobs`).Scan(&preflightID))
		preflight, err := s.PackagePreflight(t.Context(), "owner\x00id", preflightID)
		require.NoError(t, err)
		require.Equal(t, "ref\x00source", preflight.SourceRef)
		require.Equal(t, "locator\x00path", preflight.SourceLocator)
		claim, err := s.ClaimPackageImportJob(t.Context(), "worker\x00id", time.Minute)
		require.NoError(t, err)
		require.Equal(t, "worker\x00id", claim.ClaimOwner)
		_, err = s.ReleasePackageImportJob(t.Context(), claim.ID, claim.Epoch, claim.Token)
		require.NoError(t, err)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("preview", func(t *testing.T) {
		t.Parallel()
		s := seedNULPreview(t, newPostgresQueryStore(t))
		checkNULPreview(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("snapshot", func(t *testing.T) {
		t.Parallel()
		s := seedNULSnapshot(t, newPostgresQueryStore(t))
		checkNULSnapshot(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("processing", func(t *testing.T) {
		t.Parallel()
		s := seedNULProcessing(t, newPostgresQueryStore(t))
		var id, fingerprint string
		require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT job_id FROM rendition_jobs`).Scan(&id))
		require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT profile_fingerprint FROM processing_profiles`).Scan(&fingerprint))
		targets, err := s.MissingProcessingTargetsAfter(t.Context(), fingerprint, "", 1, 100)
		require.NoError(t, err)
		require.NotEmpty(t, targets)
		claim, err := s.ClaimRenditionJob(t.Context(), id, "worker\x00owner", time.Now().Add(time.Second), time.Minute)
		require.NoError(t, err)
		require.Equal(t, "worker\x00owner", claim.Owner)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("package-labels", func(t *testing.T) {
		t.Parallel()
		s := seedNULPackageLabels(t, newPostgresQueryStore(t))
		checkNULPackageLabels(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
	t.Run("XMP", func(t *testing.T) {
		t.Parallel()
		s := seedNULXMP(t, newPostgresQueryStore(t))
		checkNULXMP(t, s)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	})
}

func seedNULSnapshot(t *testing.T, s *Store) *Store {
	t.Helper()
	node, err := s.CreateFile(t.Context(), s.RootID(), "snapshot.txt", fakeHash("b7"), 9, "text/plain")
	require.NoError(t, err)
	id, err := newUUIDv4()
	require.NoError(t, err)
	_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{{Ordinal: 1, OccurrenceID: "occurrence\x00id", NodeID: node.ID, ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: node.Size, FamilyID: "occurrence\x00id", FamilyOrder: 1, DisplayName: "display\x00name", FrozenFieldsJSON: "{}", DocumentKind: "kind\x00value", Representations: []CollectionSnapshotRepresentation{{OccurrenceID: "occurrence\x00id", Role: "custom\x00role", Status: roleAvailable, TextAuthority: "none", ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: node.Size, MediaType: "media\x00type"}}}}})
	require.NoError(t, err)
	return s
}
func checkNULSnapshot(t *testing.T, s *Store) {
	t.Helper()
	var id string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT snapshot_id FROM collection_snapshots`).Scan(&id))
	members, err := s.SnapshotMembers(t.Context(), id, 0, 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "occurrence\x00id", members[0].OccurrenceID)
	require.Equal(t, "occurrence\x00id", members[0].FamilyID)
	require.Equal(t, "display\x00name", members[0].DisplayName)
	require.Equal(t, "kind\x00value", members[0].DocumentKind)
	require.Len(t, members[0].Representations, 1)
	require.Equal(t, "custom\x00role", members[0].Representations[0].Role)
	require.Equal(t, "media\x00type", members[0].Representations[0].MediaType)
}
func seedNULProcessing(t *testing.T, s *Store) *Store {
	t.Helper()
	versions := seedRenditionCatalogVersions(t, s)
	profile := catalogProcessingProfileWith(t, true, func(p *document.ProcessingProfileV1) {
		p.RetentionDisclosure.TrustBoundary = "vault\x00boundary"
		p.Rendition.TrustBoundary = "provider\x00boundary"
		p.Embeddings[0].TrustBoundary = "embedding\x00boundary"
	})
	build := catalogRenditionBuild(s, profile)
	build.ProviderOperationID = "provider\x00operation"
	for i := range build.Artifacts {
		build.Artifacts[i].ID += "\x00artifact"
	}
	for i := range build.Units {
		build.Units[i].ID += "\x00unit"
		build.Units[i].EvidenceUnitID += "\x00evidence"
	}
	for i := range build.LexicalSegments {
		build.LexicalSegments[i].ID += "\x00segment"
		build.LexicalSegments[i].UnitID += "\x00unit"
	}
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	require.NoError(t, publishRenditionForTest(t, s, RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: "2026-08-22T10:00:00.000000000Z"}, "2026-08-22T10:02:00.000000000Z", fakeHash("c7")))
	request := renditionJobTestRequest(versions[1], profile)
	request.Authorization.Principal = "operator\x00principal"
	request.Authorization.Scope = "scope\x00value"

	grantRenditionJobConsent(t, s, request)
	_, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), CurrentRenditionRoot{ID: "retained\x00build", Kind: RenditionRootRetention, TargetKind: RenditionRootBuild, TargetID: build.ID, FencingToken: 1, RecordedAt: build.CompletedAt}))
	return s
}
func seedNULPackageLabels(t *testing.T, s *Store) *Store {
	t.Helper()
	job, record, labels, receipt := receivedRecordPublication(t, s)
	record.LoadFile = "VOL\x00001.dat"
	key, err := PackageRecordKey(record.LoadFile, record.RowOrdinal, "DOC-A")
	require.NoError(t, err)
	record.RowID = key
	record.OccurrenceID = PackageOccurrenceID(record.PackageID, key)
	receipt.RecordKey, receipt.OccurrenceID = key, record.OccurrenceID
	// Worker receipts retain arbitrary gap paths and member display names.
	receipt.ReceiptJSON, err = json.Marshal(struct {
		Member CollectionSnapshotMember `json:"member"`
		Gaps   []string                 `json:"gaps"`
	}{Member: CollectionSnapshotMember{DisplayName: "display\x00name", Representations: []CollectionSnapshotRepresentation{{ContentVersionID: receipt.ContentVersionID}}}, Gaps: []string{"outside\x00/path"}})
	require.NoError(t, err)
	for i := range labels {
		labels[i].LabelSet = "set\x00id"
		labels[i].Label = "label\x00value"
		labels[i].OccurrenceID = record.OccurrenceID
	}
	_, err = s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
	require.NoError(t, err)
	_, err = s.FinishPackageImportJob(t.Context(), job.ID, job.Epoch, job.Token, "failed", "", nil)
	require.NoError(t, err)
	return s
}
func checkNULPackageLabels(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.LookupPackageLabel(t.Context(), "label\x00value", "", "set\x00id", packageDirectionReceived)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, "set\x00id", rows[0].LabelSet)
	progress, err := s.PackageImportProgress(t.Context(), rows[0].PackageID)
	require.NoError(t, err)
	require.Equal(t, 1, progress.Committed)
	require.Equal(t, 1, progress.GapCount)
	require.Equal(t, []string{"outside\x00/path"}, progress.Gaps)
}

var retainedMIMETypes = []string{"text/\x00plain", "message/rfc822\x00suffix", "\x00message/rfc822", "application/pdf\x00tail", "application/pdf\x00tail; charset=x", "image/jpeg\x00suffix", "application/pdf; attr=\x00value", "message/rfc822"}

func seedNULMIME(t *testing.T, s *Store) *Store {
	t.Helper()
	for i, mime := range retainedMIMETypes {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("mime-%d.jpg", i), fakeHash(fmt.Sprintf("%02x", 20+i)), 9, mime)
		require.NoError(t, err)
	}
	return s
}
func checkNULMIME(t *testing.T, s *Store) {
	t.Helper()
	for i, mime := range retainedMIMETypes {
		node, err := s.NodeByPath(t.Context(), fmt.Sprintf("/mime-%d.jpg", i))
		require.NoError(t, err)
		require.Equal(t, mime, node.MimeType)
		history, err := s.ContentVersionViewByID(t.Context(), node.ID, node.CurrentVersionID)
		require.NoError(t, err)
		require.Equal(t, mime, history.Version.MimeType)
	}
}
func seedNULSavedQuery(t *testing.T, s *Store) *Store {
	t.Helper()
	_, err := s.CreateSavedQuery(t.Context(), "photo query", "", SavedQueryKindQuery, []byte(`{"text":"before\u0000after","filters":{"kinds":["photo"]}}`))
	require.NoError(t, err)
	return s
}
func checkNULSavedQuery(t *testing.T, s *Store) {
	t.Helper()
	rows, total, err := s.PhotoSavedQueries(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, rows, 1)
	payload := []byte(`{"text":"changed\u0000query","filters":{"kinds":["video"]}}`)
	_, err = s.UpdateSavedQuery(t.Context(), rows[0].ID, rows[0].Revision, SavedQueryPatch{Payload: &payload})
	require.NoError(t, err)
	rows, total, err = s.PhotoSavedQueries(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, rows, 1)
}
func seedNULEvents(t *testing.T, s *Store) *Store {
	t.Helper()
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), fakeHash("f1")))
	version, target := ingestDocumentEventTarget(t, s, "events.txt", "c5")
	record := documentEventRecord(t, s.VaultID(), version.ID, "a")
	event := &record.Events[0]
	event.SourceKey = "source\x00key"
	event.SourceKindRaw = "kind\x00raw"
	event.RawValue = "raw\x00date"
	event.ZoneText = "zone\x00text"
	event.EvidenceID = "evidence\x00id"
	record.Sources[0].EvidenceID = event.EvidenceID
	event.Actors[0].DisplayName = "person\x00name"
	event.Actors[0].Address = "address\x00value"
	event.EventID = document.DocumentEventID(s.VaultID(), version.ID, event.SourceKey, event.DateKind, event.DateValue, event.Precision, event.TimezoneKind, event.EvidenceSHA256)
	record.Primaries[0].EventID = event.EventID
	record.Primaries[0].Reason = "reason\x00value"
	_, err := s.PublishDocumentEvents(t.Context(), target, fakeHash("f1"), requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)
	return s
}
func checkNULEvents(t *testing.T, s *Store) {
	t.Helper()
	node, err := s.NodeByPath(t.Context(), "/events.txt")
	require.NoError(t, err)
	view, err := s.DocumentEventsForVersion(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, view.Events.Events, 1)
	require.Equal(t, "source\x00key", view.Events.Events[0].SourceKey)
	require.Equal(t, "person\x00name", view.Events.Events[0].Actors[0].DisplayName)
	require.Equal(t, "address\x00value", view.Events.Events[0].Actors[0].Address)
	require.Equal(t, "reason\x00value", view.Events.Primaries[0].Reason)
}

func seedNULWatch(t *testing.T, s *Store) *Store {
	t.Helper()
	run, err := s.BeginIngest(t.Context(), "watch", "watch\x00name")
	require.NoError(t, err)
	_, err = s.IngestFileExact(t.Context(), run, s.RootID(), "watched.txt", fakeHash("c6"), 9, "text/plain", "folder\x00/file.txt", "")
	require.NoError(t, err)
	run, err = s.BeginIngest(t.Context(), "watch", "push-test")
	require.NoError(t, err)
	_, err = s.IngestFileExact(t.Context(), run, s.RootID(), "cursor.txt", fakeHash("c7"), 9, "text/plain", "folder\x00/file.txt", "")
	require.NoError(t, err)
	return s
}
func checkNULWatch(t *testing.T, s *Store) {
	t.Helper()
	cursors, err := s.PushCursors(t.Context(), "push-test", "", 10)
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	require.Equal(t, "folder\x00/file.txt", cursors[0].Ref)
	tail, err := s.PushCursors(t.Context(), "push-test", cursors[0].Ref, 10)
	require.NoError(t, err)
	require.Empty(t, tail)
	present, err := s.HasWatchSource(t.Context(), "watch\x00name", "folder\x00/file.txt")
	require.NoError(t, err)
	require.True(t, present)
	node, err := s.NodeByPath(t.Context(), "/watched.txt")
	require.NoError(t, err)
	_, err = s.RebindWatchedSource(t.Context(), "watch\x00name", "folder\x00/file.txt", "new\x00/file.txt", node.ID)
	require.NoError(t, err)
	present, err = s.HasWatchSource(t.Context(), "watch\x00name", "new\x00/file.txt")
	require.NoError(t, err)
	require.True(t, present)
}
func seedNULPackageHeader(t *testing.T, s *Store) *Store {
	t.Helper()
	request, run, _ := prepareReceivedPackage(t, s, "header")
	request.PartyLabel = "party\x00label"
	request.Relation = "relation\x00label"
	request.ProducedOn = "date\x00label"
	request.Volumes = []PackageVolume{{Ordinal: 1, VolumeName: "volume\x00name", DeclaredRoot: "declared\x00root", MappedRoot: "mapped\x00root", ResolvedRootSHA256: fakeHash("a4")}}
	job := packageImportJobRequest(t, s, request)
	preflight, err := s.PackagePreflight(t.Context(), job.Owner, job.PreflightID)
	require.NoError(t, err)
	preflight.PreflightID, err = newUUIDv4()
	require.NoError(t, err)
	preflight.Owner, preflight.SourceRef, preflight.SourceLocator = "owner\x00id", "ref\x00source", "locator\x00path"
	_, err = s.PutPackagePreflight(t.Context(), preflight)
	require.NoError(t, err)
	job.Owner, job.PreflightID = preflight.Owner, preflight.PreflightID
	job.JobJSON = []byte(`{"accept_partial":false,"index_supplied_text":true,"source_kind":"root","source_locator":"locator\u0000path"}`)
	_, err = s.AdmitPackageImport(t.Context(), run, request, job)
	require.NoError(t, err)
	return s
}
func checkNULPackageHeader(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.Packages(t.Context(), packageDirectionReceived, "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var operationID string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT operation_id FROM package_import_jobs`).Scan(&operationID))
	job, err := s.PackageImportJob(t.Context(), "owner\x00id", operationID)
	require.NoError(t, err)
	require.Equal(t, "owner\x00id", job.Owner)
	require.Equal(t, "party\x00label", rows[0].PartyLabel)
	require.Equal(t, "relation\x00label", rows[0].Relation)
	require.Equal(t, "date\x00label", rows[0].ProducedOn)
	require.Len(t, rows[0].Volumes, 1)
	require.Equal(t, "volume\x00name", rows[0].Volumes[0].VolumeName)
	require.Equal(t, "declared\x00root", rows[0].Volumes[0].DeclaredRoot)
	require.Equal(t, "mapped\x00root", rows[0].Volumes[0].MappedRoot)
}
func seedNULPreview(t *testing.T, s *Store) *Store {
	t.Helper()
	node, err := s.CreateFile(t.Context(), s.RootID(), "preview.txt", fakeHash("a5"), 9, "text/plain")
	require.NoError(t, err)
	raw, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{ContractVersion: document.VisualPreviewContractV1, SourceSHA256: node.BlobHash, Recipe: visualPreviewRecipe(), State: document.VisualPreviewUnsupported, Failure: &document.VisualPreviewFailureV1{Code: "unsupported_media_type", Detail: "reason\x00detail"}})
	require.NoError(t, err)
	_, err = s.PublishVisualPreview(t.Context(), node.CurrentVersionID, raw, nil)
	require.NoError(t, err)
	return s
}
func checkNULPreview(t *testing.T, s *Store) {
	t.Helper()
	node, err := s.NodeByPath(t.Context(), "/preview.txt")
	require.NoError(t, err)
	view, err := s.ContentVersionVisualPreview(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, "reason\x00detail", view.Generation.Preview.Failure.Detail)
}
