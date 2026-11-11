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

func schemaContractFilename(record schemaContract) string {
	return fmt.Sprintf("%06d-%s.json", record.Version, record.Digest)
}

func loadSchemaContracts(dir string) ([]schemaContract, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var records []schemaContract
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("unexpected directory %s", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record schemaContract
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if entry.Name() != schemaContractFilename(record) {
			return nil, fmt.Errorf("schema contract filename %s does not match its record", entry.Name())
		}
		records = append(records, record)
	}
	return records, nil
}

func checkSchemaContract(records []schemaContract, version int, digest string) error {
	matches, highest := 0, 0
	var recorded string
	for _, record := range records {
		if record.Version > highest {
			highest = record.Version
		}
		if record.Version == version {
			matches++
			recorded = record.Digest
		}
	}
	if version < highest || matches > 1 || (matches == 1 && recorded != digest) {
		return fmt.Errorf("schema version %d conflicts with recorded layouts; take version %d, preserve the record from main, remove your conflicting proposal for the old version, and move it to the new version", version, highest+1)
	}
	if matches == 0 {
		record := schemaContract{Version: version, Digest: digest}
		return fmt.Errorf("add schema contract %s: {\"version\": %d, \"digest\": \"%s\"}", schemaContractFilename(record), version, digest)
	}
	return nil
}

func TestStorageSchemaVersionContract(t *testing.T) {
	records, err := loadSchemaContracts("testdata/schema-contracts")
	require.NoError(t, err)
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			require.NoError(t, checkSchemaContract(records, currentStorageSchemaVersion, schemaCatalogDigest(t, db)))
		})
	}
}

func TestStorageSchemaContractRecords(t *testing.T) {
	dir := t.TempDir()
	record := schemaContract{Version: 49, Digest: "abc"}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	path := filepath.Join(dir, schemaContractFilename(record))
	require.NoError(t, os.WriteFile(path, data, 0600))
	records, err := loadSchemaContracts(dir)
	require.NoError(t, err)
	require.Equal(t, []schemaContract{record}, records)
	require.NoError(t, os.Rename(path, filepath.Join(dir, "wrong.json")))
	_, err = loadSchemaContracts(dir)
	require.ErrorContains(t, err, "does not match its record")
}

func TestStorageSchemaContractCollisions(t *testing.T) {
	main := schemaContract{Version: 49, Digest: "main"}
	proposal := schemaContract{Version: 49, Digest: "proposal"}
	dir := t.TempDir()
	for _, record := range []schemaContract{main, proposal} {
		data, err := json.Marshal(record)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, schemaContractFilename(record)), data, 0600))
	}
	records, err := loadSchemaContracts(dir)
	require.NoError(t, err)
	guidance := "schema version 49 conflicts with recorded layouts; take version 50, preserve the record from main, remove your conflicting proposal for the old version, and move it to the new version"
	require.EqualError(t, checkSchemaContract(records, 49, proposal.Digest), guidance)
	require.EqualError(t, checkSchemaContract([]schemaContract{main}, 49, proposal.Digest), guidance)
	require.EqualError(t, checkSchemaContract(records, 48, "old"), "schema version 48 conflicts with recorded layouts; take version 50, preserve the record from main, remove your conflicting proposal for the old version, and move it to the new version")
	require.EqualError(t, checkSchemaContract(records[:1], 50, proposal.Digest), `add schema contract 000050-proposal.json: {"version": 50, "digest": "proposal"}`)
	proposal.Version = 50
	require.NoError(t, checkSchemaContract([]schemaContract{main, proposal}, 50, proposal.Digest))
	require.NoError(t, checkSchemaContract([]schemaContract{main, {Version: 49, Digest: "old"}, proposal}, 50, proposal.Digest))
}

func TestStorageSchemaContractChanges(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			digest := schemaCatalogDigest(t, db)
			records := []schemaContract{{Version: 48, Digest: digest}}
			require.NoError(t, checkSchemaContract(records, 48, digest))
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
						require.ErrorContains(t, err, "take version 49")
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
