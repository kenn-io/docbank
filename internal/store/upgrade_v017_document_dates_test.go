package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestUpgradeReleasedV017RebuildsDocumentDateKeys(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0170SQL, 48)
			node := createReleasedFixtureFile(t, legacy, "released.txt", fakeHash("d1"), 8, "text/plain")
			oldSum := sha256.Sum256([]byte("docbank-document-events:f10-metadata+content-version+provenance-binding+actor-evidence:v2"))
			oldFingerprint := hex.EncodeToString(oldSum[:])
			targets, err := legacy.MissingDocumentEventTargetsAfter(ctx, oldFingerprint, "", 100)
			require.NoError(t, err)
			target := byTargetID(targets, node.CurrentVersionID)
			require.Equal(t, node.CurrentVersionID, target.ContentVersionID)
			require.EqualValues(t, 1, target.InputEpoch)
			manifest := sha256.New()
			require.NoError(t, writeDocumentEventManifestFrame(manifest, documentEventManifestHeader{
				DeriverDescriptor: "docbank-document-events:f10-metadata+content-version+provenance-binding+actor-evidence:v2", DeriverFingerprint: oldFingerprint,
				VaultUID: legacy.VaultID(), DocumentKind: DocumentEventKindForMIMEType(target.MIMEType), Metadata: documentEventMetadataManifest{Present: false},
				Target: documentEventManifestTarget{ContentVersionID: target.ContentVersionID, NodeID: target.NodeID, BlobHash: target.BlobHash, Size: target.Size, MIMEType: target.MIMEType, RecordedAt: target.RecordedAt},
			}))
			require.NoError(t, writeDocumentEventManifestFrame(manifest, documentEventManifestTrailer{}))
			inputs := hex.EncodeToString(manifest.Sum(nil))
			evidence := sha256.New()
			for _, value := range []string{"content-version", target.ContentVersionID, strconv.FormatInt(target.NodeID, 10), target.BlobHash, strconv.FormatInt(target.Size, 10), target.MIMEType, target.RecordedAt} {
				_, err := fmt.Fprintf(evidence, "%d:%s", len(value), value)
				require.NoError(t, err)
			}
			value := strings.TrimSuffix(target.RecordedAt, "Z")
			precision := document.EventPrecision("second")
			digits := 0
			if dot := strings.LastIndexByte(value, '.'); dot >= 0 {
				precision = "fraction"
				digits = len(value) - dot - 1
			}
			axis, err := document.EventAxisKey(value, precision, "utc", nil)
			require.NoError(t, err)
			utc, _, err := document.EventUTCKey(value, precision, "utc", nil)
			require.NoError(t, err)
			event := document.DocumentEventV1{Actors: []document.DocumentEventActorV1{}, DateKind: "vault_recorded", SourceKey: "version/" + target.ContentVersionID + "/recorded_at", DateValue: value, RawValue: target.RecordedAt, Precision: precision, FractionDigits: digits, TimezoneKind: "utc", ZoneText: "Z", AxisKey: axis, UTCKey: utc, ClaimBasis: "docbank_observed", ParseConfidence: "exact", EvidenceKind: "content_version", EvidenceID: target.ContentVersionID, EvidenceSHA256: hex.EncodeToString(evidence.Sum(nil)), EvidenceLocator: "recorded_at"}
			event.EventID = document.DocumentEventID(legacy.VaultID(), target.ContentVersionID, event.SourceKey, event.DateKind, event.DateValue, event.Precision, event.TimezoneKind, event.EvidenceSHA256)
			record := document.DocumentEventsV1{VaultUID: legacy.VaultID(), ContentVersionID: target.ContentVersionID, ContractVersion: document.DocumentEventsContractV1, DocumentKind: DocumentEventKindForMIMEType(target.MIMEType), Events: []document.DocumentEventV1{event}, Diagnostics: []document.DocumentEventDiagnosticV1{}, Sources: []document.DocumentEventSourceV1{{EvidenceKind: event.EvidenceKind, EvidenceID: event.EvidenceID, EvidenceSHA256: event.EvidenceSHA256}}, Primaries: []document.DocumentEventPrimaryV1{}}
			for _, disclosure := range []string{"full", "safe"} {
				primary, ok := document.SelectPrimaryEvent(record.Events, record.DocumentKind, "vault", disclosure)
				require.True(t, ok)
				record.Primaries = append(record.Primaries, primary)
			}
			canonical, checksum, err := document.MarshalDocumentEventsV1(record)
			require.NoError(t, err)
			generation := document.DocumentEventGenerationID(legacy.VaultID(), node.CurrentVersionID, document.DocumentEventsContractV1, oldFingerprint, inputs)
			_, err = db.ExecContext(ctx, `INSERT INTO document_event_state(singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at) VALUES(1,$1,$2,1,1,$3)`, document.DocumentEventsContractV1, oldFingerprint, "2020-01-01T00:00:00Z")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO document_event_generations(generation_id,content_version_id,contract_version,deriver_fingerprint,inputs_sha256,document_kind,canonical_json,checksum,event_count,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, generation, node.CurrentVersionID, document.DocumentEventsContractV1, oldFingerprint, inputs, record.DocumentKind, canonical, checksum, len(record.Events), "2020-01-01T00:00:00Z")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO document_events(generation_id,event_id,source_key,date_kind,source_kind_raw,date_value,raw_value,precision,fraction_digits,timezone_kind,zone_text,offset_seconds,axis_key,utc_key,claim_basis,parse_confidence,evidence_kind,evidence_id,evidence_sha256,evidence_locator,sensitive) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`, generation, event.EventID, event.SourceKey, event.DateKind, event.SourceKindRaw, event.DateValue, event.RawValue, event.Precision, event.FractionDigits, event.TimezoneKind, event.ZoneText, nullableDocumentEventOffset(event.OffsetSeconds), event.AxisKey, nullableString(event.UTCKey), event.ClaimBasis, event.ParseConfidence, event.EvidenceKind, event.EvidenceID, event.EvidenceSHA256, []byte(event.EvidenceLocator), documentEventBool(event.Sensitive))
			require.NoError(t, err)
			for _, actor := range event.Actors {
				_, err = db.ExecContext(ctx, `INSERT INTO document_event_actors(generation_id,event_id,role,ordinal,actor_key,display_name,address,claim_json,evidence_kind,evidence_id,sensitive) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, generation, event.EventID, actor.Role, actor.Ordinal, actor.ActorKey, actor.DisplayName, actor.Address, []byte(actor.Claim), actor.EvidenceKind, actor.EvidenceID, documentEventBool(actor.Sensitive))
				require.NoError(t, err)
			}
			for _, primary := range record.Primaries {
				_, err = db.ExecContext(ctx, `INSERT INTO document_event_primaries(generation_id,scope_class,disclosure,event_id,rule_id,reason) VALUES($1,$2,$3,$4,$5,$6)`, generation, primary.ScopeClass, primary.Disclosure, primary.EventID, primary.RuleID, primary.Reason)
				require.NoError(t, err)
			}
			_, err = db.ExecContext(ctx, `INSERT INTO document_event_heads(content_version_id,generation_id,input_epoch,published_at) VALUES($1,$2,1,$3)`, node.CurrentVersionID, generation, "2020-01-01T00:00:00Z")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO document_event_attempts(content_version_id,input_epoch,input_revision,inputs_sha256,state,diagnostic_json,attempted_at) VALUES($1,1,$2,$3,'indexed',$4,$5)`, node.CurrentVersionID, target.InputRevision, inputs, []byte(`[]`), "2020-01-01T00:00:00Z")
			require.NoError(t, err)
			var oldHead, oldInputs, oldState string
			var oldEpoch, oldRevision int64
			require.NoError(t, db.QueryRowContext(ctx, `SELECT h.generation_id,a.inputs_sha256,a.state,h.input_epoch,a.input_revision FROM document_event_heads h JOIN document_event_attempts a ON a.content_version_id=h.content_version_id WHERE h.content_version_id=$1`, node.CurrentVersionID).Scan(&oldHead, &oldInputs, &oldState, &oldEpoch, &oldRevision))
			require.Equal(t, generation, oldHead)
			require.Equal(t, inputs, oldInputs)
			require.Equal(t, "indexed", oldState)
			require.EqualValues(t, 1, oldEpoch)
			require.Equal(t, target.InputRevision, oldRevision)
			var keyTables int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='document_date_keys'`).Scan(&keyTables))
			require.Zero(t, keyTables)
			require.NoError(t, db.Close())
			s, err := Open(ctx, path, driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.ErrorIs(t, requireDocumentDateIndex(ctx, s.db), ErrDocumentDatesPending)
			driveDocumentEventWorker(t, s)
			require.NoError(t, requireDocumentDateIndex(ctx, s.db))
			var count int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_date_keys`).Scan(&count))
			require.Positive(t, count)
		})
	}
}
