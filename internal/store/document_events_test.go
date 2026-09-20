package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestEventControlStateIsLazy(t *testing.T) {
	s := newTestStore(t)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_state`).Scan(&count))
	require.Zero(t, count)
	_, err := s.db.Exec(`INSERT INTO document_event_state(
		singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at
	) VALUES(1,'document-events/v1','test',1,1,'2020-01-01T00:00:00Z')`)
	require.NoError(t, err)
}

func TestPublishDocumentEventsIsIdempotentAndDetectsCorruption(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	version, target := ingestDocumentEventTarget(t, s, "report.txt", "a1")
	record := documentEventRecord(t, s.VaultID(), version.ID, "a")
	canonical, checksum, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)

	_, err = s.db.Exec(`INSERT INTO document_event_state(
		singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at
	) VALUES(1,?,?,?,?,?)`, document.DocumentEventsContractV1, fakeHash("f1"), 1, 1,
		"2020-01-01T00:00:00Z")
	require.NoError(t, err)
	inputsSHA256 := requireDocumentEventInputsSHA256(t, s, target)

	first, err := s.PublishDocumentEvents(ctx, target, fakeHash("f1"), inputsSHA256, canonical)
	require.NoError(t, err)
	assert.Equal(t, checksum, first.Checksum)
	firstView, err := s.DocumentEventsForVersion(ctx, version.ID)
	require.NoError(t, err)
	assert.Equal(t, record, firstView.Events)
	assert.Equal(t, target.InputEpoch, firstView.InputEpoch)

	second, err := s.PublishDocumentEvents(ctx, target, fakeHash("f1"), inputsSHA256, canonical)
	require.NoError(t, err)
	assert.Equal(t, first.GenerationID, second.GenerationID)
	assert.Equal(t, first.Checksum, second.Checksum)
	assert.Equal(t, first.CreatedAt, second.CreatedAt)
	secondView, err := s.DocumentEventsForVersion(ctx, version.ID)
	require.NoError(t, err)
	assert.Equal(t, firstView.PublishedAt, secondView.PublishedAt)
	var publicationEpoch int64
	require.NoError(t, s.db.QueryRow(`SELECT publication_epoch FROM document_event_state WHERE singleton=1`).Scan(&publicationEpoch))
	assert.Equal(t, int64(2), publicationEpoch, "unchanged replay must not advance the publication epoch")

	_, err = s.db.Exec(`UPDATE document_event_generations SET checksum=? WHERE generation_id=?`,
		fakeHash("ff"), first.GenerationID)
	require.Error(t, err, "generation parent rows must be immutable")

	otherVersion, otherTarget := ingestDocumentEventTarget(t, s, "other.txt", "b2")
	otherRecord := documentEventRecord(t, s.VaultID(), otherVersion.ID, "b")
	otherCanonical, otherChecksum, err := document.MarshalDocumentEventsV1(otherRecord)
	require.NoError(t, err)
	otherInputsSHA256 := requireDocumentEventInputsSHA256(t, s, otherTarget)
	otherID := document.DocumentEventGenerationID(s.VaultID(), otherVersion.ID,
		document.DocumentEventsContractV1, fakeHash("f1"), otherInputsSHA256)
	_, err = s.db.Exec(`INSERT INTO document_event_generations(
		generation_id,content_version_id,contract_version,deriver_fingerprint,inputs_sha256,
		document_kind,canonical_json,checksum,event_count,created_at
	) VALUES(?,?,?,?,?,?,?,?,?,?)`, otherID, otherVersion.ID, document.DocumentEventsContractV1,
		fakeHash("f1"), otherInputsSHA256, "other",
		append(otherCanonical, '\n'), otherChecksum, len(otherRecord.Events), "2020-01-01T00:00:00Z")
	require.NoError(t, err)
	_, err = s.PublishDocumentEvents(ctx, otherTarget, fakeHash("f1"), otherInputsSHA256, otherCanonical)
	require.ErrorIs(t, err, ErrDocumentEventsCorrupt)
	_, err = s.db.Exec(`DELETE FROM nodes WHERE id=?`, otherVersion.NodeID)
	require.NoError(t, err)

	_, err = s.db.Exec(`DROP TRIGGER document_event_generations_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE document_event_generations SET checksum='doctored' WHERE generation_id=?`, first.GenerationID)
	require.NoError(t, err)
	_, err = s.DocumentEventsForVersion(ctx, version.ID)
	require.ErrorIs(t, err, ErrDocumentEventsCorrupt)
	_, err = s.PublishDocumentEvents(ctx, target, fakeHash("f1"), inputsSHA256, canonical)
	require.ErrorIs(t, err, ErrDocumentEventsCorrupt)

	_, err = s.db.Exec(`INSERT INTO document_event_dirty(content_version_id,revision,reason)
		VALUES(?,1,'test') ON CONFLICT(content_version_id) DO UPDATE SET
		revision=revision+1,reason=excluded.reason`, version.ID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE document_event_attempts SET
		input_epoch=?,input_revision=?,inputs_sha256=?,state=?,diagnostic_json=?,attempted_at=?
		WHERE content_version_id=?`, target.InputEpoch, target.InputRevision, inputsSHA256,
		"failed", []byte(`[]`), "2020-01-01T00:00:00Z", version.ID)
	require.NoError(t, err)
	_, err = s.db.Exec(`DELETE FROM nodes WHERE id=?`, version.NodeID)
	require.NoError(t, err)
	for _, check := range []struct {
		query string
		key   string
	}{
		{`SELECT count(*) FROM document_event_generations WHERE generation_id=?`, first.GenerationID},
		{`SELECT count(*) FROM document_event_heads WHERE content_version_id=?`, version.ID},
		{`SELECT count(*) FROM document_event_dirty WHERE content_version_id=?`, version.ID},
		{`SELECT count(*) FROM document_event_attempts WHERE content_version_id=?`, version.ID},
	} {
		require.NoError(t, s.db.QueryRow(check.query, check.key).Scan(&publicationEpoch), check.query)
		assert.Zero(t, publicationEpoch, check.query)
	}
}

func TestPublishDocumentEventsRejectsMismatchedTargetIdentity(t *testing.T) {
	s := newTestStore(t)
	version, target := ingestDocumentEventTarget(t, s, "identity.txt", "c3")
	record := documentEventRecord(t, s.VaultID(), version.ID, "c")
	canonical, _, err := document.MarshalDocumentEventsV1(record)
	require.NoError(t, err)

	tests := []struct {
		name      string
		target    DocumentEventTarget
		canonical []byte
	}{
		{name: "blob", target: func() DocumentEventTarget { v := target; v.BlobHash = fakeHash("wrong"); return v }(), canonical: canonical},
		{name: "node", target: func() DocumentEventTarget { v := target; v.NodeID++; return v }(), canonical: canonical},
		{name: "size", target: func() DocumentEventTarget { v := target; v.Size++; return v }(), canonical: canonical},
		{name: "media type", target: func() DocumentEventTarget { v := target; v.MIMEType = "application/octet-stream"; return v }(), canonical: canonical},
		{name: "recorded at", target: func() DocumentEventTarget { v := target; v.RecordedAt = "2000-01-01T00:00:00Z"; return v }(), canonical: canonical},
	}
	wrongVault := documentEventRecord(t, "another-vault", version.ID, "c")
	tests = append(tests, struct {
		name      string
		target    DocumentEventTarget
		canonical []byte
	}{name: "vault", target: target, canonical: mustMarshalDocumentEvents(t, wrongVault)})
	inputsSHA256 := requireDocumentEventInputsSHA256(t, s, target)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.PublishDocumentEvents(t.Context(), tt.target, fakeHash("f1"), inputsSHA256, tt.canonical)
			require.Error(t, err)
		})
	}
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM document_event_generations`).Scan(&count))
	assert.Zero(t, count)
}

func TestPublishDocumentEventsAcceptsCanonicalEmptyProjection(t *testing.T) {
	s := newTestStore(t)
	version, target := ingestDocumentEventTarget(t, s, "undated.txt", "d4")
	record := document.DocumentEventsV1{
		VaultUID: s.VaultID(), ContentVersionID: version.ID,
		ContractVersion: document.DocumentEventsContractV1, DocumentKind: "other",
		Diagnostics: []document.DocumentEventDiagnosticV1{},
		Events:      []document.DocumentEventV1{},
		Primaries:   []document.DocumentEventPrimaryV1{},
		Sources:     []document.DocumentEventSourceV1{},
	}
	canonical := mustMarshalDocumentEvents(t, record)

	_, err := s.PublishDocumentEvents(t.Context(), target, fakeHash("f1"),
		requireDocumentEventInputsSHA256(t, s, target), canonical)
	require.NoError(t, err)
	view, err := s.DocumentEventsForVersion(t.Context(), version.ID)
	require.NoError(t, err)
	assert.Equal(t, record, view.Events)
}

func TestPublishDocumentEventsRetainsActorEvidenceInProjections(t *testing.T) {
	s := newTestStore(t)
	version, target := ingestDocumentEventTarget(t, s, "actors.txt", "a71")
	record := documentEventRecord(t, s.VaultID(), version.ID, "a7")
	record.Events[0].Actors[0].EvidenceKind = "email_generation"
	record.Events[0].Actors[0].EvidenceID = "email/undated-author"
	record.Sources = append(record.Sources, document.DocumentEventSourceV1{
		EvidenceKind: "email_generation", EvidenceID: "email/undated-author",
		EvidenceSHA256: fakeHash("c7"),
	})
	_, err := s.PublishDocumentEvents(t.Context(), target, fakeHash("f71"),
		requireDocumentEventInputsSHA256(t, s, target), mustMarshalDocumentEvents(t, record))
	require.NoError(t, err)

	var eventEvidence, actorEvidence, actorEvidenceID, primaryEventID string
	err = s.db.QueryRowContext(t.Context(), `SELECT e.evidence_kind,a.evidence_kind,a.evidence_id,p.event_id
		FROM document_event_heads h
		JOIN document_events e ON e.generation_id=h.generation_id
		JOIN document_event_actors a ON a.generation_id=e.generation_id AND a.event_id=e.event_id
		JOIN document_event_primaries p ON p.generation_id=e.generation_id AND p.event_id=e.event_id
		WHERE h.content_version_id=?`, version.ID).
		Scan(&eventEvidence, &actorEvidence, &actorEvidenceID, &primaryEventID)
	require.NoError(t, err)
	require.Equal(t, "source_metadata", eventEvidence)
	require.Equal(t, "email_generation", actorEvidence)
	require.Equal(t, "email/undated-author", actorEvidenceID)
	require.Equal(t, record.Events[0].EventID, primaryEventID)
}

func TestDocumentEventsForVersionRejectsHeadFromAnotherVersion(t *testing.T) {
	s := newTestStore(t)
	firstVersion, firstTarget := ingestDocumentEventTarget(t, s, "first.txt", "e5")
	secondVersion, secondTarget := ingestDocumentEventTarget(t, s, "second.txt", "f6")
	firstCanonical := mustMarshalDocumentEvents(t,
		documentEventRecord(t, s.VaultID(), firstVersion.ID, "a"))
	secondCanonical := mustMarshalDocumentEvents(t,
		documentEventRecord(t, s.VaultID(), secondVersion.ID, "b"))
	_, err := s.PublishDocumentEvents(t.Context(), firstTarget, fakeHash("f1"),
		requireDocumentEventInputsSHA256(t, s, firstTarget), firstCanonical)
	require.NoError(t, err)
	secondGeneration, err := s.PublishDocumentEvents(t.Context(), secondTarget,
		fakeHash("f1"), requireDocumentEventInputsSHA256(t, s, secondTarget), secondCanonical)
	require.NoError(t, err)

	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `DELETE FROM document_event_heads WHERE content_version_id=?`,
		secondVersion.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE document_event_heads SET generation_id=?
		WHERE content_version_id=?`, secondGeneration.GenerationID, firstVersion.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	_, err = s.DocumentEventsForVersion(t.Context(), firstVersion.ID)
	require.ErrorIs(t, err, ErrDocumentEventsCorrupt)
}

func ingestDocumentEventTarget(t *testing.T, s *Store, name, hashSeed string) (ContentVersion, DocumentEventTarget) {
	t.Helper()
	run, err := s.BeginIngest(t.Context(), "test", "/synthetic")
	require.NoError(t, err)
	node, _, err := s.IngestFile(t.Context(), run, s.RootID(), name, fakeHash(hashSeed), 12,
		"text/plain", "/synthetic/"+name, "2024-01-02T03:04:05Z")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	var inputRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT COALESCE((SELECT revision
		FROM document_event_dirty WHERE content_version_id=?),0)`, version.ID).Scan(&inputRevision))
	return version, DocumentEventTarget{
		ContentVersionID: version.ID, BlobHash: version.BlobHash, MIMEType: version.MimeType,
		RecordedAt: version.RecordedAt, NodeID: version.NodeID, Size: version.Size,
		InputEpoch: 1, InputRevision: inputRevision,
	}
}

func documentEventRecord(t *testing.T, vaultID, versionID, digestSeed string) document.DocumentEventsV1 {
	t.Helper()
	digest := fakeHash(digestSeed)
	event := document.DocumentEventV1{
		Actors: []document.DocumentEventActorV1{{
			ActorKey: "email:ada@example.test", Address: "ada@example.test",
			Claim: `{"address":"ada@example.test"}`, DisplayName: "Ada", Ordinal: 0,
			Role: "author",
		}},
		AxisKey: "2019-03-04T00:00:00.000000000", ClaimBasis: "source_asserted",
		DateKind: "created", DateValue: "2019-03-04", EvidenceID: "metadata/" + digestSeed,
		EvidenceKind: "source_metadata", EvidenceLocator: "field:created",
		EvidenceSHA256: digest, ParseConfidence: "exact", Precision: "date",
		RawValue: "2019-03-04", SourceKey: "metadata/" + digestSeed + "/created",
		TimezoneKind: "omitted",
	}
	event.EventID = document.DocumentEventID(vaultID, versionID, event.SourceKey, event.DateKind,
		event.DateValue, event.Precision, event.TimezoneKind, event.EvidenceSHA256)
	return document.DocumentEventsV1{
		VaultUID: vaultID, ContentVersionID: versionID,
		ContractVersion: document.DocumentEventsContractV1, DocumentKind: "other",
		Diagnostics: []document.DocumentEventDiagnosticV1{},
		Events:      []document.DocumentEventV1{event},
		Primaries: []document.DocumentEventPrimaryV1{{
			EventID: event.EventID, Reason: "created", RuleID: "primary-rule/v1",
			ScopeClass: "vault", Disclosure: "full",
		}},
		Sources: []document.DocumentEventSourceV1{{
			EvidenceID: event.EvidenceID, EvidenceKind: event.EvidenceKind,
			EvidenceSHA256: digest,
		}},
	}
}

func mustMarshalDocumentEvents(t *testing.T, value document.DocumentEventsV1) []byte {
	t.Helper()
	canonical, _, err := document.MarshalDocumentEventsV1(value)
	require.NoError(t, err)
	return canonical
}

func requireDocumentEventInputsSHA256(t *testing.T, s *Store, target DocumentEventTarget) string {
	t.Helper()
	snapshot, err := s.LoadDocumentEventEvidence(t.Context(), target)
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.InputsSHA256)
	return snapshot.InputsSHA256
}
