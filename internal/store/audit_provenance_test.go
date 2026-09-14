package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/audit"
)

func TestAuditedProvenanceAppendAndSupersessionRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	node, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)

	mtime := "2026-08-26T12:00:00Z"
	first, err := s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "triage", OriginalPath: "opaque://report", OriginalMTime: &mtime,
	})
	require.NoError(t, err)
	assert.Equal(t, node.Revision+1, first.Node.Revision)
	assert.Equal(t, "provenance_add", auditEventKindForSequence(t, s, 2))

	second, err := s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: first.Node.Revision, SourceKind: "agent",
		SourceDescription: "reconcile", OriginalPath: "opaque://report-corrected",
		Supersedes: &first.Fact.Identity,
	})
	require.NoError(t, err)
	assert.Equal(t, first.Node.Revision+1, second.Node.Revision)
	assert.Equal(t, "provenance_supersede", auditEventKindForSequence(t, s, 3))
	page, err := s.NodeProvenance(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 3)
	assert.False(t, findProvenanceFact(page.Items, first.Fact.Identity).Active)
	assert.True(t, findProvenanceFact(page.Items, second.Fact.Identity).Active)
	history, err := s.AuditHistory(t.Context(), node.ID, 10, "")
	require.NoError(t, err)
	var supersession AuditEvent
	for _, event := range history.Items {
		if event.Kind == "provenance_supersede" {
			supersession = event
			break
		}
	}
	require.NotNil(t, supersession.Attachment)
	require.NotNil(t, supersession.Attachment.Before)
	require.NotNil(t, supersession.Attachment.After)
	assert.Equal(t, first.Fact.Identity, supersession.Attachment.Before.ProvenanceID)
	assert.Equal(t, second.Fact.Identity, supersession.Attachment.After.ProvenanceID)
	require.NoError(t, s.ValidateMetadata(t.Context()))

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
}

func TestAuditedProvenanceAppendRollsBackAllMetadata(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	node, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	require.NoError(t, createAuditScopeFailureTrigger(s))

	_, err = s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "rollback", OriginalPath: "opaque://rollback",
	})
	require.ErrorContains(t, err, "forced audited provenance failure")
	unchanged, err := s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.Revision, unchanged.Revision)
	var ingests, provenance, sequence int64
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM ingests WHERE source_desc=?),
		(SELECT COUNT(*) FROM provenance WHERE original_path=?),
		(SELECT operation_sequence_high_water FROM audit_authority)`,
		"rollback", "opaque://rollback").Scan(&ingests, &provenance, &sequence))
	assert.Zero(t, ingests)
	assert.Zero(t, provenance)
	assert.Equal(t, int64(1), sequence)
}

func TestAuditedProvenanceReplayRejectsOperationalPredecessor(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginCallerSuppliedIngest(ctx, "agent", "source")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(), "report.txt", fakeHash("a1"),
		7, "text/plain", "/source/report.txt", "")
	require.NoError(t, err)
	page, err := s.NodeProvenance(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "correction", OriginalPath: "opaque://corrected",
		Supersedes: &page.Items[0].Identity,
	})
	require.NoError(t, err)

	authority, scope, err := loadInitialAuditProjection(ctx, s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(ctx, s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, scope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, scope, initial)
	require.NoError(t, err)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], authority.sequence)
	require.NoError(t, err)
	mutation := mutations[2]
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	require.NoError(t, err)
	deltas := auditRecordsByDigest(records["attached_metadata_delta"])
	_, err = replay.validateProvenanceAppendDelta(mutation.record, operationID, deltas, map[string]bool{})
	require.NoError(t, err)

	// Keep the same predecessor identity and supersession delta, but make
	// its baseline ingest operational. Replay must enforce the write policy.
	operationalKind, err := audit.Text("cli")
	require.NoError(t, err)
	changed := false
	for key, record := range replay.attachments {
		if record.Kind != metadataIngestType {
			continue
		}
		ingestID, err := auditUUIDField(record, "ingest_id")
		require.NoError(t, err)
		if ingestID != run.ID() {
			continue
		}
		replay.attachments[key], err = replaceAuditRecordField(record, "source_kind", operationalKind)
		require.NoError(t, err)
		changed = true
	}
	require.True(t, changed)
	_, err = replay.validateProvenanceAppendDelta(mutation.record, operationID, deltas, map[string]bool{})
	require.ErrorContains(t, err, "operational ingest provenance cannot be superseded")
}

func TestAuditedProvenanceReplayRejectsTamperedIngestIdentity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	node, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	_, err = s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "tamper", OriginalPath: "opaque://source",
	})
	require.NoError(t, err)

	authority, auditScope, err := loadInitialAuditProjection(t.Context(), s.db)
	require.NoError(t, err)
	records, err := loadInitialAuditRecords(t.Context(), s.db)
	require.NoError(t, err)
	initial, err := selectInitialAuditRecords(authority, auditScope, records)
	require.NoError(t, err)
	replay, err := newAuditedHistoryReplay(authority, auditScope, initial)
	require.NoError(t, err)
	mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], authority.sequence)
	require.NoError(t, err)
	mutation := mutations[2]
	deltas := auditRecordsByDigest(records["attached_metadata_delta"])
	digest, err := auditDigestField(mutation.record, "attached_metadata_change_digest")
	require.NoError(t, err)
	delta := deltas[digest]
	changes, err := auditRecordListField(delta.record, "changes")
	require.NoError(t, err)
	var appendedIngest audit.Record
	for _, change := range changes {
		kind, kindErr := auditTextField(change, "record_kind")
		require.NoError(t, kindErr)
		if kind == metadataIngestType {
			appendedIngest, err = validateAuditedIngestAddition(change)
			require.NoError(t, err)
		}
	}
	ingestKey, err := attachedAuditKey(appendedIngest)
	require.NoError(t, err)
	replay.attachments[ingestKey] = appendedIngest
	used := map[string]bool{}
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	require.NoError(t, err)
	_, err = replay.validateProvenanceAppendDelta(mutation.record, operationID, deltas, used)
	require.ErrorContains(t, err, "provenance mutation reuses ingest identity")
	delete(replay.attachments, ingestKey)
	for index, change := range changes {
		kind, kindErr := auditTextField(change, "record_kind")
		require.NoError(t, kindErr)
		if kind != metadataIngestType {
			continue
		}
		tampered, tamperErr := replaceAuditRecordField(change, "stable_identity", audit.Nested(audit.Record{
			Kind: "tampered_identity",
		}))
		require.NoError(t, tamperErr)
		changes[index] = tampered
	}
	delta.record, err = replaceAuditRecordField(delta.record, "changes", audit.List(auditNestedValues(changes)...))
	require.NoError(t, err)
	deltas[digest] = delta
	used = map[string]bool{}
	_, err = replay.validateProvenanceAppendDelta(mutation.record, operationID, deltas, used)
	require.ErrorContains(t, err, "audited ingest attachment identity does not match its post record")
}

func TestAuditedProvenanceImportRejectsTamperedFact(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	seedMetadataRoundTrip(t, s)
	scope, err := s.NodeByPath(t.Context(), "/Projects")
	require.NoError(t, err)
	node, err := s.NodeByPath(t.Context(), "/Projects/report.txt")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	_, err = s.AppendNodeProvenance(t.Context(), ProvenanceAppendInput{
		NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
		SourceDescription: "tamper", OriginalPath: "opaque://original",
	})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	malformed := rewriteAppendedProvenancePath(t, exported.Bytes(), node.ID)

	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(malformed))
	require.ErrorContains(t, err, "replayed audit attachments do not match current metadata")
	var auditRows int64
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
	assert.Zero(t, auditRows)
}

func TestAuditedProvenanceImportRequiresCallerSuppliedIngest(t *testing.T) {
	for _, test := range []struct {
		kind  string
		valid bool
	}{
		{kind: "cli"},
		{kind: "watch"},
		{kind: "embedded:"},
		{kind: "embedded:agent", valid: true},
		{kind: "embedded:watch", valid: true},
	} {
		t.Run(test.kind, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), "report.txt", fakeHash("a1"), 7, "text/plain")
			require.NoError(t, err)
			seedInitialAuditAuthority(t, s, s.RootID())
			run, err := s.BeginIngest(ctx, test.kind, "source")
			require.NoError(t, err)

			// Build a correctly hashed append with the supplied stored kind,
			// bypassing the writer that always adds the caller-supplied prefix.
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				if _, err := ensureIngestRunTx(ctx, tx, run); err != nil {
					return err
				}
				fact := metadataProvenance{
					Type: metadataProvenanceType, NodeID: node.ID, IngestID: run.ID(),
					OriginalPath: "report.txt",
				}
				fact.Identity, err = provenanceIdentity(fact)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`,
					fact.Identity, fact.NodeID, fact.IngestID, fact.OriginalPath); err != nil {
					return err
				}
				if test.kind == "watch" {
					if err := insertWatchSourceTx(tx, "source", fact.OriginalPath, node.ID, node.BlobHash, node.Size); err != nil {
						return err
					}
				}
				if err := bumpRevisionTx(tx, node.ID, run.record.StartedAt); err != nil {
					return err
				}
				resulting, err := nodeByIDTx(tx, node.ID)
				if err != nil {
					return err
				}
				authority, scopes, sequence, err := loadAuditedNodeAuthority(ctx, tx, node.ID)
				if err != nil {
					return err
				}
				return persistAuditedProvenanceAppend(ctx, tx, s.vaultID, run.ID(), run.record.StartedAt,
					sequence, authority, scopes, node, resulting, run.record, fact)
			}))

			// Export the fixture's populated tables directly: ExportMetadata
			// itself validates replay and would reject the malformed source.
			var exported bytes.Buffer
			write := newMetadataJSONWriter(&exported)
			require.NoError(t, write(metadataHeader{
				Type: "meta", Format: "docbank-metadata", Version: metadataFormatVersion,
				VaultID: s.VaultID(), NodeSequence: node.ID,
			}))
			require.NoError(t, exportBlobs(ctx, s.db, write, false))
			require.NoError(t, exportNodes(ctx, s.db, write))
			require.NoError(t, exportIngests(ctx, s.db, write))
			require.NoError(t, exportContentVersions(ctx, s.db, write))
			require.NoError(t, exportProvenance(ctx, s.db, write))
			require.NoError(t, exportWatchSources(ctx, s.db, write))
			require.NoError(t, exportAuditMetadata(ctx, s.db, write))

			restored := newTestStore(t)
			err = restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes()))
			if test.valid {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "provenance mutation ingest requires a non-empty caller-supplied source kind")
			var auditRows int
			require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM audit_records`).Scan(&auditRows))
			assert.Zero(t, auditRows, "rejected import must roll back audit history")
		})
	}
}

func findProvenanceFact(facts []ProvenanceFact, identity string) ProvenanceFact {
	for _, fact := range facts {
		if fact.Identity == identity {
			return fact
		}
	}
	return ProvenanceFact{}
}

func createAuditScopeFailureTrigger(s *Store) error {
	_, err := s.db.Exec(`CREATE TRIGGER reject_provenance_scope_advance
		BEFORE UPDATE ON audit_scopes BEGIN
		SELECT RAISE(ABORT, 'forced audited provenance failure'); END`)
	return err
}

func rewriteAppendedProvenancePath(t *testing.T, input []byte, nodeID int64) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	found := false
	for index, line := range lines {
		var header struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &header))
		if header.Type != metadataProvenanceType {
			continue
		}
		var provenance metadataProvenance
		require.NoError(t, json.Unmarshal(line, &provenance))
		if provenance.NodeID == nodeID && provenance.OriginalPath == "opaque://original" {
			provenance.OriginalPath = "opaque://tampered"
			var err error
			provenance.Identity, err = provenanceIdentity(provenance)
			require.NoError(t, err)
			lines[index], err = json.Marshal(provenance)
			require.NoError(t, err)
			found = true
			break
		}
	}
	require.True(t, found)
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}

func TestAuditedProvenanceReplayRejectsDirectoryTarget(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedMetadataRoundTrip(t, s)
	dir, err := s.NodeByPath(ctx, "/Projects")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, dir.ID)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
		NodeID: dir.ID, IfRevision: dir.Revision, SourceKind: "agent",
		SourceDescription: "synthetic directory replay", OriginalPath: "opaque://directory",
	})
	require.ErrorIs(t, err, ErrNotFile)
	run, err := s.BeginCallerSuppliedIngest(ctx, "agent", "synthetic directory replay")
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		prior, err := nodeByIDTx(tx, dir.ID)
		if err != nil {
			return err
		}
		if _, err = ensureIngestRunTx(ctx, tx, run); err != nil {
			return err
		}
		fact := metadataProvenance{
			Type: metadataProvenanceType, NodeID: dir.ID, IngestID: run.record.ID,
			OriginalPath: "opaque://directory",
		}
		fact.Identity, err = provenanceIdentity(fact)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`,
			fact.Identity, fact.NodeID, fact.IngestID, fact.OriginalPath); err != nil {
			return err
		}
		if err = bumpRevisionTx(tx, dir.ID, run.record.StartedAt); err != nil {
			return err
		}
		resulting, err := nodeByIDTx(tx, dir.ID)
		if err != nil {
			return err
		}
		authority, scopes, sequence, err := loadAuditedNodeAuthority(ctx, tx, dir.ID)
		if err != nil {
			return err
		}
		return persistAuditedProvenanceAppend(ctx, tx, s.vaultID, run.record.ID, run.record.StartedAt,
			sequence, authority, scopes, prior, resulting, run.record, fact)
	}))
	err = s.ValidateMetadata(ctx)
	require.ErrorContains(t, err, "provenance mutation targets non-file node")
}

// These digests bind the complete canonical record stream for caller-supplied
// provenance and operational observations.
func TestAuditedProvenanceCanonicalRecords(t *testing.T) {
	for operation, want := range map[string]string{
		"append":           "a9ad4987c066f3fca86e36ff69c9c55e74b342cece6f79944dc38a75ef880b34",
		"supersede":        "a83dec2c0d2295f015e1e4154587fcaa8e15423c095ab1d0f151bbc55e2b90c7",
		"observe new":      "a7cb0b9617b42dc1ee0ccbfbcc9ef27ae3828365fc698dcd57bf2d6a7da6fd68",
		"observe existing": "4f1b5d32845ab9cb0fb7e0e98b90b22277b4c7eb583db733a7f202cec6f8a3cb",
	} {
		t.Run(operation, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			seedMetadataRoundTrip(t, s)
			s.vaultID = "99999999-9999-4999-8999-999999999999"
			_, err := s.db.Exec("UPDATE vault_metadata SET vault_uid=?", s.vaultID)
			require.NoError(t, err)
			node, err := s.NodeByPath(ctx, "/Projects/report.txt")
			require.NoError(t, err)
			var predecessor string
			if operation == "supersede" {
				const priorIngest = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
				_, err = s.db.Exec("INSERT INTO ingests(id,started_at,source_kind,source_desc) VALUES(?,?,'embedded:agent','Synthetic predecessor')",
					priorIngest, testAuditTimestamp)
				require.NoError(t, err)
				prior := metadataProvenance{Type: metadataProvenanceType, NodeID: node.ID, IngestID: priorIngest, OriginalPath: "/synthetic/prior.txt"}
				predecessor, err = provenanceIdentity(prior)
				require.NoError(t, err)
				_, err = s.db.Exec("INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)",
					predecessor, prior.NodeID, prior.IngestID, prior.OriginalPath)
				require.NoError(t, err)
			}
			seedInitialAuditAuthority(t, s, *node.ParentID)
			run := IngestRun{record: metadataIngest{
				Type: metadataIngestType, ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
				StartedAt: testAuditTimestamp, SourceKind: "embedded:agent", SourceDesc: "Synthetic canonical assertion",
			}}
			observation := operation == "observe new" || operation == "observe existing"
			if observation {
				run.record.SourceKind = "filesystem"
			}
			if operation == "observe existing" {
				require.NoError(t, s.db.QueryRow("SELECT id,started_at,source_kind,source_desc FROM ingests WHERE id=?", metadataIngestID).
					Scan(&run.record.ID, &run.record.StartedAt, &run.record.SourceKind, &run.record.SourceDesc))
			}
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				ingestAdded, err := ensureIngestRunTx(ctx, tx, run)
				if err != nil {
					return err
				}
				fact := metadataProvenance{Type: metadataProvenanceType, NodeID: node.ID,
					IngestID: run.ID(), OriginalPath: "/synthetic/assertion.txt"}
				if predecessor != "" {
					fact.Supersedes = &predecessor
				}
				fact.Identity, err = provenanceIdentity(fact)
				if err != nil {
					return err
				}
				if _, err = tx.Exec("INSERT INTO provenance(identity,node_id,ingest_id,original_path,supersedes) VALUES(?,?,?,?,?)",
					fact.Identity, fact.NodeID, fact.IngestID, fact.OriginalPath, fact.Supersedes); err != nil {
					return err
				}
				if err = bumpRevisionTx(tx, node.ID, testAuditTimestamp); err != nil {
					return err
				}
				resulting, err := nodeByIDTx(tx, node.ID)
				if err != nil {
					return err
				}
				authority, scopes, sequence, err := loadAuditedNodeAuthority(ctx, tx, node.ID)
				if err != nil {
					return err
				}
				const operationID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
				if observation {
					return persistAuditedIngestObservation(ctx, tx, s.vaultID, operationID, testAuditTimestamp,
						sequence, authority, scopes, node, resulting, run.record, fact, nil, ingestAdded)
				}
				return persistAuditedProvenanceAppend(ctx, tx, s.vaultID, operationID, testAuditTimestamp,
					sequence, authority, scopes, node, resulting, run.record, fact)
			}))
			require.NoError(t, s.ValidateMetadata(ctx))
			var bindings int64
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM provenance_version_bindings`).Scan(&bindings))
			assert.Zero(t, bindings, "legacy audited provenance histories remain explicitly unbound")
			if observation {
				wantCount := uint64(2)
				if operation == "observe existing" {
					wantCount = 1
				}
				records, err := loadInitialAuditRecords(ctx, s.db)
				require.NoError(t, err)
				mutations, err := auditRecordsByOptionalSequence(records["canonical_mutation"], 2)
				require.NoError(t, err)
				count, err := auditUnsignedField(mutations[2].record, auditAttachedMetadataChangeCountField)
				require.NoError(t, err)
				assert.Equal(t, wantCount, count)

				var exported bytes.Buffer
				require.NoError(t, s.ExportMetadata(ctx, &exported))
				restored := newTestStore(t)
				require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
				var restoredBindings int64
				require.NoError(t, restored.db.QueryRow(
					`SELECT COUNT(*) FROM provenance_version_bindings`,
				).Scan(&restoredBindings))
				assert.Zero(t, restoredBindings)
				var roundTrip bytes.Buffer
				require.NoError(t, restored.ExportMetadata(ctx, &roundTrip))
				assert.Equal(t, exported.Bytes(), roundTrip.Bytes())
			}
			var records bytes.Buffer
			require.NoError(t, exportAuditRecords(ctx, s.db, newMetadataJSONWriter(&records)))
			assert.Equal(t, want, fmt.Sprintf("%x", sha256.Sum256(records.Bytes())))
		})
	}
}
