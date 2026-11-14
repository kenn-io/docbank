package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkMetadataImportCatalogsPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	seeds := retainedMetadataCatalogSeeds()
	seeds["NUL watch"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULWatch(t, newTestStore(t))
	}
	seeds["NUL package header"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULPackageHeader(t, newTestStore(t))
	}
	seeds["NUL preview"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULPreview(t, newTestStore(t))
	}
	seeds["NUL MIME"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULMIME(t, newTestStore(t))
	}
	seeds["NUL saved query"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULSavedQuery(t, newTestStore(t))
	}
	seeds["NUL snapshot"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULSnapshot(t, newTestStore(t))
	}
	seeds["NUL processing"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULProcessing(t, newTestStore(t))
	}
	seeds["NUL package labels"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULPackageLabels(t, newTestStore(t))
	}
	seeds["NUL mailbox state"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULMailbox(t, newTestStore(t))
	}
	seeds["NUL XMP receipt"] = func(t *testing.T) *Store {
		t.Helper()
		return seedNULXMP(t, newTestStore(t))
	}
	seeds["NUL media identities and inputs"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "recording.wav", fakeHash("d6"), 10, "audio/wav")
		require.NoError(t, err)
		sourceID := "source\x00id"
		_, err = s.RetainMediaReference(t.Context(), MediaReferencePublicationRequest{Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000801", Principal: "operator", Verb: "submit_remote_recording", SourceID: sourceID, RequestSHA256: fakeHash("d5")}, Provider: "url", OriginScope: "scope", IdentitySHA256: fakeHash("d4"), Outcome: "unsupported", Occurrence: MediaOccurrenceInput{ID: "appearance\x00id", SourceID: sourceID, Principal: "operator", Ref: "ref", Revision: "1", Filename: "recording.wav", MessageJSON: "{}"}})
		require.NoError(t, err)
		require.NoError(t, s.PublishMediaSourceVersion(t.Context(), MediaSourceVersionInput{ID: "version\x00id", SourceID: sourceID, ContentVersionID: node.CurrentVersionID, CaptureJSON: "{}", Revision: 1, BindOccurrenceIDs: []string{"appearance\x00id"}}))
		_, err = s.ImportMediaInputArtifact(t.Context(), MediaInputArtifactRequest{Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000802", Principal: "operator", Verb: "import_recording_artifact", SourceID: sourceID, RequestSHA256: fakeHash("d3")}, InputID: "input\x00id", OccurrenceID: "appearance\x00id", SourceVersionID: "version\x00id", VirtualPath: "/inputs/transcript.txt", MediaType: "text/plain", ByteLength: 12, Physical: BlobPhysical{Encoding: "raw", StoredBytes: 12, Created: true}, Kind: "transcript", Origin: "origin\x00value", Provider: "provider\x00value", Language: "language\x00value", InputSHA: fakeHash("d2")})
		require.NoError(t, err)
		return s
	}
	seeds["NUL archive identities"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		require.NoError(t, s.RegisterMailboxArchive(t.Context(), MailboxArchive{ID: "archive\x00id", Owner: "owner\x00id", Description: "description"}))
		email := newEmailFixture(t, s, "archive.eml")
		source, err := emailVersion(t.Context(), s.db, email.publication.ContentVersionID)
		require.NoError(t, err)
		run, err := s.BeginIngest(t.Context(), "mailbox", "Synthetic archive")
		require.NoError(t, err)
		settings, err := (MailboxSettings{DestinationID: s.RootID()}).Canonical()
		require.NoError(t, err)
		_, err = s.PublishMailboxTransfer(t.Context(), MailboxTransferPublication{Owner: "owner\x00id", Run: run, Request: MailboxTransferRequest{ArchiveID: "archive\x00id", Reference: "reference\x00original", SHA256: source.BlobHash, Size: source.Size, Settings: settings, DestinationID: s.RootID(), Name: "message.eml"}, Email: email.publication})
		require.NoError(t, err)

		return s
	}
	seeds["NUL assertion note"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, face := faceForDecisionTest(t, s, "")
		person, err := s.CreatePerson(t.Context(), "Candidate\x00Name", "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "author", Action: "assert", Note: "note\x00evidence", Revision: 1})
		require.NoError(t, err)
		_, err = s.DecidePhotoFace(t.Context(), node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, PersonID: person.PersonID, Action: "name", Revision: face.Revision})
		require.NoError(t, err)
		return s
	}
	seeds["NUL face receipt"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node := browsePhotoNode(t, s, "portrait.jpg", fakeHash("d9"), "image/jpeg")
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			return s.publishFaceDetectionTx(t.Context(), tx, node.CurrentVersionID, fakeHash("d8"), document.FaceDetection{Faces: []document.FaceObservation{{Box: document.FaceBox{Width: .3, Height: .4}}}}, time.Now())
		}))
		faces, _, _, err := s.PhotoFacePersonNames(t.Context(), node.CurrentVersionID)
		require.NoError(t, err)
		require.Len(t, faces, 1)
		face := faces[0]
		raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: node.CurrentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID}})
		require.NoError(t, err)
		candidate, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "face:" + face.FaceID, DisplayName: "Face\x00Name", Reason: personCandidateReasonFaceMatch, Evidence: raw})
		require.NoError(t, err)
		require.NoError(t, s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
			_, retained, err := s.OpenPersonCandidate(t.Context(), tx, candidate)
			require.True(t, retained)
			return err
		}))
		_, err = s.DecidePhotoFace(t.Context(), node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: face.FaceID, Action: "dismiss", Revision: face.Revision})
		require.NoError(t, err)
		return s
	}
	seeds["NUL archive"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		require.NoError(t, s.RegisterMailboxArchive(t.Context(), MailboxArchive{ID: "archive", Owner: "operator", Description: "archive\x00description"}))
		return s
	}
	seeds["NUL photo facts"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node := browsePhotoNode(t, s, "camera.jpg", fakeHash("c7"), "image/jpeg")
		browsePhotoMetadata(t, s, node, "nul-photo", photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Camera\x00Make")), photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString("Lens\x00Model")))
		return s
	}
	seeds["NUL remote source"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		source := fakeHash("c6")
		_, err := s.RetainMediaReference(t.Context(), MediaReferencePublicationRequest{Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000801", Principal: "operator", Verb: "submit_remote_recording", RequestSHA256: fakeHash("c5"), SourceID: source}, Provider: "url\x00provider", OriginScope: "scope\x00origin", IdentitySHA256: fakeHash("c4"), Outcome: "unsupported", Occurrence: MediaOccurrenceInput{ID: "occurrence", SourceID: source, Principal: "operator", Ref: "ref", Revision: "1", Filename: "recording.wav", MessageJSON: "{}"}})
		require.NoError(t, err)
		return s
	}
	seeds["NUL source description"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		run, err := s.BeginIngest(t.Context(), "cli", "source\x00description \\u0000 café")
		require.NoError(t, err)
		_, err = s.IngestFileExact(t.Context(), run, s.RootID(), "nul.txt", fakeHash("b4"), 9, "text/plain", "opaque://report", "")
		require.NoError(t, err)
		plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
		require.NoError(t, err)
		_, err = s.EnableInitialAudit(t.Context(), plan)
		require.NoError(t, err)
		return s
	}
	for name, input := range map[string]ProvenanceAppendInput{
		"NUL source reference":   {SourceKind: "agent", SourceDescription: "supplied source", OriginalPath: "opaque://source\x00reference \\u0000 café"},
		"NUL caller source kind": {SourceKind: "agent\x00kind \\u0000 café", SourceDescription: "supplied source", OriginalPath: "opaque://source"},
	} {
		seeds[name] = func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			node, err := s.CreateFile(t.Context(), s.RootID(), "nul.jpg", fakeHash("b5"), 9, "image/jpeg")
			require.NoError(t, err)
			plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
			require.NoError(t, err)
			_, err = s.EnableInitialAudit(t.Context(), plan)
			require.NoError(t, err)
			input.NodeID, input.IfRevision = node.ID, node.Revision
			_, err = s.AppendNodeProvenance(t.Context(), input)
			require.NoError(t, err)
			return s
		}
	}
	seeds["NUL operational photo source"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		run, err := s.BeginIngest(t.Context(), "cli\x00kind", "operational source")
		require.NoError(t, err)
		_, err = s.IngestFileExact(t.Context(), run, s.RootID(), "nul-photo.jpg", fakeHash("b6"), 9, "image/jpeg", "/source/\x00reference/photo.jpg", "2024-03-05T10:00:00Z")
		require.NoError(t, err)
		plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
		require.NoError(t, err)
		_, err = s.EnableInitialAudit(t.Context(), plan)
		require.NoError(t, err)
		return s
	}
	seeds["NUL person name"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		person, err := s.CreatePerson(t.Context(), "Ada\x00李明", "operator")
		require.NoError(t, err)
		_, err = s.CreatePerson(t.Context(), "Zoe", "operator")
		require.NoError(t, err)
		version := seedDocumentPeopleEvent(t, s, "claim.txt", "c9", nil)
		node, err := s.NodeByID(t.Context(), version.NodeID)
		require.NoError(t, err)
		_, err = s.SetCustodian(t.Context(), CustodianRequest{Scope: CustodianScope{Kind: "document", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}, PersonID: person.PersonID, RawLabel: "Ada\x00label", Rank: "primary", Basis: "operator_assigned", SourceRef: "claim\x00reference", IfMatchRevision: 1})
		require.NoError(t, err)
		_, err = s.LinkExternalIdentity(t.Context(), PersonExternalIdentity{PersonID: person.PersonID, System: "msgvault", ArchiveID: "synthetic", UID: "synthetic-id", UIDKind: "vcard_uid", UIDState: "current", DisplayNameSnapshot: "Ada\x00snapshot"}, person.Revision)
		require.NoError(t, err)
		person, _, err = s.PersonByID(t.Context(), person.PersonID)
		require.NoError(t, err)
		_, err = s.AddPersonIdentity(t.Context(), person.PersonID, person.Revision, PersonIdentity{Kind: "email", ValueDisplay: "evidence@example.test", Origin: "operator", EvidenceKind: "operator_assertion", EvidenceID: "claim\x00\\evidence", Confidence: "operator_asserted"})
		require.NoError(t, err)
		asset := albumAsset(t, s, "portrait.jpg")
		photo := requireNodeByID(t, s, asset.Files[0].NodeID)
		_, err = s.MarkPersonInPhoto(t.Context(), photo.CurrentVersionID, person.PersonID, "")
		require.NoError(t, err)
		return s
	}
	seeds["NUL extraction"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "text.txt", fakeHash("c8"), 15, "text/plain")
		require.NoError(t, err)
		require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: node.BlobHash, Extractor: "custom\x00extractor", ExtractorVersion: 1, Status: ExtractionOK, Text: "before\x00after café"}))
		require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: node.BlobHash, Extractor: "failed\x00extractor", ExtractorVersion: 1, Status: ExtractionFailed, Error: "failure\x00detail"}))
		return s
	}
	for name, text := range map[string]string{"NUL leading email text": " \x00after", "NUL email text": "before\x00after"} {
		seeds[name] = func(t *testing.T) *Store {
			t.Helper()
			s := newTestStore(t)
			fixture := newEmailSourceFixture(t, s, "source.eml", "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=source.txt\r\n\r\nbody\r\n--m--\r\n")
			view, err := s.PublishEmailGeneration(t.Context(), fixture.publication)
			require.NoError(t, err)
			receipt, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "native-nul"))
			require.NoError(t, err)
			require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: receipt.Relations[0].Child.SHA256, Extractor: legacyPlainTextExtractor, ExtractorVersion: legacyPlainTextExtractorVersion, Status: ExtractionOK, Text: text}))
			_, err = s.MigrateLegacyPlainText(t.Context())
			require.NoError(t, err)
			return s
		}
	}
	seeds["NUL media occurrence"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		request := suppliedMediaPublicationFixture(t, s)
		request.Operation.Principal = "operator\x00principal"
		request.Occurrence.Principal = request.Operation.Principal
		request.Occurrence.Ref = "ref\x00original"
		request.Occurrence.Revision = "revision\x001"
		request.Occurrence.Filename = "recording\x00name.mp3"
		request.Occurrence.PersonRef = "person\x00ref"
		request.Occurrence.SpeakerLabel = "speaker\x00label"
		_, err := s.RetainSuppliedMedia(t.Context(), request)
		require.NoError(t, err)
		return s
	}
	seeds["mailbox work"] = seedMetadataImportMailbox
	seeds["mailbox job"] = func(t *testing.T) *Store {
		t.Helper()
		s := seeds["mailbox"](t)
		_, err := s.BeginMailboxJob(t.Context(), "operator", MailboxJobRequest{ID: "queued-job", ContainerID: "synthetic-mailbox", ContainerSHA256: fakeHash("ac"), Settings: MailboxSettings{DestinationID: s.RootID()}})
		require.NoError(t, err)
		return s
	}
	seeds["photo suggestions"] = seedMetadataImportSuggestions
	seeds["rendered pages"] = seedMetadataImportPages
	seeds["received records"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		job, record, labels, receipt := receivedRecordPublication(t, s)
		_, err := s.CommitPackageRecordWithLease(t.Context(), job.ID, job.Epoch, job.Token, record, labels, receipt)
		require.NoError(t, err)
		_, err = s.FinishPackageImportJob(t.Context(), job.ID, job.Epoch, job.Token, "failed", "", nil)
		require.NoError(t, err)
		return s
	}
	seeds["export plan"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		plan := exportPlanFixture(t, s, "operator")
		id, err := newUUIDv4()
		require.NoError(t, err)
		job, err := s.QueueExportJob(t.Context(), "operator", bundle.JobRequest{OperationID: id, PlanID: plan.ID, Fingerprint: plan.Fingerprint})
		require.NoError(t, err)
		claim, err := s.ClaimExportJob(t.Context())
		require.NoError(t, err)
		require.NoError(t, s.AdvanceExportJob(t.Context(), claim, plan.RoleEntries, plan.RoleBytes))
		require.NoError(t, s.FinishExportJob(t.Context(), claim, &bundle.Receipt{Format: bundle.Format, PlanFingerprint: plan.Fingerprint, SHA256: fakeHash("e2"), Size: 100, Entries: plan.ArchiveEntries()}, job.ID+".zip", ""))
		return s
	}
	seeds["represented snapshot"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "report.txt", fakeHash("a3"), 12, "text/plain")
		require.NoError(t, err)
		id, err := newUUIDv4()
		require.NoError(t, err)
		occurrence := strings.Repeat("c", 32)
		_, err = s.SealCollectionSnapshot(t.Context(), SnapshotSealRequest{SnapshotID: id, Members: []CollectionSnapshotMember{{Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: node.Size, FamilyID: occurrence, FamilyOrder: 1, DisplayName: node.Name, FrozenFieldsJSON: "{}", DocumentKind: "other", Representations: []CollectionSnapshotRepresentation{{OccurrenceID: occurrence, Role: "native", Status: roleAvailable, TextAuthority: "none", ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, MediaType: node.MimeType, Size: node.Size}}}}})
		require.NoError(t, err)
		return s
	}
	seeds["media input"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		retained, err := s.RetainSuppliedMedia(t.Context(), suppliedMediaPublicationFixture(t, s))
		require.NoError(t, err)
		_, err = s.ImportMediaInputArtifact(t.Context(), MediaInputArtifactRequest{
			Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000064", Principal: "operator", Verb: "import_recording_artifact", SourceID: retained.SourceID, RequestSHA256: testSHA256([]byte("artifact-request"))},
			InputID:   testSHA256([]byte("artifact")), OccurrenceID: retained.OccurrenceID, SourceVersionID: retained.SourceVersionID,
			VirtualPath: "/media-input/transcript.txt", MediaType: "text/plain", ByteLength: 20,
			Physical: BlobPhysical{Encoding: "raw", StoredBytes: 20, Created: true}, Kind: "transcript", Origin: "supplied", Language: "en", InputSHA: testSHA256([]byte("transcript")),
		})
		require.NoError(t, err)
		return s
	}
	seeds["preview and checksum"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, err := s.CreateFile(t.Context(), s.RootID(), "photo.raw", fakeHash("11"), 12, "image/x-raw")
		require.NoError(t, err)
		_, err = s.PublishVisualPreview(t.Context(), node.CurrentVersionID, readyVisualPreview(t, node.BlobHash, fakeHash("22"), 9), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
		require.NoError(t, err)
		require.NoError(t, s.RecordVerifiedBlobChecksum(t.Context(), BlobChecksumRecord{BlobSHA256: node.BlobHash, MD5: strings.Repeat("a", 32)}))
		return s
	}
	seeds["audit and moved parents"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		seedMetadataRoundTrip(t, s)
		parent, err := s.Mkdir(t.Context(), s.RootID(), "newer-parent")
		require.NoError(t, err)
		old, err := s.NodeByPath(t.Context(), "/Projects")
		require.NoError(t, err)
		_, _, err = s.Move(t.Context(), old.ID, parent.ID, "Projects", old.Revision)
		require.NoError(t, err)
		deleted, err := s.Mkdir(t.Context(), s.RootID(), "deleted-highest")
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), deleted.ID, deleted.Revision)
		require.NoError(t, err)
		_, err = s.TrashEmptyBounded(t.Context(), 0, 10, true)
		require.NoError(t, err)
		plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
		require.NoError(t, err)
		_, err = s.EnableInitialAudit(t.Context(), plan)
		require.NoError(t, err)
		return s
	}
	seeds["hidden photo lockout"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		asset := albumAsset(t, s, "hidden.jpg")
		require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
		_, err := s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
		require.NoError(t, err)
		for range 4 {
			_, _, err := s.UnlockPhotoHidden(t.Context(), "wrong-passcode")
			require.Error(t, err)
		}
		return s
	}
	seeds["processing jobs"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		versions := seedRenditionCatalogVersions(t, s)
		request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
		grantRenditionJobConsent(t, s, request)
		_, _, err := s.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		return s
	}
	seeds["candidate faces"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		node, face := faceForDecisionTest(t, s, "")
		buildFaceMatchCandidateForTest(t, s, node.CurrentVersionID, face.FaceID, 1, 0)
		person, err := s.CreatePerson(t.Context(), "Example Person", "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(t.Context(), PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
		require.NoError(t, err)
		require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
		return s
	}
	seeds["photo projection pages"] = func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		run, err := s.BeginIngest(t.Context(), "cli", "Synthetic dated photos")
		require.NoError(t, err)
		tag, err := s.CreateTag(t.Context(), "retained")
		require.NoError(t, err)
		for i := range 205 {
			node := ingestMtimePhoto(t, s, run, fmt.Sprintf("photo-%03d.jpg", i), "2024-03-05T10:00:00Z")
			_, err := s.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
		}
		return s
	}
	// Verify these native fixtures actually produce the custom records whose
	// insertion behavior the round trip must exercise.
	requiredKinds := map[string][]string{
		"NUL watch":                       {"watch_source"},
		"NUL package header":              {"package", "package_volume"},
		"NUL preview":                     {"visual_preview_generation"},
		"NUL MIME":                        {"content_version"},
		"NUL saved query":                 {"saved_query"},
		"NUL snapshot":                    {"collection_snapshot", "collection_snapshot_member", "collection_snapshot_representation"},
		"NUL processing":                  {"processing_profile", "rendition_build", "rendition_unit", "processing_consent_grant", "rendition_job"},
		"NUL package labels":              {"package_label", "package_record", "package_import_receipt"},
		"NUL mailbox state":               {"mailbox_container", "mailbox_job", "mailbox_occurrence"},
		"NUL XMP receipt":                 {"photo_change_receipt"},
		"NUL media identities and inputs": {"media_source", "media_source_version", "media_occurrence", "media_input_artifact", "media_operation"},
		"NUL archive identities":          {"mailbox_archive", "mailbox_transfer_receipt"},

		"NUL assertion note": {"person_document_assertion", "photo_change_receipt"},
		"NUL archive":        {"mailbox_archive"},
		"NUL photo facts":    {"source_metadata_generation"},
		"NUL remote source":  {"media_source", "media_occurrence"},
		"NUL face receipt":   {"photo_change_receipt", "person_match_candidate"},

		"mailbox job":                  {"mailbox_job"},
		"mailbox work":                 {"mailbox_job", "mailbox_occurrence", "mailbox_archive", "mailbox_transfer_receipt", "mailbox_container", "email_document_publication"},
		"photo suggestions":            {"photo_suggestion"},
		"rendered pages":               {"page_document", "page_recipe", "page_image", "page_render_job"},
		"preview and checksum":         {"blob_checksum", "visual_preview_generation"},
		"received records":             {"package_record", "package_label", "package_import_receipt", "package_import_job", "package", "package_volume"},
		"represented snapshot":         {"collection_snapshot", "collection_snapshot_member", "collection_snapshot_representation"},
		"export plan":                  {"export_authority"},
		"candidate faces":              {"person_document_assertion"},
		"media input":                  {"media_source", "media_source_version", "media_occurrence", "media_input_artifact", "media_operation"},
		"embeddings":                   {"embedding_input_generation"},
		"photo checkout":               {"photo_checkout", "photo_checkout_entry"},
		"audit and moved parents":      {"blob", "audit_record"},
		"hidden photo lockout":         {"photo_hidden_failure"},
		"processing":                   {"processing_profile", "rendition_head", "rendition_lexical_generation", "current_rendition_root"},
		"processing jobs":              {"processing_incarnation", "processing_consent_grant", "rendition_job", "rendition_job_waiter"},
		"NUL source description":       {"ingest", "provenance", "audit_record"},
		"NUL person name":              {"person", "person_identity", "custodian_assignment", "person_external_identity"},
		"NUL extraction":               {"extracted_text"},
		"NUL email text":               {"extracted_text", "rendition_lexical_segment", "rendition_lexical_generation"},
		"NUL leading email text":       {"extracted_text", "rendition_lexical_segment", "rendition_lexical_generation"},
		"NUL media occurrence":         {"media_occurrence", "media_operation"},
		"NUL source reference":         {"ingest", "provenance", "audit_record"},
		"NUL caller source kind":       {"ingest", "provenance", "audit_record"},
		"NUL operational photo source": {"ingest", "provenance", "audit_record", "photo_file"},
	}
	for name, seed := range seeds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			source := seed(t)
			var input bytes.Buffer
			require.NoError(t, source.ExportMetadata(ctx, &input))
			require.Subset(t, metadataImportRecordTypes(t, input.Bytes()), requiredKinds[name])
			setup, owner := pgtest.NewDatabase(t)
			cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
			require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
			runtime := newPostgresSchemaStore(t, setup, cfg)
			var output bytes.Buffer
			require.NoError(t, runtime.ExportMetadata(ctx, &output))
			require.Equal(t, input.String(), output.String())
			require.NoError(t, runtime.ValidateMetadata(ctx))
			recovered := newTestStore(t)
			require.NoError(t, recovered.ImportMetadata(ctx, bytes.NewReader(output.Bytes())))
			var roundTrip bytes.Buffer
			require.NoError(t, recovered.ExportMetadata(ctx, &roundTrip))
			require.Equal(t, input.String(), roundTrip.String())
			projections := func(store *Store) []string {
				var result []string
				require.NoError(t, queryRows(ctx, store.db, `SELECT file_id,sort_key,day,COALESCE(modified_time,''),COALESCE(folder_key,'') FROM photo_timeline ORDER BY file_id`, nil, func(rows *sql.Rows) error {
					var file, sortKey, day, modified, folder string
					if err := rows.Scan(&file, &sortKey, &day, &modified, &folder); err != nil {
						return err
					}
					result = append(result, file, sortKey, day, modified, folder)
					return nil
				}))
				require.NoError(t, queryRows(ctx, store.db, `SELECT asset_id FROM photo_xmp_unsettled ORDER BY asset_id`, nil, func(rows *sql.Rows) error {
					var asset string
					if err := rows.Scan(&asset); err != nil {
						return err
					}
					result = append(result, asset)
					return nil
				}))
				return result
			}
			require.Equal(t, projections(recovered), projections(runtime))
			if name == "photo projection pages" {
				for _, store := range []*Store{source, runtime, recovered} {
					var timeline, pending int
					require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_timeline`).Scan(&timeline))
					require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_xmp_unsettled`).Scan(&pending))
					require.Equal(t, 205, timeline)
					require.Equal(t, 205, pending)
				}
			}

			before, err := source.VerifyAudit(ctx, nil)
			require.NoError(t, err)
			after, err := runtime.VerifyAudit(ctx, nil)
			require.NoError(t, err)
			require.Equal(t, before, after)

			if name == "NUL watch" {
				for _, store := range []*Store{runtime, recovered} {
					checkNULWatch(t, store)
				}
			}
			if name == "NUL package header" {
				for _, store := range []*Store{runtime, recovered} {
					checkNULPackageHeader(t, store)
				}
			}
			if name == "NUL preview" {
				for _, store := range []*Store{runtime, recovered} {
					checkNULPreview(t, store)
				}
			}
			if name == "NUL MIME" {
				pdfs, err := source.PDFTextTargetsAfter(ctx, fakeHash("e3"), "", 100, 100, true)
				require.NoError(t, err)
				require.Len(t, pdfs, 2)
				emails, err := source.MissingEmailTargetsAfter(ctx, fakeHash("e4"), fakeHash("e5"), "", 100)
				require.NoError(t, err)
				require.Len(t, emails, 1)
				for _, store := range []*Store{runtime, recovered} {
					got, err := store.PDFTextTargetsAfter(ctx, fakeHash("e3"), "", 100, 100, true)
					require.NoError(t, err)
					require.Equal(t, pdfs, got)
					gotEmails, err := store.MissingEmailTargetsAfter(ctx, fakeHash("e4"), fakeHash("e5"), "", 100)
					require.NoError(t, err)
					require.Equal(t, emails, gotEmails)
				}
				for _, store := range []*Store{runtime, recovered} {
					checkNULMIME(t, store)
				}
			}
			if name == "NUL saved query" {
				for _, store := range []*Store{runtime, recovered} {
					checkNULSavedQuery(t, store)
				}
			}
			if name == "NUL snapshot" {
				for _, store := range []*Store{runtime, recovered} {
					checkNULSnapshot(t, store)
				}
			}
			if name == "NUL package labels" {
				for _, store := range []*Store{runtime, recovered} {
					checkNULPackageLabels(t, store)
				}
			}
			if name == "NUL mailbox state" {
				for _, store := range []*Store{runtime, recovered} {
					checkNULMailbox(t, store)
				}
			}
			if name == "NUL XMP receipt" {
				for _, store := range []*Store{runtime, recovered} {
					checkNULXMP(t, store)
				}
			}
			if name == "NUL archive identities" {
				for _, store := range []*Store{runtime, recovered} {
					receipt, err := store.MailboxTransfer(ctx, "owner\x00id", "archive\x00id", "reference\x00original")
					require.NoError(t, err)
					require.Equal(t, "archive\x00id", receipt.Request.ArchiveID)
					require.NoError(t, store.RegisterMailboxArchive(ctx, MailboxArchive{ID: "archive\x00id", Owner: "owner\x00id", Description: "description"}))
					require.NoError(t, store.RegisterMailboxArchive(ctx, MailboxArchive{ID: "new\x00archive", Owner: "owner\x00id", Description: "new"}))
				}
			}
			if name == "NUL media identities and inputs" {
				for _, store := range []*Store{runtime, recovered} {
					item, err := store.MediaSourceVersion(ctx, "operator", "source\x00id", "version\x00id")
					require.NoError(t, err)
					require.Equal(t, "appearance\x00id", item.OccurrenceID)
					require.Equal(t, "source\x00id", item.Receipt.SourceID)
					input, err := store.SuppliedTranscriptForSourceVersion(ctx, "operator", "transcript", "source\x00id", "version\x00id", "")
					require.NoError(t, err)
					require.Equal(t, "input\x00id", input.InputID)
					require.Equal(t, "origin\x00value", input.Origin)
					require.Equal(t, "provider\x00value", input.Provider)
					require.Equal(t, "language\x00value", input.Language)
					visible, err := store.MediaInputBindingVisible(ctx, "operator", "source\x00id", "version\x00id", "input\x00id")
					require.NoError(t, err)
					require.True(t, visible)
					_, err = store.ImportMediaInputArtifact(ctx, MediaInputArtifactRequest{Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000803", Principal: "operator", Verb: "import_recording_artifact", SourceID: "source\x00id", RequestSHA256: fakeHash("d3")}, InputID: "input\x00id", OccurrenceID: "appearance\x00id", SourceVersionID: "version\x00id", VirtualPath: "/inputs/transcript.txt", MediaType: "text/plain", ByteLength: 12, Physical: BlobPhysical{Encoding: "raw", StoredBytes: 12}, Kind: "transcript", Origin: "origin\x00value", Provider: "provider\x00value", Language: "language\x00value", InputSHA: fakeHash("d2")})
					require.NoError(t, err)
					page, total, more, err := store.MediaOccurrences(ctx, "operator", "source\x00id", "appearance\x00id", 10)
					require.NoError(t, err)
					require.Equal(t, 1, total)
					require.False(t, more)
					require.Empty(t, page)
					_, err = store.RecordMediaOccurrenceRevocation(ctx, testMediaOperation("revoke_occurrence", "operator"), "appearance\x00id", "1")
					require.NoError(t, err)
				}
			}
			if name == "NUL assertion note" {
				for _, store := range []*Store{runtime, recovered} {
					node, err := store.NodeByPath(ctx, "/portrait.jpg")
					require.NoError(t, err)
					person, err := store.CreatePerson(ctx, "Author", "operator")
					require.NoError(t, err)
					assertion, err := store.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "author", Action: "assert", Note: "new\x00note", Revision: 1})
					require.NoError(t, err)
					require.Equal(t, "new\x00note", assertion.Note)
				}
			}

			if name == "NUL archive" {
				for _, store := range []*Store{runtime, recovered} {
					require.NoError(t, store.RegisterMailboxArchive(ctx, MailboxArchive{ID: "archive", Owner: "operator", Description: "archive\x00description"}))
					require.NoError(t, store.RegisterMailboxArchive(ctx, MailboxArchive{ID: "new-archive", Owner: "operator", Description: "new\x00description"}))
				}
			}

			if name == "NUL photo facts" {
				for _, store := range []*Store{runtime, recovered} {
					node, err := store.NodeByPath(ctx, "/camera.jpg")
					require.NoError(t, err)
					facts, err := store.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
					require.NoError(t, err)
					require.Equal(t, "Camera\x00Make", *facts.Fields.CameraMake)
					require.Equal(t, "Lens\x00Model", *facts.Fields.LensModel)
					browsePhotoMetadata(t, store, node, "changed-nul", photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("New\x00Camera")))
					facts, err = store.ContentVersionPhotoMetadata(ctx, node.CurrentVersionID)
					require.NoError(t, err)
					require.Equal(t, "New\x00Camera", *facts.Fields.CameraMake)
				}
			}
			if name == "NUL face receipt" {
				for _, store := range []*Store{runtime, recovered} {
					var id string
					require.NoError(t, store.db.QueryRowContext(ctx, `SELECT receipt_id FROM photo_change_receipts WHERE operation='face_decision' LIMIT 1`).Scan(&id))
					_, err = store.UndoPhotoFaceDecision(ctx, id)
					require.NoError(t, err)
					candidates, _, err := store.PersonCandidates(ctx, "open", 10, 0)
					require.NoError(t, err)
					require.Len(t, candidates, 1)
					require.Equal(t, "Face\x00Name", candidates[0].DisplayName)
					node, err := store.NodeByPath(ctx, "/portrait.jpg")
					require.NoError(t, err)
					faces, _, _, err := store.PhotoFacePersonNames(ctx, node.CurrentVersionID)
					require.NoError(t, err)
					require.Len(t, faces, 1)
					_, err = store.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: faces[0].FaceID, Action: "dismiss", Revision: faces[0].Revision})
					require.NoError(t, err)
					require.NoError(t, store.withStorageTx(ctx, func(tx *sql.Tx) error {
						return store.publishFaceDetectionTx(ctx, tx, node.CurrentVersionID, fakeHash("b9"), document.FaceDetection{}, time.Now())
					}))
					protected, _, _, err := store.PhotoFacePersonNames(ctx, node.CurrentVersionID)
					require.NoError(t, err)
					require.Len(t, protected, 1)
					require.Equal(t, faces[0].FaceID, protected[0].FaceID)
					fresh, _, err := store.CreatePhotoFace(ctx, node.CurrentVersionID, "00000000-0000-4000-8000-000000000903", document.FaceBox{Width: .1, Height: .1}, "")
					require.NoError(t, err)
					_, err = store.DecidePhotoFace(ctx, node.CurrentVersionID, PhotoFaceDecisionRequest{FaceID: fresh.FaceID, Action: "name", PersonName: "New\x00Person", Revision: fresh.Revision})
					require.NoError(t, err)
					require.NoError(t, store.ValidateMetadata(ctx))
				}
			}
			if name == "NUL person name" {
				first, _, err := source.PeopleByDisplayName(ctx, "", "", "", 1, false, PeopleSearchOptions{})
				require.NoError(t, err)
				require.Equal(t, "Ada\x00李明", first[0].DisplayName)
				for _, store := range []*Store{runtime, recovered} {
					page, _, err := store.PeopleByDisplayName(ctx, "", "", "", 1, false, PeopleSearchOptions{})
					require.NoError(t, err)
					require.Equal(t, first, page)
					identities, err := store.PersonIdentities(ctx, page[0].PersonID)
					require.NoError(t, err)
					require.Len(t, identities, 1)
					require.Equal(t, "claim\x00\\evidence", identities[0].EvidenceID)
					next, _, err := store.PeopleByDisplayName(ctx, "", page[0].DisplayNameFolded, page[0].PersonID, 1, false, PeopleSearchOptions{})
					require.NoError(t, err)
					require.Len(t, next, 1)
					require.Equal(t, "Zoe", next[0].DisplayName)
					recent, _, err := store.PeopleByDisplayName(ctx, "", "", "", 10, false, PeopleSearchOptions{RecentFirst: true})
					require.NoError(t, err)
					require.Len(t, recent, 2)
					require.Equal(t, "Ada\x00李明", recent[0].DisplayName)
					require.NotEmpty(t, recent[0].RecentRank)
					continued, _, err := store.PeopleByDisplayName(ctx, "Ada", recent[0].DisplayNameFolded, recent[0].PersonID, 10, false, PeopleSearchOptions{RecentFirst: true, AfterRecent: recent[0].RecentRank})
					require.NoError(t, err)
					require.Empty(t, continued)
					for prefix, count := range map[string]int{"Ada": 1, "李": 1, "Ada\x00李": 1, "a\x00李": 0, "明": 0} {
						matches, _, err := store.PeopleByDisplayName(ctx, prefix, "", "", 10, false, PeopleSearchOptions{})
						require.NoError(t, err, prefix)
						require.Len(t, matches, count, prefix)
					}
					for _, name := range []string{"Xa\x00Y a\x00Y", "李·明\x00😀Fun"} {
						_, err := store.CreatePerson(ctx, name, "operator")
						require.NoError(t, err)
					}
					for _, prefix := range []string{"a\x00Y", "明", "Fun"} {
						matches, _, err := store.PeopleByDisplayName(ctx, prefix, "", "", 10, false, PeopleSearchOptions{})
						require.NoError(t, err)
						require.Len(t, matches, 1)
					}
				}
				for _, store := range []*Store{runtime, recovered} {
					node, err := store.NodeByPath(ctx, "/claim.txt")
					require.NoError(t, err)
					target := requireDocumentEventTarget(t, store, node.CurrentVersionID)
					record := documentEventRecord(t, store.VaultID(), node.CurrentVersionID, "c9")
					raw, _, err := document.MarshalDocumentEventsV1(record)
					require.NoError(t, err)
					_, err = store.PublishDocumentEvents(ctx, target, DocumentEventsDeriverFingerprint, requireDocumentEventInputsSHA256(t, store, target), raw)
					require.NoError(t, err)
					input, err := store.PrepareDocumentPeopleInputs(ctx, node.CurrentVersionID)
					require.NoError(t, err)
					require.Len(t, input.Custodians, 1)
					claim := input.Custodians[0]
					require.Equal(t, "Ada\x00label", claim.RawLabel)
					publication := DocumentPeoplePublication{People: document.DocumentPeopleV1{ContractVersion: document.PersonContractV1, ContentVersionID: input.ContentVersionID, EventGenerationID: input.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{{PersonID: *claim.PersonID, Role: "custodian", EvidenceKind: "custodian_assignment", EvidenceID: claim.AssignmentID, Confidence: "operator_asserted", Basis: "operator_assigned", RawLabel: claim.RawLabel, ClaimCount: 1}}}, InputsSHA256: fakeHash("c3"), ResolverFingerprint: document.PersonResolverFingerprint(), NodeID: input.NodeID, BindingEpoch: input.BindingEpoch, NodeRevision: input.NodeRevision}
					_, err = store.PublishDocumentPeople(ctx, publication)
					require.NoError(t, err)
					edges, _, err := store.DocumentPeopleForVersion(ctx, node.CurrentVersionID)
					require.NoError(t, err)
					require.Len(t, edges, 1)
					require.Equal(t, "Ada\x00label", edges[0].RawLabel)
				}
				created, err := runtime.CreatePerson(ctx, "New\x00Person", "operator")
				require.NoError(t, err)
				updated, err := runtime.UpdatePerson(ctx, created.PersonID, created.Revision, "Renamed\x00Person")
				require.NoError(t, err)
				got, _, err := runtime.PersonByID(ctx, updated.PersonID)
				require.NoError(t, err)
				require.Equal(t, "Renamed\x00Person", got.DisplayName)
			}
			if name == "NUL extraction" {
				node, err := runtime.NodeByPath(ctx, "/text.txt")
				require.NoError(t, err)
				require.NoError(t, runtime.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "custom\x00extractor", ExtractorVersion: 2, Status: ExtractionOK, Text: "new\x00text"}))
				require.NoError(t, runtime.SeedTextExtractionQueue(ctx, "custom\x00extractor", 2))
				var got string
				require.NoError(t, runtime.db.QueryRowContext(ctx, `SELECT text FROM extracted_text WHERE blob_hash=$1 AND extractor=$2`, node.BlobHash, []byte("custom\x00extractor")).Scan(&got))
				require.Equal(t, "new\x00text", got)
				require.NoError(t, runtime.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "failed\x00extractor", ExtractorVersion: 1, Status: ExtractionFailed, Error: "error\x00detail"}))
				require.NoError(t, runtime.ValidateMetadata(ctx))
			}
			if name == "NUL leading email text" || name == "NUL email text" {
				node, err := source.NodeByPath(ctx, "/source.eml")
				require.NoError(t, err)
				query := document.EmailDocumentRelationQuery{ParentVersionID: node.CurrentVersionID}
				want, err := source.EmailDocumentRelations(ctx, query)
				require.NoError(t, err)
				require.Len(t, want.Items, 1)
				if name == "NUL leading email text" {
					require.Equal(t, "none", want.Items[0].State)
				} else {
					require.Equal(t, "indexed", want.Items[0].State)
				}
				for _, store := range []*Store{runtime, recovered} {
					got, err := store.EmailDocumentRelations(ctx, query)
					require.NoError(t, err)
					require.Equal(t, want, got)
				}
				child := want.Items[0].Relation.Child
				require.NoError(t, runtime.RecordExtraction(ctx, ExtractionResult{BlobHash: child.SHA256, Extractor: legacyPlainTextExtractor, ExtractorVersion: legacyPlainTextExtractorVersion, Status: ExtractionOK, Text: "new\x00body"}))
				got, err := runtime.EmailDocumentRelations(ctx, query)
				require.NoError(t, err)
				require.Equal(t, "indexed", got.Items[0].State)
				require.NoError(t, runtime.ValidateMetadata(ctx))
			}
			if name == "NUL media occurrence" {
				for _, store := range []*Store{runtime, recovered} {
					items, total, _, err := store.MediaOccurrences(ctx, "operator\x00principal", "", "", 10)
					require.NoError(t, err)
					require.Equal(t, 1, total)
					require.Equal(t, "ref\x00original", items[0].Ref)
					require.Equal(t, "revision\x001", items[0].Revision)
					require.Equal(t, "recording\x00name.mp3", items[0].Filename)
					require.Equal(t, "person\x00ref", items[0].PersonRef)
					require.Equal(t, "speaker\x00label", items[0].SpeakerLabel)
					in := MediaOccurrenceInput{ID: items[0].OccurrenceID, SourceID: items[0].SourceID, SourceVersionID: items[0].SourceVersionID, Principal: "operator\x00principal", Ref: items[0].Ref, Revision: items[0].Revision, Filename: items[0].Filename, PersonRef: items[0].PersonRef, SpeakerLabel: items[0].SpeakerLabel, MessageJSON: items[0].MessageJSON}
					// A native exact replay must find the binary caller revision.
					_, err = store.RecordMediaOccurrence(ctx, testMediaOperation("declare_occurrence", in.Principal), in)
					require.NoError(t, err)
					_, err = store.RecordMediaOccurrenceRevocation(ctx, testMediaOperation("revoke_occurrence", in.Principal), in.ID, in.Revision)
					require.NoError(t, err)
					_, err = store.MediaOccurrence(ctx, in.Principal, in.ID)
					require.ErrorIs(t, err, ErrNotFound)
				}
			}
			if name == "NUL operational photo source" {
				node, err := source.NodeByPath(ctx, "/nul-photo.jpg")
				require.NoError(t, err)
				want, err := source.NodeProvenance(ctx, node.ID, 10, 0)
				require.NoError(t, err)
				require.Len(t, want.Items, 1)
				for _, store := range []*Store{runtime, recovered} {
					got, err := store.NodeProvenance(ctx, node.ID, 10, 0)
					require.NoError(t, err)
					require.Equal(t, want, got)
					collections, total, err := store.Collections(ctx, 10, 0)
					require.NoError(t, err)
					require.Equal(t, 1, total)
					require.Equal(t, "cli\x00kind", collections[0].SourceKind)
					var path string
					require.NoError(t, store.db.QueryRowContext(ctx, `SELECT original_path FROM photo_source_claims`).Scan(&path))
					require.Equal(t, "/source/\x00reference/photo.jpg", path)
				}
				run, err := runtime.BeginIngest(ctx, "cli\x00kind", "operational source")
				require.NoError(t, err)
				_, err = runtime.IngestFileExact(ctx, run, runtime.RootID(), "new-photo.jpg", fakeHash("b7"), 9, "image/jpeg", "/source/\x00reference/new.jpg", "2024-03-05T10:00:00Z")
				require.NoError(t, err)
				_, err = runtime.VerifyAudit(ctx, &before.Evidence)
				require.NoError(t, err)
			}
			if name == "NUL source reference" || name == "NUL caller source kind" {
				node, err := source.NodeByPath(ctx, "/nul.jpg")
				require.NoError(t, err)
				want, err := source.NodeProvenance(ctx, node.ID, 10, 0)
				require.NoError(t, err)
				require.Len(t, want.Items, 1)
				for _, store := range []*Store{runtime, recovered} {
					got, err := store.NodeProvenance(ctx, node.ID, 10, 0)
					require.NoError(t, err)
					require.Equal(t, want, got)
					collections, total, err := store.Collections(ctx, 10, 0)
					require.NoError(t, err)
					require.Zero(t, total)
					require.Empty(t, collections)
				}
				_, err = runtime.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: node.Revision, SourceKind: "new\x00kind", SourceDescription: "new source", OriginalPath: "opaque://new\x00reference"})
				require.NoError(t, err)
				got, err := runtime.NodeProvenance(ctx, node.ID, 10, 0)
				require.NoError(t, err)
				require.Len(t, got.Items, 2)
				require.Equal(t, "new\x00kind", got.Items[0].SourceKind)
				require.Equal(t, "opaque://new\x00reference", got.Items[0].OriginalPath)
				_, err = runtime.VerifyAudit(ctx, &before.Evidence)
				require.NoError(t, err)
			}
			if name == "NUL source description" {
				node, err := source.NodeByPath(ctx, "/nul.txt")
				require.NoError(t, err)
				want, err := source.NodeProvenance(ctx, node.ID, 10, 0)
				require.NoError(t, err)
				require.Equal(t, "source\x00description \\u0000 café", want.Items[0].SourceDescription)
				collection, err := source.CollectionByID(ctx, want.Items[0].IngestID)
				require.NoError(t, err)
				for _, store := range []*Store{runtime, recovered} {
					got, err := store.NodeProvenance(ctx, node.ID, 10, 0)
					require.NoError(t, err)
					require.Equal(t, want, got)
					collections, total, err := store.Collections(ctx, 10, 0)
					require.NoError(t, err)
					require.Equal(t, 1, total)
					require.Equal(t, []Collection{collection}, collections)
				}
				_, err = runtime.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent", SourceDescription: "new\x00source", OriginalPath: "opaque://new-reference"})
				require.NoError(t, err)
				got, err := runtime.NodeProvenance(ctx, node.ID, 10, 0)
				require.NoError(t, err)
				require.Len(t, got.Items, 2)
				require.Equal(t, "new\x00source", got.Items[0].SourceDescription)
				_, err = runtime.VerifyAudit(ctx, &before.Evidence)
				require.NoError(t, err)
			}
			if name == "hidden photo lockout" {
				_, _, err := runtime.UnlockPhotoHidden(ctx, "wrong-passcode")
				require.Error(t, err)
				var count, lastID int64
				require.NoError(t, runtime.db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(failure_id) FROM photo_hidden_failures`).Scan(&count, &lastID))
				require.Equal(t, int64(5), count)
				require.Equal(t, int64(5), lastID)
			}
			if name == "candidate faces" {
				visible, _, err := runtime.PersonCandidates(ctx, "open", 10, 0)
				require.NoError(t, err)
				require.Len(t, visible, 1)
				node, err := runtime.NodeByPath(ctx, "/portrait.jpg")
				require.NoError(t, err)
				asset, err := runtime.PhotoAssetForNode(ctx, node.ID)
				require.NoError(t, err)
				_, err = runtime.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
				visible, _, err = runtime.PersonCandidates(ctx, "open", 10, 0)
				require.NoError(t, err)
				require.Empty(t, visible, "restored candidate face membership must hide evidence from hidden photos")
			}
			if name == "audit and moved parents" {
				sequence, err := source.auditNodeSequence(ctx, source.db)
				require.NoError(t, err)
				node, err := runtime.Mkdir(ctx, runtime.RootID(), "after-transfer")
				require.NoError(t, err)
				require.Equal(t, sequence+1, node.ID)
				_, err = runtime.VerifyAudit(ctx, &before.Evidence)
				require.NoError(t, err)
			}
		})
	}
}

func metadataImportRecordTypes(t *testing.T, input []byte) []string {
	t.Helper()
	var kinds []string
	for line := range bytes.SplitSeq(bytes.TrimSpace(input), []byte{'\n'}) {
		var record struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &record))
		kinds = append(kinds, record.Type)
	}
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

func seedMetadataImportMailbox(t *testing.T) *Store {
	t.Helper()
	ctx := t.Context()
	s := newTestStore(t)
	email := newEmailFixture(t, s, "seed.eml")
	source, err := emailVersion(ctx, s.db, email.publication.ContentVersionID)
	require.NoError(t, err)
	c := MailboxContainerRequest{ID: "source", Owner: "operator", SHA256: source.BlobHash, Size: source.Size, Format: "mbox"}
	_, err = s.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, s.PutMailboxChunk(ctx, c.Owner, c.ID, MailboxChunk{Index: 0, SHA256: c.SHA256, Size: c.Size}))
	_, err = s.SealMailboxContainer(ctx, c.Owner, c.ID, c.SHA256, c.Size)
	require.NoError(t, err)
	_, err = s.BeginMailboxJob(ctx, c.Owner, MailboxJobRequest{ID: "job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: MailboxSettings{DestinationID: s.RootID()}})
	require.NoError(t, err)
	job, err := s.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "mailbox:" + job.CollectionID, Owner: c.Owner, Description: "Synthetic archive"}))
	settings, err := job.Settings.Canonical()
	require.NoError(t, err)
	location := MailboxLocation{ContainerID: c.ID, Entry: "source.mbox", EntrySHA256: c.SHA256, Sequence: 1, End: c.Size, RawSHA256: c.SHA256, EMLSHA256: c.SHA256, EMLSize: c.Size, Separator: "From synthetic", Labels: []string{}}
	occurrence := MailboxOccurrence{JobID: job.ID, Ordinal: 1, Location: location}
	require.NoError(t, s.StartMailboxOccurrence(ctx, job.ID, job.Claim, occurrence))
	publication := MailboxTransferPublication{Owner: c.Owner, Request: MailboxTransferRequest{ArchiveID: "mailbox:" + job.CollectionID, Reference: "0:1", SHA256: c.SHA256, Size: c.Size, Settings: settings, DestinationID: s.RootID(), Name: "message.eml"}, Email: email.publication, Location: &location}
	require.NoError(t, s.CommitMailboxOccurrence(ctx, job.ID, job.Claim, occurrence, &publication))
	require.NoError(t, s.FinishMailboxJob(ctx, job.ID, job.Claim, "complete", "", true))
	return s
}

func seedMetadataImportSuggestions(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	asset := albumAsset(t, s, "partly-decided.jpg")
	proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "sunset"}})
	acceptPhotoSuggestion(t, s, proposal, true, "coast")
	asset = albumAsset(t, s, "dismissed.jpg")
	proposal = putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A river."})
	_, err := s.RespondPhotoSuggestion(t.Context(), proposal.ID, dismissalOf(t.Context(), t, s, proposal))
	require.NoError(t, err)
	asset = albumAsset(t, s, "settled.jpg")
	proposal = putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A mountain."})
	acceptPhotoSuggestion(t, s, proposal, true)
	return s
}

func seedMetadataImportPages(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	request := pageStoreRequest(t, s)
	id, err := newUUIDv4()
	require.NoError(t, err)
	job, err := s.QueuePageJob(t.Context(), id, request)
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	frames := pageStoreFrames(t, request)
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, frames))
	recipe := pageStoreRecipe()
	require.NoError(t, s.PublishPageImage(t.Context(), claim, pageStoreImage(t, frames[0], recipe), recipe, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 10}))
	require.NoError(t, s.CancelPageJob(t.Context(), job.ID, request.Binding()))
	return s
}

type failedMetadataReader struct{ err error }

func (r failedMetadataReader) Read([]byte) (int, error) { return 0, r.err }

func checkMetadataImportRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	source := newTestStore(t)
	_, err := source.CreateFile(t.Context(), source.RootID(), "retained.txt", fakeHash("a1"), 9, "text/plain")
	require.NoError(t, err)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &input))
	for name, late := range map[string]bool{"interrupted stream": false, "late allocation write": true} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			setup, owner := pgtest.NewDatabase(t)
			cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
			before, err := pgschema.Setup(ctx, setup, cfg)
			require.NoError(t, err)
			var broken = io.MultiReader(bytes.NewReader(input.Bytes()), failedMetadataReader{errors.New("source interrupted")})
			writer := setup
			injected := false
			var injectionErr error
			if late {
				conn, err := setup.Conn(ctx)
				require.NoError(t, err)
				var config *pgx.ConnConfig
				require.NoError(t, conn.Raw(func(raw any) error {
					native, ok := raw.(*stdlib.Conn)
					require.True(t, ok)
					config = native.Conn().Config().Copy()
					return nil
				}))
				require.NoError(t, conn.Close())
				config.Tracer = renditionQueryStart(func(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) {
					if !injected && strings.HasPrefix(data.SQL, "UPDATE node_allocation SET high_water=") {
						injectionErr = setup.QueryRowContext(ctx, `SELECT pg_terminate_backend($1,5000)`, conn.PgConn().PID()).Scan(&injected)
					}
				})
				writer = stdlib.OpenDB(*config)
				t.Cleanup(func() { require.NoError(t, writer.Close()) })
				broken = bytes.NewReader(input.Bytes())
			}
			require.Error(t, ImportPostgresMetadata(ctx, writer, cfg, broken))
			if late {
				require.NoError(t, injectionErr)
				require.True(t, injected)
			}

			after, err := pgschema.Setup(ctx, setup, cfg)
			require.NoError(t, err)
			require.Equal(t, before, after)
			var nodes, blobs, highWater int64
			require.NoError(t, setup.QueryRowContext(ctx, `SELECT
				(SELECT COUNT(*) FROM documents.nodes), (SELECT COUNT(*) FROM documents.blobs),
				(SELECT high_water FROM documents.node_allocation WHERE singleton=1)`).Scan(&nodes, &blobs, &highWater))
			require.Equal(t, int64(1), nodes)
			require.Zero(t, blobs)
			require.Equal(t, int64(1), highWater)

			require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
		})
	}
}

func checkMetadataImportAdmissionPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &input))
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	setup.SetMaxOpenConns(1)
	var originalPath string
	require.NoError(t, setup.QueryRowContext(ctx, `SHOW search_path`).Scan(&originalPath))
	runtime := newPostgresSchemaStore(t, setup, cfg)
	require.ErrorContains(t, ImportPostgresMetadata(ctx, runtime.db, cfg, bytes.NewReader(input.Bytes())), "schema owner role")
	_, err := runtime.Mkdir(ctx, runtime.RootID(), "keep-this")
	require.NoError(t, err)
	var before, after bytes.Buffer
	require.NoError(t, runtime.ExportMetadata(ctx, &before))
	require.ErrorContains(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())), "not pristine")
	require.NoError(t, runtime.ExportMetadata(ctx, &after))
	require.Equal(t, before.String(), after.String())
	var path string
	require.NoError(t, setup.QueryRowContext(ctx, `SHOW search_path`).Scan(&path))
	require.Equal(t, originalPath, path, "the caller's pool search path stays unchanged")
	require.Zero(t, setup.Stats().InUse)
}

func checkMetadataImportKeepsFreshConsentPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	ctx := t.Context()
	source := newTestStore(t)
	request := testProviderAuthorizationRequest()
	_, err := source.GrantConsent(ctx, grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	_, err = source.AuthorizeProviderOperation(ctx, request)
	require.NoError(t, err)
	var input bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &input))
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	require.NoError(t, ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(input.Bytes())))
	runtime := newPostgresSchemaStore(t, setup, cfg)
	_, err = runtime.AuthorizeProviderOperation(ctx, request)
	require.ErrorIs(t, err, ErrProcessingConsentRequired)
	_, err = runtime.GrantConsent(ctx, grantRequestForAuthorization(request, nil))
	require.NoError(t, err)
	_, err = runtime.AuthorizeProviderOperation(ctx, request)
	require.NoError(t, err)
}
