package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSavedQueryBackupRoundTripPreservesEveryFieldAndKind(t *testing.T) {
	source := newTestStore(t)
	ctx := t.Context()
	queryRecord, err := source.CreateSavedQuery(ctx, "Zulu query", "full expression", SavedQueryKindQuery, []byte(savedQueryFullExpression))
	require.NoError(t, err)
	highlightRecord, err := source.CreateSavedQuery(ctx, "Alpha highlights", "literal terms", SavedQueryKindHighlightSet, []byte(savedHighlightPayload))
	require.NoError(t, err)
	highlightRecord, err = source.UpdateSavedQuery(ctx, highlightRecord.ID, highlightRecord.Revision, SavedQueryPatch{
		Description: new("literal terms\nreviewed"),
	})
	require.NoError(t, err)

	snapshot, err := source.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(ctx, &exported))
	require.NoError(t, snapshot.Close())

	queryLine := fmt.Sprintf(`{"type":"saved_query","saved_query_id":"%s"`, queryRecord.ID)
	highlightLine := fmt.Sprintf(`{"type":"saved_query","saved_query_id":"%s"`, highlightRecord.ID)
	assert.Contains(t, exported.String(), queryLine)
	assert.Contains(t, exported.String(), highlightLine)
	orderedIDs := []string{queryRecord.ID, highlightRecord.ID}
	slices.Sort(orderedIDs)
	assert.Less(t,
		strings.Index(exported.String(), `"saved_query_id":"`+orderedIDs[0]+`"`),
		strings.Index(exported.String(), `"saved_query_id":"`+orderedIDs[1]+`"`),
		"saved definitions must export in deterministic ID order",
	)

	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	gotQuery, err := target.SavedQueryByID(ctx, queryRecord.ID)
	require.NoError(t, err)
	assert.Equal(t, queryRecord, gotQuery)
	gotHighlight, err := target.SavedQueryByID(ctx, highlightRecord.ID)
	require.NoError(t, err)
	assert.Equal(t, highlightRecord, gotHighlight)

	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	assert.Equal(t, exported.Bytes(), restored.Bytes())
}

func TestSavedQueryMetadataImportRejectsMalformedOrCollidingAuthorityTransactionally(t *testing.T) {
	source := newTestStore(t)
	first, err := source.CreateSavedQuery(t.Context(), "first", "", SavedQueryKindQuery, []byte(`{"text":"one"}`))
	require.NoError(t, err)
	second, err := source.CreateSavedQuery(t.Context(), "second", "", SavedQueryKindHighlightSet, []byte(savedHighlightPayload))
	require.NoError(t, err)
	var valid bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &valid))

	firstPayload := base64.StdEncoding.EncodeToString(first.Payload)
	oversizedPayload := base64.StdEncoding.EncodeToString(oversizedSavedQueryPayload())
	for _, test := range []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "unknown kind",
			mutate: func(input string) string {
				return mutateSavedQueryMetadataRecord(t, input, first.ID, func(line []byte) []byte {
					mutated := bytes.Replace(line, []byte(`"kind":"query"`), []byte(`"kind":"future_kind"`), 1)
					require.NotEqual(t, string(line), string(mutated))
					return mutated
				})
			},
		},
		{
			name: "oversized payload",
			mutate: func(input string) string {
				return mutateSavedQueryMetadataRecord(t, input, first.ID, func(line []byte) []byte {
					mutated := bytes.Replace(line, []byte(firstPayload), []byte(oversizedPayload), 1)
					require.NotEqual(t, string(line), string(mutated))
					return mutated
				})
			},
		},
		{
			name: "fingerprint does not match canonical payload",
			mutate: func(input string) string {
				return strings.Replace(input, first.Fingerprint,
					"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1)
			},
		},
		{
			name: "payload is not canonical",
			mutate: func(input string) string {
				noncanonical := base64.StdEncoding.EncodeToString([]byte(`{ "text": "one" }`))
				return strings.Replace(input, firstPayload, noncanonical, 1)
			},
		},
		{
			name: "duplicate ID",
			mutate: func(input string) string {
				return strings.Replace(input, second.ID, first.ID, 1)
			},
		},
		{
			name: "duplicate normalized name",
			mutate: func(input string) string {
				return strings.Replace(input, `"name":"second"`, `"name":"first"`, 1)
			},
		},
		{
			name: "zero revision",
			mutate: func(input string) string {
				return mutateSavedQueryMetadataRecord(t, input, first.ID, func(line []byte) []byte {
					mutated := bytes.Replace(line, []byte(`"revision":1`), []byte(`"revision":0`), 1)
					require.NotEqual(t, string(line), string(mutated))
					return mutated
				})
			},
		},
		{
			name: "noncanonical timestamp",
			mutate: func(input string) string {
				return strings.Replace(input, first.CreatedAt, "2026-01-01T00:00:00Z", 1)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := newTestStore(t)
			err := target.ImportMetadata(t.Context(), strings.NewReader(test.mutate(valid.String())))
			require.Error(t, err)
			rows, total, listErr := target.SavedQueries(t.Context(), "", 10, 0)
			require.NoError(t, listErr)
			assert.Zero(t, total)
			assert.Empty(t, rows)
			var roots int
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent_id IS NULL`).Scan(&roots))
			assert.Equal(t, 1, roots, "failed import must restore the pristine target")
		})
	}
}

func TestSavedQueryMetadataExportRejectsDatabaseInjectedPolicyViolations(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			for _, test := range []struct {
				name   string
				inject func(*Store, string) error
			}{
				{
					name: "unknown kind",
					inject: func(s *Store, id string) error {
						_, err := s.db.Exec(`UPDATE saved_queries SET kind='future_kind' WHERE id=?`, id)
						return err
					},
				},
				{
					name: "oversized payload",
					inject: func(s *Store, id string) error {
						_, err := s.db.Exec(`UPDATE saved_queries SET payload=? WHERE id=?`,
							oversizedSavedQueryPayload(), id)
						return err
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					s := newTestStoreWithDriver(t, driver.driver)
					created, err := s.CreateSavedQuery(t.Context(), "injected", "",
						SavedQueryKindQuery, []byte(`{"text":"before"}`))
					require.NoError(t, err)
					require.NoError(t, test.inject(s, created.ID),
						"schema must not encode evolving saved-query policy")

					err = s.ExportMetadata(t.Context(), &bytes.Buffer{})
					require.ErrorContains(t, err, "validating saved query metadata for export")
				})
			}
		})
	}
}

func TestSavedQueryFutureTimestampRemainsPortableAfterEdit(t *testing.T) {
	const future = "2099-01-01T00:00:00.000000000Z"
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			source := newTestStoreWithDriver(t, test.driver)
			created, err := source.CreateSavedQuery(t.Context(), "future", "before",
				SavedQueryKindQuery, []byte(`{"text":"future"}`))
			require.NoError(t, err)
			var exported bytes.Buffer
			require.NoError(t, source.ExportMetadata(t.Context(), &exported))
			futureMetadata := mutateSavedQueryMetadataRecord(
				t, exported.String(), created.ID, func(line []byte) []byte {
					mutated := bytes.ReplaceAll(line, []byte(created.CreatedAt), []byte(future))
					require.NotEqual(t, string(line), string(mutated))
					return mutated
				},
			)

			target := newTestStoreWithDriver(t, test.driver)
			require.NoError(t, target.ImportMetadata(t.Context(), strings.NewReader(futureMetadata)))
			imported, err := target.SavedQueryByID(t.Context(), created.ID)
			require.NoError(t, err)
			assert.Equal(t, future, imported.CreatedAt)
			assert.Equal(t, future, imported.UpdatedAt)
			noop, err := target.UpdateSavedQuery(t.Context(), imported.ID, imported.Revision, SavedQueryPatch{})
			require.NoError(t, err)
			assert.Equal(t, imported, noop)

			updated, err := target.UpdateSavedQuery(t.Context(), imported.ID, imported.Revision,
				SavedQueryPatch{Description: new("after")})
			require.NoError(t, err)
			var afterEdit bytes.Buffer
			require.NoError(t, target.ExportMetadata(t.Context(), &afterEdit))
			assert.Equal(t, future, updated.CreatedAt)
			assert.Equal(t, future, updated.UpdatedAt)
			assert.Equal(t, int64(2), updated.Revision)

			restored := newTestStoreWithDriver(t, test.driver)
			require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(afterEdit.Bytes())))
			roundTripped, err := restored.SavedQueryByID(t.Context(), updated.ID)
			require.NoError(t, err)
			assert.Equal(t, updated, roundTripped)
		})
	}
}

func mutateSavedQueryMetadataRecord(
	t *testing.T, input, id string, mutate func([]byte) []byte,
) string {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace([]byte(input)), []byte{'\n'})
	for index, line := range lines {
		var identity struct {
			Type string `json:"type"`
			ID   string `json:"saved_query_id"`
		}
		require.NoError(t, json.Unmarshal(line, &identity))
		if identity.Type == metadataSavedQueryType && identity.ID == id {
			lines[index] = mutate(line)
			return string(append(bytes.Join(lines, []byte{'\n'}), '\n'))
		}
	}
	require.FailNow(t, "saved query metadata record not found", id)
	return ""
}

func TestSavedQueryMetadataImportAcceptsOldBackupWithNoDefinitions(t *testing.T) {
	source := newTestStore(t)
	var oldBackup bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &oldBackup))
	assert.NotContains(t, oldBackup.String(), `"type":"saved_query"`)

	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(oldBackup.Bytes())))
	rows, total, err := target.SavedQueries(t.Context(), "", 10, 0)
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.Empty(t, rows)
}

func TestSavedQueryImportRequiresPristineSavedAuthority(t *testing.T) {
	source := newTestStore(t)
	var metadata bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &metadata))

	target := newTestStore(t)
	_, err := target.CreateSavedQuery(t.Context(), "existing", "", SavedQueryKindQuery, []byte(`{"text":"existing"}`))
	require.NoError(t, err)
	err = target.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes()))
	require.ErrorContains(t, err, "not pristine")
}

func TestSavedQueryOldestReleasedSchemaUpgradeCreatesEmptyAuthority(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			createV090Fixture(t, dbPath, test.driver)

			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			var schemaVersion int
			require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata WHERE singleton=1`).Scan(&schemaVersion))
			assert.Equal(t, 7, schemaVersion)
			rows, total, err := s.SavedQueries(t.Context(), "", 10, 0)
			require.NoError(t, err)
			assert.Zero(t, total)
			assert.Empty(t, rows)
		})
	}
}

func TestSavedQueryCurrentSchemaRejectsMissingAuthorityTable(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			_, err = s.db.Exec(`DROP TABLE saved_queries`)
			require.NoError(t, err)
			require.NoError(t, s.Close())

			reopened, err := Open(dbPath, test.driver)
			if reopened != nil {
				require.NoError(t, reopened.Close())
			}
			require.ErrorContains(t, err, "saved_queries")
		})
	}
}

func TestSavedQuerySchemaRejectsPriorUnreleasedLayout(t *testing.T) {
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "docbank.db")
			s, err := Open(dbPath, test.driver)
			require.NoError(t, err)
			_, err = s.db.Exec(`UPDATE vault_metadata SET schema_version=6 WHERE singleton=1`)
			require.NoError(t, err)
			require.NoError(t, s.Close())

			reopened, err := Open(dbPath, test.driver)
			if reopened != nil {
				require.NoError(t, reopened.Close())
			}
			require.ErrorContains(t, err, "schema version 6 has no supported JSONL cutover")
		})
	}
}
