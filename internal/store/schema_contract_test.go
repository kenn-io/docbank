package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

type schemaContract struct {
	Version int               `json:"version"`
	Digest  string            `json:"digest"`
	Indexes map[string]string `json:"indexes"`
}

func schemaCatalogDigest(t *testing.T, db *sql.DB) (string, map[string]string) {
	t.Helper()
	catalog := releasedSchemaCatalog(t, db, true)
	indexes := make(map[string]string)
	names := make([]string, 0, len(catalog))
	for name, object := range catalog {
		if object[0] == "index" {
			var unique bool
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT "unique" FROM pragma_index_list(?) WHERE name=?`, object[1], name).Scan(&unique))
			if !unique {
				indexes[name] = object[2]
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
	data, err := canonical.Marshal(entries)
	require.NoError(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(data)), indexes
}

func schemaContractFile(record schemaContract) (string, []byte, error) {
	data, err := canonical.Marshal(record)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%06d-%x.json", record.Version, sha256.Sum256(data)), data, nil
}

func loadSchemaContracts(dir string) ([]schemaContract, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []schemaContract
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		record, err := canonical.Decode[schemaContract](data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		name, _, err := schemaContractFile(record)
		if err != nil {
			return nil, err
		}
		if entry.Name() != name {
			return nil, fmt.Errorf("schema contract filename %s does not match its record", entry.Name())
		}
		records = append(records, record)
	}
	return records, nil
}

func checkSchemaContract(records []schemaContract, version int, digest string, indexes map[string]string) error {
	highest, found := 0, false
	combined := make(map[string]string)
	collision := false
	for _, record := range records {
		highest = max(highest, record.Version)
		if record.Version != version {
			continue
		}
		found = true
		collision = collision || record.Digest != digest
		for name, sql := range record.Indexes {
			if previous, ok := combined[name]; ok && previous != sql {
				collision = true
			}
			combined[name] = sql
		}
	}
	for name, sql := range combined {
		if indexes[name] != sql {
			collision = true
		}
	}
	if version < highest || collision {
		return fmt.Errorf("schema version %d conflicts with recorded layouts; take version %d, preserve main's records, remove your conflicting proposal for the old version, and move it to the new version", version, highest+1)
	}
	additions := make(map[string]string)
	for name, sql := range indexes {
		if _, ok := combined[name]; !ok {
			additions[name] = sql
		}
	}
	if !found || len(additions) > 0 {
		record := schemaContract{Version: version, Digest: digest, Indexes: additions}
		name, data, err := schemaContractFile(record)
		if err != nil {
			return err
		}
		return fmt.Errorf("add schema contract %s: %s", name, data)
	}
	return nil
}

func TestStorageSchemaVersionContract(t *testing.T) {
	if slices.Contains(releasedStorageSchemaVersions, currentStorageSchemaVersion) {
		return
	}
	records, err := loadSchemaContracts("testdata/schema-contracts")
	require.NoError(t, err)
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			digest, indexes := schemaCatalogDigest(t, db)
			require.NoError(t, checkSchemaContract(records, currentStorageSchemaVersion, digest, indexes))
		})
	}
}

func TestStorageSchemaContractRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	records, err := loadSchemaContracts(dir)
	require.NoError(t, err)
	require.Empty(t, records)
	require.NoError(t, os.Mkdir(dir, 0700))
	record := schemaContract{Version: 49, Digest: "abc", Indexes: map[string]string{"idx": "CREATE INDEX idx ON nodes(id)"}}
	name, data, err := schemaContractFile(record)
	require.NoError(t, err)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0600))
	records, err = loadSchemaContracts(dir)
	require.NoError(t, err)
	require.Equal(t, []schemaContract{record}, records)
	require.NoError(t, os.Rename(path, filepath.Join(dir, "wrong.json")))
	_, err = loadSchemaContracts(dir)
	require.ErrorContains(t, err, "does not match its record")
	require.NoError(t, os.Rename(filepath.Join(dir, "wrong.json"), path))
	require.NoError(t, os.WriteFile(path, []byte(`{"digest":"abc","format":1,"indexes":{"idx":"CREATE INDEX idx ON nodes(id)"},"version":49}`), 0600))
	_, err = loadSchemaContracts(dir)
	require.ErrorContains(t, err, `unknown object member name "format"`)
}

func TestStorageSchemaContractChanges(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			digest, indexes := schemaCatalogDigest(t, db)
			record := schemaContract{Version: 49, Digest: digest, Indexes: indexes}
			require.ErrorContains(t, checkSchemaContract(nil, 49, digest, indexes), "add schema contract 000049-")
			require.NoError(t, checkSchemaContract([]schemaContract{record}, 49, digest, indexes))
			var proposals []schemaContract
			for _, statement := range []string{`CREATE TABLE proposal_a (id INTEGER)`, `CREATE TABLE proposal_b (id INTEGER)`} {
				_, err := db.ExecContext(t.Context(), "SAVEPOINT probe")
				require.NoError(t, err)
				_, err = db.ExecContext(t.Context(), statement)
				require.NoError(t, err)
				changed, current := schemaCatalogDigest(t, db)
				proposals = append(proposals, schemaContract{Version: 49, Digest: changed, Indexes: current})
				_, err = db.ExecContext(t.Context(), "ROLLBACK TO probe")
				require.NoError(t, err)
				_, err = db.ExecContext(t.Context(), "RELEASE probe")
				require.NoError(t, err)
			}
			require.ErrorContains(t, checkSchemaContract(proposals, 49, proposals[1].Digest, indexes), "take version 50")
			proposals[1].Version = 50
			require.NoError(t, checkSchemaContract(proposals, 50, proposals[1].Digest, indexes))
			for _, tc := range []struct {
				name string
				sql  string
			}{
				{"table", `CREATE TABLE contract_probe (id INTEGER)`},
				{"column", `ALTER TABLE nodes ADD COLUMN contract_probe TEXT`},
				{"unique index", `CREATE UNIQUE INDEX contract_probe ON nodes(id)`},
				{"remove index", `DROP INDEX nodes_parent`},
				{"change index", `DROP INDEX nodes_parent; CREATE INDEX nodes_parent ON nodes(id)`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := db.ExecContext(t.Context(), "SAVEPOINT probe")
					require.NoError(t, err)
					_, err = db.ExecContext(t.Context(), tc.sql)
					require.NoError(t, err)
					changed, current := schemaCatalogDigest(t, db)
					require.ErrorContains(t, checkSchemaContract([]schemaContract{record}, 49, changed, current), "take version 50")
					_, err = db.ExecContext(t.Context(), "ROLLBACK TO probe")
					require.NoError(t, err)
					_, err = db.ExecContext(t.Context(), "RELEASE probe")
					require.NoError(t, err)
				})
			}
			_, err := db.ExecContext(t.Context(), `CREATE INDEX contract_probe ON nodes(id)`)
			require.NoError(t, err)
			newDigest, current := schemaCatalogDigest(t, db)
			require.Equal(t, digest, newDigest)
			addition := schemaContract{Version: 49, Digest: digest, Indexes: map[string]string{"contract_probe": current["contract_probe"]}}
			name, data, err := schemaContractFile(addition)
			require.NoError(t, err)
			require.EqualError(t, checkSchemaContract([]schemaContract{record}, 49, newDigest, current), fmt.Sprintf("add schema contract %s: %s", name, data))
			records := []schemaContract{record, addition}
			require.NoError(t, checkSchemaContract(records, 49, newDigest, current))
			_, err = db.ExecContext(t.Context(), `DROP INDEX contract_probe; CREATE INDEX contract_probe ON nodes(name)`)
			require.NoError(t, err)
			changed, current := schemaCatalogDigest(t, db)
			require.ErrorContains(t, checkSchemaContract(records, 49, changed, current), "take version 50")
			_, err = db.ExecContext(t.Context(), `DROP INDEX contract_probe`)
			require.NoError(t, err)
			changed, current = schemaCatalogDigest(t, db)
			require.ErrorContains(t, checkSchemaContract(records, 49, changed, current), "take version 50")
		})
	}
}

func TestStorageSchemaContractCollisions(t *testing.T) {
	main := schemaContract{Version: 49, Digest: "main", Indexes: map[string]string{"a": "sql a"}}
	proposal := schemaContract{Version: 49, Digest: "proposal", Indexes: map[string]string{"b": "sql b"}}
	current := map[string]string{"a": "sql a", "b": "sql b"}
	guidance := "schema version 49 conflicts with recorded layouts; take version 50, preserve main's records, remove your conflicting proposal for the old version, and move it to the new version"
	require.EqualError(t, checkSchemaContract([]schemaContract{main, proposal}, 49, "combined", current), guidance)
	proposal.Version = 50
	proposal.Indexes["a"] = "sql a"
	require.NoError(t, checkSchemaContract([]schemaContract{main, proposal}, 50, "proposal", current))
	require.ErrorContains(t, checkSchemaContract([]schemaContract{main, proposal}, 48, "old", current), "take version 51")
	addition := schemaContract{Version: 49, Digest: "main", Indexes: map[string]string{"b": "sql b"}}
	mainName, _, err := schemaContractFile(main)
	require.NoError(t, err)
	additionName, _, err := schemaContractFile(addition)
	require.NoError(t, err)
	require.NotEqual(t, mainName, additionName)
	require.NoError(t, checkSchemaContract([]schemaContract{main, addition}, 49, "main", current))
	addition.Indexes["a"] = "different"
	require.EqualError(t, checkSchemaContract([]schemaContract{main, addition}, 49, "main", current), guidance)
}
