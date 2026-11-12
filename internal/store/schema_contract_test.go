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
		if match == nil {
			return nil, fmt.Errorf("invalid schema contract filename %s", entry.Name())
		}
		version, _ := strconv.Atoi(match[1])
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read schema contract %s: %w", entry.Name(), err)
		}
		summary := strings.TrimSpace(string(body))
		if summary == "" || strings.ContainsAny(summary, "\r\n") {
			return nil, fmt.Errorf("schema contract %s must contain a one-line summary", entry.Name())
		}
		records = append(records, schemaContract{Version: version, Digest: match[2]})
	}
	return records, nil
}

func checkSchemaContract(records []schemaContract, version int, digest string) error {
	highest, matches, mismatches := 0, 0, 0
	for _, record := range records {
		highest = max(highest, record.Version)
		if record.Version == version {
			if record.Digest == digest {
				matches++
			} else {
				mismatches++
			}
		}
	}
	name := schemaContractFile(schemaContract{Version: version, Digest: digest})
	if version < highest {
		return fmt.Errorf("schema version %d conflicts with recorded layouts; preserve main's records, remove your old record, set currentStorageSchemaVersion to %d, rerun this test against the combined layout, and add the record it prints", version, highest+1)
	}
	if mismatches > 0 {
		return fmt.Errorf("schema version %d differs from recorded layouts; if the records are yours and unmerged, remove the old records and keep or add %s with a one-line summary; if main already records this version, preserve main's record, set currentStorageSchemaVersion to %d, rerun this test against the combined layout, and add the record it prints", version, name, highest+1)
	}
	if matches == 0 {
		return fmt.Errorf("add schema contract file testdata/schema-contracts/%s with a one-line summary", name)
	}
	return nil
}

func TestStorageSchemaVersionContract(t *testing.T) {
	t.Parallel()
	records, err := loadSchemaContracts("testdata/schema-contracts")
	require.NoError(t, err)
	if slices.Contains(releasedStorageSchemaVersions, currentStorageSchemaVersion) {
		t.Skip("current schema version is released")
	}
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			require.NoError(t, checkSchemaContract(records, currentStorageSchemaVersion, schemaCatalogDigest(t, db)))
		})
	}
}

func TestStorageSchemaContractRecords(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	record := schemaContract{Version: 49, Digest: strings.Repeat("a", 64)}
	name := schemaContractFile(record)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("Add documents table\n"), 0600))
	records, err := loadSchemaContracts(dir)
	require.NoError(t, err)
	require.Equal(t, []schemaContract{record}, records)
	for _, body := range []string{"", " \n\t", "first\nsecond\n"} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0600))
		_, err := loadSchemaContracts(dir)
		require.ErrorContains(t, err, "must contain a one-line summary")
	}
	require.NoError(t, os.Remove(path))
	for _, name := range []string{
		"49-" + record.Digest + ".schema",
		"000049-" + strings.Repeat("A", 64) + ".schema",
		"000049-abc.schema",
		"000049-" + record.Digest + ".json",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0600))
		_, err := loadSchemaContracts(dir)
		require.ErrorContains(t, err, "invalid schema contract filename "+name)
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
	}
}

func TestStorageSchemaContractChanges(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("a", 64)
	next := strings.Repeat("b", 64)
	records := []schemaContract{{Version: 49, Digest: digest}, {Version: 50, Digest: next}}
	require.EqualError(t, checkSchemaContract(nil, 49, digest), fmt.Sprintf("add schema contract file testdata/schema-contracts/000049-%s.schema with a one-line summary", digest))
	require.NoError(t, checkSchemaContract(records[:1], 49, digest))
	err := checkSchemaContract(records[:1], 49, next)
	require.ErrorContains(t, err, "remove the old records and keep or add 000049-"+next+".schema")
	require.ErrorContains(t, err, "if main already records this version")
	require.EqualError(t, checkSchemaContract(records, 49, digest), "schema version 49 conflicts with recorded layouts; preserve main's records, remove your old record, set currentStorageSchemaVersion to 51, rerun this test against the combined layout, and add the record it prints")
	require.NoError(t, checkSchemaContract(records, 50, next))
	err = checkSchemaContract([]schemaContract{{Version: 49, Digest: digest}, {Version: 49, Digest: next}}, 49, digest)
	require.ErrorContains(t, err, "remove the old records and keep or add 000049-"+digest+".schema")
	require.ErrorContains(t, err, "if main already records this version")
	require.ErrorContains(t, err, "set currentStorageSchemaVersion to 50")
}

func TestStorageSchemaCatalogDigest(t *testing.T) {
	t.Parallel()
	db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), DefaultSQLiteDriver(), schemaSQL, currentStorageSchemaVersion)
	probe := func(statement string) string {
		_, err := db.ExecContext(t.Context(), "SAVEPOINT probe")
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
		digest := schemaCatalogDigest(t, db)
		_, err = db.ExecContext(t.Context(), "ROLLBACK TO probe")
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), "RELEASE probe")
		require.NoError(t, err)
		return digest
	}
	trigger := `CREATE TRIGGER contract_probe AFTER INSERT ON nodes BEGIN SELECT 1; END`
	require.NotEqual(t, probe(trigger), probe(trigger+`; CREATE TABLE contract_probe (id INTEGER)`))
	digest := schemaCatalogDigest(t, db)
	for _, statement := range []string{
		`ALTER TABLE nodes ADD COLUMN contract_probe TEXT`,
		`CREATE INDEX contract_probe ON nodes(id)`,
		trigger,
		`DROP INDEX nodes_parent; CREATE INDEX nodes_parent ON nodes(id)`,
	} {
		require.NotEqual(t, digest, probe(statement), statement)
	}
}
