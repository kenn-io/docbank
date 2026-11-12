package store

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestUpgradeReleasedV017RebuildsDocumentDateKeys(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0170SQL, 48)
			node := createReleasedFixtureFile(t, legacy, "released.txt", fakeHash("d1"), 8, "text/plain")
			oldSum := sha256.Sum256([]byte("docbank-document-events:f10-metadata+content-version+provenance-binding+actor-evidence:v2"))
			oldFingerprint := hex.EncodeToString(oldSum[:])
			inputs := fakeHash("released-inputs")
			record := documentEventRecord(t, legacy.VaultID(), node.CurrentVersionID, "released")
			canonical, checksum, err := document.MarshalDocumentEventsV1(record)
			require.NoError(t, err)
			generation := document.DocumentEventGenerationID(legacy.VaultID(), node.CurrentVersionID, document.DocumentEventsContractV1, oldFingerprint, inputs)
			_, err = db.ExecContext(ctx, `INSERT INTO document_event_state(singleton,contract_version,deriver_fingerprint,input_epoch,publication_epoch,updated_at) VALUES(1,$1,$2,1,1,$3)`, document.DocumentEventsContractV1, oldFingerprint, "2020-01-01T00:00:00Z")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, `INSERT INTO document_event_generations(generation_id,content_version_id,contract_version,deriver_fingerprint,inputs_sha256,document_kind,canonical_json,checksum,event_count,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, generation, node.CurrentVersionID, document.DocumentEventsContractV1, oldFingerprint, inputs, record.DocumentKind, canonical, checksum, len(record.Events), "2020-01-01T00:00:00Z")
			require.NoError(t, err)
			event := record.Events[0]
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
			_, err = db.ExecContext(ctx, `INSERT INTO document_event_attempts(content_version_id,input_epoch,input_revision,inputs_sha256,state,diagnostic_json,attempted_at) VALUES($1,1,0,$2,'indexed',$3,$4)`, node.CurrentVersionID, inputs, []byte(`[]`), "2020-01-01T00:00:00Z")
			require.NoError(t, err)
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
