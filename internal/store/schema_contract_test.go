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
	rows := releasedSchemaCatalogRows(t, db, true)
	rows = slices.DeleteFunc(rows, func(row [4]string) bool {
		if row[0] != "index" {
			return false
		}
		var unique bool
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT "unique" FROM pragma_index_list(?) WHERE name=?`, row[2], row[1]).Scan(&unique))
		return !unique
	})
	data, err := canonical.Marshal(rows)
	require.NoError(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func schemaContractFile(record schemaContract, summaryDigest string) string {
	return fmt.Sprintf("%06d-%s-%s.schema", record.Version, record.Digest, summaryDigest)
}

var schemaContractName = regexp.MustCompile(`^([0-9]{6})-([^-]+)-([^-]+)\.schema$`)

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
		if match == nil || !canonical.IsSHA256Hex(match[2]) || !canonical.IsSHA256Hex(match[3]) {
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
		if fmt.Sprintf("%x", sha256.Sum256(body)) != match[3] {
			return nil, fmt.Errorf("schema contract %s summary hash does not match its contents", entry.Name())
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
	name := schemaContractFile(schemaContract{Version: version, Digest: digest}, "<summary-sha256>")
	guidance := "write a one-line summary, hash its exact file bytes with sha256sum, and replace <summary-sha256> in " + name
	if version < highest {
		return fmt.Errorf("schema version %d conflicts with recorded layouts; preserve main's records, remove your old record, set currentStorageSchemaVersion to %d, rerun this test against the combined layout, and add the record it prints", version, highest+1)
	}
	if mismatches > 0 {
		return fmt.Errorf("schema version %d differs from recorded layouts; if the records are yours and unmerged, remove the old records and %s; if main already records this version, preserve main's record, set currentStorageSchemaVersion to %d, rerun this test against the combined layout, and add the record it prints", version, guidance, highest+1)
	}
	if matches == 0 {
		return fmt.Errorf("add a schema contract under testdata/schema-contracts: %s", guidance)
	}
	return nil
}

func schemaContractReleased(records []schemaContract, version int, released []int) (bool, error) {
	for _, record := range records {
		if record.Version > version {
			return false, fmt.Errorf("schema contract version %d exceeds currentStorageSchemaVersion %d", record.Version, version)
		}
	}
	return slices.Contains(released, version), nil
}

func TestStorageSchemaVersionContract(t *testing.T) {
	t.Parallel()
	records, err := loadSchemaContracts("testdata/schema-contracts")
	require.NoError(t, err)
	released, err := schemaContractReleased(records, currentStorageSchemaVersion, releasedStorageSchemaVersions)
	require.NoError(t, err)
	if released {
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
	digest := strings.Repeat("a", 64)
	names := []string{
		"000049-" + digest + "-9b6914ea954e6023e027151042eab8f33e298aa5aa83dec9f421427f14ded361.schema",
		"000049-" + digest + "-03c8fb4f55e145ae35268d4dc38370db92219fede5bc902a5d17c4ffda75783c.schema",
	}
	for i, body := range []string{"Add documents table\n", "Add records table\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, names[i]), []byte(body), 0600))
	}
	records, err := loadSchemaContracts(dir)
	require.NoError(t, err)
	require.Equal(t, []schemaContract{{Version: 49, Digest: digest}, {Version: 49, Digest: digest}}, records)
	require.NoError(t, checkSchemaContract(records, 49, digest))
	released, err := schemaContractReleased(records, 48, []int{48})
	require.ErrorContains(t, err, "schema contract version 49 exceeds currentStorageSchemaVersion 48")
	require.False(t, released)

	path := filepath.Join(dir, names[0])
	for _, body := range []string{"", " \n\t", "first\nsecond\n"} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0600))
		_, err := loadSchemaContracts(dir)
		require.ErrorContains(t, err, "must contain a one-line summary")
	}
	require.NoError(t, os.WriteFile(path, []byte("Changed summary\n"), 0600))
	_, err = loadSchemaContracts(dir)
	require.ErrorContains(t, err, "summary hash does not match")
	require.NoError(t, os.Remove(path))
	for _, name := range []string{
		"49-" + digest + "-" + digest + ".schema",
		"000049-" + strings.Repeat("A", 64) + "-" + digest + ".schema",
		"000049-" + digest + "-abc.schema",
		"000049-" + digest + "-" + digest + ".json",
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
	require.ErrorContains(t, checkSchemaContract(nil, 49, digest), "000049-"+digest+"-<summary-sha256>.schema")
	require.NoError(t, checkSchemaContract(records[:1], 49, digest))
	err := checkSchemaContract(records[:1], 49, next)
	require.ErrorContains(t, err, "000049-"+next+"-<summary-sha256>.schema")
	require.ErrorContains(t, err, "set currentStorageSchemaVersion to 50")
	require.ErrorContains(t, checkSchemaContract(records, 49, digest), "set currentStorageSchemaVersion to 51")
	require.NoError(t, checkSchemaContract(records, 50, next))
	err = checkSchemaContract([]schemaContract{{Version: 49, Digest: digest}, {Version: 49, Digest: next}}, 49, digest)
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
	require.Equal(t, digest, probe(`CREATE INDEX contract_probe ON nodes(id)`))
	for _, statement := range []string{
		`ALTER TABLE nodes ADD COLUMN contract_probe TEXT`,
		`CREATE UNIQUE INDEX contract_probe ON nodes(id)`,
		trigger,
	} {
		require.NotEqual(t, digest, probe(statement), statement)
	}
}
