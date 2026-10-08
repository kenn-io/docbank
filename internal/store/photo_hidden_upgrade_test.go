package store

import (
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
)

//go:embed testdata/schema-v0.15.1.sql
var schemaV0151SQL string

func TestPhotoHiddenUpgradeReleasedV0151(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		for _, pending := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pending=%t", driver.name, pending), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "docbank.db")
				db, err := driver.driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
				require.NoError(t, err)
				_, err = db.Exec(schemaV0151SQL)
				require.NoError(t, err)
				_, err = db.Exec(`INSERT INTO vault_metadata(singleton,vault_uid,schema_version) VALUES(1,'10000000-0000-4000-8000-000000000003',29)`)
				require.NoError(t, err)
				_, err = db.Exec(`INSERT INTO nodes(id,parent_id,name,kind,revision,created_at,modified_at) VALUES(1,NULL,'','dir',1,'2026-08-16T12:00:00.000000000Z','2026-08-16T12:00:00.000000000Z')`)
				require.NoError(t, err)
				tx, err := db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				primary, err := ensurePrimaryBlobStoreTx(tx)
				require.NoError(t, err)
				require.NoError(t, initializeDocumentPeopleState(t.Context(), tx))
				require.NoError(t, ensureProcessingIncarnationTx(tx))
				require.NoError(t, tx.Commit())
				legacy := &Store{db: db, writeDB: db, rootID: 1, vaultID: "10000000-0000-4000-8000-000000000003", primaryStoreID: primary.ID, driver: driver.driver}
				node, err := legacy.CreateFile(t.Context(), 1, "released-photo.jpg", fakeHash("a1"), 4, "image/jpeg")
				require.NoError(t, err)
				incarnation, err := legacy.CurrentProcessingIncarnation(t.Context())
				require.NoError(t, err)
				var job RenditionJob
				now := time.Now().UTC().Add(time.Second)
				if pending {
					versions := seedRenditionCatalogVersions(t, legacy)
					request := renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false))
					grantRenditionJobConsent(t, legacy, request)
					var waiter RenditionJobWaiter
					job, waiter, err = legacy.EnqueueRenditionJob(t.Context(), request)
					require.NoError(t, err)
					claim, err := legacy.ClaimRenditionJob(t.Context(), job.ID, "worker:upgrade", now, time.Minute)
					require.NoError(t, err)
					_, err = legacy.BeginRenditionProvider(t.Context(), claim, waiter.ID, now.Add(time.Second), renditionJobTestSnapshot(request))
					require.NoError(t, err)
					require.NoError(t, legacy.CheckpointRenditionProvider(t.Context(), claim, "upgrade-provider-handle", now.Add(2*time.Second)))
					require.NoError(t, legacy.MarkRenditionJobRetry(t.Context(), claim, RenditionFailureTransient, now.Add(3*time.Second), now.Add(4*time.Second)))
				}
				snapshot, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				require.NoError(t, err)
				var before bytes.Buffer
				require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), snapshot, &before, 29))
				require.NoError(t, snapshot.Rollback())
				require.NoError(t, db.Close())
				s, err := Open(path, driver.driver)
				require.NoError(t, err)
				defer func() { require.NoError(t, s.Close()) }()
				current, err := s.CurrentProcessingIncarnation(t.Context())
				require.NoError(t, err)
				require.Equal(t, incarnation, current)
				var version int
				require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&version))
				assert.Equal(t, 30, version)
				var after bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &after))
				assert.Equal(t, before.String(), after.String())
				if pending {
					resumed, err := s.ClaimRenditionJob(t.Context(), job.ID, "worker:upgraded", now.Add(5*time.Second), time.Minute)
					require.NoError(t, err)
					_, err = s.RenditionJobWorkByClaim(t.Context(), resumed, now.Add(5*time.Second))
					require.NoError(t, err)
				}
				asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
				require.NoError(t, err)
				assert.Nil(t, asset.HiddenAt)
				assert.Equal(t, int64(1), asset.Revision)
				assert.Equal(t, node.ID, asset.Files[0].NodeID)
				require.FileExists(t, path+".schema-v29.bak")
				require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic"))
			})
		}
	}
}

func TestUpgradeMetadataRejectsRepeatedAndConflictingSeededIncarnation(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	grantRenditionJobConsent(t, s, renditionJobTestRequest(versions[0], catalogProcessingProfile(t, false)))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	incarnation := metadataProcessingIncarnation{Type: metadataProcessingIncarnationType}
	require.NoError(t, s.db.QueryRow(`SELECT i.incarnation_id,i.created_at FROM processing_incarnations i JOIN current_processing_incarnation c ON c.incarnation_id=i.incarnation_id`).Scan(&incarnation.ID, &incarnation.CreatedAt))
	var line []byte
	for candidate := range bytes.SplitSeq(exported.Bytes(), []byte{'\n'}) {
		if bytes.Contains(candidate, []byte(`"type":"processing_incarnation"`)) && bytes.Contains(candidate, []byte(incarnation.ID)) {
			line = candidate
			break
		}
	}
	require.NotEmpty(t, line)
	conflicting := incarnation
	conflicting.CreatedAt = "2000-01-01T00:00:00Z"
	conflictJSON, err := json.Marshal(conflicting)
	require.NoError(t, err)
	for name, replacement := range map[string][]byte{"repeated": append(append(append([]byte{}, line...), '\n'), line...), "conflicting": conflictJSON} {
		t.Run(name, func(t *testing.T) {
			target, err := openCurrentStore(filepath.Join(t.TempDir(), "target.db"), s.driver, incarnation)
			require.NoError(t, err)
			defer func() { require.NoError(t, target.Close()) }()
			malformed := bytes.Replace(exported.Bytes(), line, replacement, 1)
			require.Error(t, target.importMetadata(t.Context(), bytes.NewReader(malformed), true))
		})
	}
}

func TestPhotoHiddenUpgradeRejectsChangedReleasedColumns(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			t.Parallel()
			db, err := driver.driver.Open(filepath.Join(t.TempDir(), "released.db"), docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			_, err = db.Exec(schemaV0151SQL)
			require.NoError(t, err)
			require.NoError(t, validateV29Schema(db, nil, nil))
			_, err = db.Exec(`ALTER TABLE photo_files RENAME COLUMN role TO unsupported_role`)
			require.NoError(t, err)
			require.ErrorContains(t, validateV29Schema(db, nil, nil), "photo_files")
		})
	}
}
