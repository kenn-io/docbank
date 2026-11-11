package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

type schemaContract struct {
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

func schemaCatalogDigest(t *testing.T, db *sql.DB) string {
	t.Helper()
	catalog := releasedSchemaCatalog(t, db, true)
	names := slices.Sorted(maps.Keys(catalog))
	entries := make([][4]string, 0, len(names))
	for _, name := range names {
		object := catalog[name]
		entries = append(entries, [4]string{name, object[0], object[1], object[2]})
	}
	data, err := canonical.Marshal(entries)
	require.NoError(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func schemaContractFile(record schemaContract) (string, []byte, error) {
	data, err := canonical.Marshal(record)
	return fmt.Sprintf("%06d-%s.json", record.Version, record.Digest), data, err
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
		if name, _, _ := schemaContractFile(record); entry.Name() != name {
			return nil, fmt.Errorf("schema contract filename %s does not match its record", entry.Name())
		}
		records = append(records, record)
	}
	return records, nil
}

func checkSchemaContract(records []schemaContract, version int, digest string) error {
	highest, found, collision := 0, false, false
	for _, record := range records {
		highest = max(highest, record.Version)
		if record.Version == version {
			found = true
			collision = collision || record.Digest != digest
		}
	}
	if version < highest || collision {
		return fmt.Errorf("schema version %d conflicts with recorded layouts; preserve main's records, remove your old record, set currentStorageSchemaVersion to %d, rerun this test against the combined layout, and add the record it prints", version, highest+1)
	}
	if !found {
		name, data, err := schemaContractFile(schemaContract{Version: version, Digest: digest})
		if err != nil {
			return err
		}
		return fmt.Errorf("add schema contract %s: %s", name, data)
	}
	return nil
}

func TestStorageSchemaVersionContract(t *testing.T) {
	t.Parallel()
	if slices.Contains(releasedStorageSchemaVersions, currentStorageSchemaVersion) {
		return
	}
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
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "records")
	records, err := loadSchemaContracts(dir)
	require.NoError(t, err)
	require.Empty(t, records)
	require.NoError(t, os.Mkdir(dir, 0700))
	record := schemaContract{Version: 49, Digest: "abc"}
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
	require.NoError(t, os.WriteFile(path, []byte(`{"digest":"abc","format":1,"version":49}`), 0600))
	_, err = loadSchemaContracts(dir)
	require.ErrorContains(t, err, `unknown object member name "format"`)
}

func TestStorageSchemaContractChanges(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			probe := func(statements ...string) string {
				_, err := db.ExecContext(t.Context(), "SAVEPOINT probe")
				require.NoError(t, err)
				for _, statement := range statements {
					_, err = db.ExecContext(t.Context(), statement)
					require.NoError(t, err)
				}
				digest := schemaCatalogDigest(t, db)
				_, err = db.ExecContext(t.Context(), "ROLLBACK TO probe")
				require.NoError(t, err)
				_, err = db.ExecContext(t.Context(), "RELEASE probe")
				require.NoError(t, err)
				return digest
			}
			digest := schemaCatalogDigest(t, db)
			record := schemaContract{Version: 49, Digest: digest}
			require.EqualError(t, checkSchemaContract(nil, 49, digest), fmt.Sprintf(`add schema contract 000049-%s.json: {"digest":"%s","version":49}`, digest, digest))
			require.NoError(t, checkSchemaContract([]schemaContract{record}, 49, digest))
			for _, statement := range []string{
				`CREATE TABLE contract_probe (id INTEGER)`,
				`ALTER TABLE nodes ADD COLUMN contract_probe TEXT`,
				`CREATE UNIQUE INDEX contract_probe ON nodes(id)`,
				`CREATE INDEX contract_probe ON nodes(id)`,
				`DROP INDEX nodes_parent`,
				`DROP INDEX nodes_parent; CREATE INDEX nodes_parent ON nodes(id)`,
			} {
				require.ErrorContains(t, checkSchemaContract([]schemaContract{record}, 49, probe(statement)), "set currentStorageSchemaVersion to 50", statement)
			}
			for _, pair := range [][2]string{
				{`CREATE TABLE proposal_a (id INTEGER)`, `CREATE TABLE proposal_b (id INTEGER)`},
				{`CREATE INDEX proposal_a ON nodes(id)`, `CREATE INDEX proposal_b ON nodes(name)`},
			} {
				main := schemaContract{Version: 49, Digest: probe(pair[0])}
				proposal := schemaContract{Version: 49, Digest: probe(pair[1])}
				combined := probe(pair[0], pair[1])
				require.ErrorContains(t, checkSchemaContract([]schemaContract{main, proposal}, 49, combined), "set currentStorageSchemaVersion to 50", pair[1])
				require.EqualError(t, checkSchemaContract([]schemaContract{main}, 50, combined), fmt.Sprintf(`add schema contract 000050-%s.json: {"digest":"%s","version":50}`, combined, combined))
				require.NoError(t, checkSchemaContract([]schemaContract{main, {Version: 50, Digest: combined}}, 50, combined))
			}
		})
	}
}

func TestStorageSchemaContractCollisions(t *testing.T) {
	t.Parallel()
	records := []schemaContract{{Version: 49, Digest: "main"}, {Version: 50, Digest: "next"}}
	require.EqualError(t, checkSchemaContract(records, 49, "main"), "schema version 49 conflicts with recorded layouts; preserve main's records, remove your old record, set currentStorageSchemaVersion to 51, rerun this test against the combined layout, and add the record it prints")
	require.NoError(t, checkSchemaContract(records, 50, "next"))
}
