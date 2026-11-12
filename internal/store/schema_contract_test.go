package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

type schemaContract struct {
	Version int
	Digest  string
}

func schemaCatalogDigest(t *testing.T, db *sql.DB) string {
	t.Helper()
	data, err := canonical.Marshal(releasedSchemaCatalogRows(t, db, true))
	require.NoError(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func schemaContractFile(record schemaContract) string {
	return fmt.Sprintf("%06d-%s.schema", record.Version, record.Digest)
}

var schemaContractName = regexp.MustCompile(`^([0-9]{6})-([0-9a-f]{64})\.schema$`)

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
		match := schemaContractName.FindStringSubmatch(entry.Name())
		if entry.IsDir() || match == nil {
			return nil, fmt.Errorf("invalid schema contract filename %s", entry.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, err
		}
		records = append(records, schemaContract{Version: version, Digest: match[2]})
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
		name := schemaContractFile(schemaContract{Version: version, Digest: digest})
		return fmt.Errorf("add empty schema contract file testdata/schema-contracts/%s", name)
	}
	return nil
}

func TestStorageSchemaVersionContract(t *testing.T) {
	t.Parallel()
	if slices.Contains(releasedStorageSchemaVersions, currentStorageSchemaVersion) {
		t.Skip("current schema version is released")
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
	record := schemaContract{Version: 49, Digest: strings.Repeat("a", 64)}
	name := schemaContractFile(record)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, nil, 0600))
	records, err = loadSchemaContracts(dir)
	require.NoError(t, err)
	require.Equal(t, []schemaContract{record}, records)
	require.NoError(t, os.Remove(path))
	for _, name := range []string{
		"49-" + record.Digest + ".schema",
		"000049-" + strings.Repeat("A", 64) + ".schema",
		"000049-abc.schema",
		"000049-" + record.Digest + ".json",
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0600))
			_, err := loadSchemaContracts(dir)
			require.ErrorContains(t, err, "invalid schema contract filename")
			require.NoError(t, os.Remove(filepath.Join(dir, name)))
		})
	}
}

func TestStorageSchemaContractChanges(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("a", 64)
	next := strings.Repeat("b", 64)
	records := []schemaContract{{Version: 49, Digest: digest}, {Version: 50, Digest: next}}
	require.EqualError(t, checkSchemaContract(nil, 49, digest), fmt.Sprintf("add empty schema contract file testdata/schema-contracts/000049-%s.schema", digest))
	require.NoError(t, checkSchemaContract(records[:1], 49, digest))
	require.EqualError(t, checkSchemaContract(records, 49, digest), "schema version 49 conflicts with recorded layouts; preserve main's records, remove your old record, set currentStorageSchemaVersion to 51, rerun this test against the combined layout, and add the record it prints")
	require.NoError(t, checkSchemaContract(records, 50, next))
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
			trigger := `CREATE TRIGGER contract_probe AFTER INSERT ON nodes BEGIN SELECT 1; END`
			require.ErrorContains(t, checkSchemaContract([]schemaContract{{Version: 49, Digest: probe(`CREATE TABLE contract_probe (id INTEGER)`)}}, 49, probe(`CREATE TABLE contract_probe (id INTEGER)`, trigger)), "set currentStorageSchemaVersion to 50")
			require.ErrorContains(t, checkSchemaContract([]schemaContract{{Version: 49, Digest: probe(`CREATE TABLE contract_probe (id INTEGER)`, trigger)}}, 49, probe(`CREATE TABLE contract_probe (id INTEGER, changed TEXT)`, trigger)), "set currentStorageSchemaVersion to 50")
			digest := schemaCatalogDigest(t, db)
			record := schemaContract{Version: 49, Digest: digest}
			for _, statement := range []string{
				`CREATE TABLE contract_probe (id INTEGER)`,
				`ALTER TABLE nodes ADD COLUMN contract_probe TEXT`,
				`CREATE INDEX contract_probe ON nodes(id)`,
				`DROP INDEX nodes_parent`,
				`DROP INDEX nodes_parent; CREATE INDEX nodes_parent ON nodes(id)`,
			} {
				require.ErrorContains(t, checkSchemaContract([]schemaContract{record}, 49, probe(statement)), "set currentStorageSchemaVersion to 50", statement)
			}
			a, b := `CREATE TABLE proposal_a (id INTEGER)`, `CREATE TABLE proposal_b (id INTEGER)`
			main := schemaContract{Version: 49, Digest: probe(a)}
			proposal := schemaContract{Version: 49, Digest: probe(b)}
			combined := probe(a, b)
			require.ErrorContains(t, checkSchemaContract([]schemaContract{main, proposal}, 49, combined), "set currentStorageSchemaVersion to 50")
			require.EqualError(t, checkSchemaContract([]schemaContract{main}, 50, combined), fmt.Sprintf("add empty schema contract file testdata/schema-contracts/000050-%s.schema", combined))
			require.NoError(t, checkSchemaContract([]schemaContract{main, {Version: 50, Digest: combined}}, 50, combined))
		})
	}
}
