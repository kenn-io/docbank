package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

type schemaContract struct {
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

func schemaCatalogDigest(t *testing.T, db *sql.DB) string {
	t.Helper()
	catalog := releasedSchemaCatalog(t, db, true)
	names := make([]string, 0, len(catalog))
	for name, object := range catalog {
		if object[0] == "index" {
			var unique bool
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT "unique" FROM pragma_index_list(?) WHERE name=?`, object[1], name).Scan(&unique))
			if !unique {
				continue
			}
		}
		names = append(names, name)
	}
	slices.Sort(names)
	entries := make([][4]string, 0, len(names))
	for _, name := range names {
		object := catalog[name]
		entries = append(entries, [4]string{name, object[0], object[1], object[2]})
	}
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func checkSchemaContract(records []schemaContract, version int, digest string) error {
	matches := 0
	var recorded string
	for _, record := range records {
		if record.Version == version {
			matches++
			recorded = record.Digest
		}
	}
	if matches > 1 {
		return fmt.Errorf("duplicate schema contract for version %d", version)
	}
	if matches == 0 {
		return fmt.Errorf("append schema contract: {\"version\": %d, \"digest\": \"%s\"}", version, digest)
	}
	if recorded != digest {
		return fmt.Errorf("schema layout changed under version %d; bump version and append schema contract: {\"version\": %d, \"digest\": \"%s\"}", version, version+1, digest)
	}
	return nil
}

func TestStorageSchemaVersionContract(t *testing.T) {
	data, err := os.ReadFile("testdata/schema-contracts.json")
	require.NoError(t, err)
	var records []schemaContract
	require.NoError(t, json.Unmarshal(data, &records))
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			require.NoError(t, checkSchemaContract(records, currentStorageSchemaVersion, schemaCatalogDigest(t, db)))
		})
	}
}

func TestStorageSchemaContractChanges(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			digest := schemaCatalogDigest(t, db)
			records := []schemaContract{{Version: 48, Digest: digest}}
			require.NoError(t, checkSchemaContract(records, 48, digest))
			require.ErrorContains(t, checkSchemaContract(nil, 49, digest), `"version": 49`)
			require.ErrorContains(t, checkSchemaContract(append(records, records[0]), 48, digest), "duplicate")
			for _, tc := range []struct {
				name    string
				sql     string
				changed bool
			}{
				{"table", `CREATE TABLE contract_probe (id INTEGER)`, true},
				{"column", `ALTER TABLE nodes ADD COLUMN contract_probe TEXT`, true},
				{"unique index", `CREATE UNIQUE INDEX contract_probe ON nodes(id)`, true},
				{"non-unique index", `CREATE INDEX contract_probe ON nodes(id)`, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := db.ExecContext(t.Context(), "SAVEPOINT probe")
					require.NoError(t, err)
					_, err = db.ExecContext(t.Context(), tc.sql)
					require.NoError(t, err)
					err = checkSchemaContract(records, 48, schemaCatalogDigest(t, db))
					if tc.changed {
						require.ErrorContains(t, err, `"version": 49`)
					} else {
						require.NoError(t, err)
					}
					_, err = db.ExecContext(t.Context(), "ROLLBACK TO probe")
					require.NoError(t, err)
					_, err = db.ExecContext(t.Context(), "RELEASE probe")
					require.NoError(t, err)
				})
			}
		})
	}
}
