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

	"github.com/sqlc-dev/meyer/lexer"
	"github.com/sqlc-dev/meyer/token"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

type schemaContract struct {
	Version int
	Digest  string
}

type schemaDigestRow struct {
	Type   string
	Name   string
	Table  string
	Tokens []string
}

func schemaRowsDigest(rows [][4]string) (string, error) {
	var catalog []schemaDigestRow
	for _, row := range rows {
		entry := schemaDigestRow{Type: row[0], Name: row[1], Table: row[2]}
		for _, tok := range lexer.Lex(row[3]) {
			switch tok.Kind {
			case token.ILLEGAL:
				return "", fmt.Errorf("invalid SQL token %q in %s", tok.Text(row[3]), row[1])
			case token.EOF:
				if tok.Pos != len(row[3]) {
					return "", fmt.Errorf("incomplete SQL for %s", row[1])
				}
			default:
				entry.Tokens = append(entry.Tokens, tok.Text(row[3]))
			}
		}
		catalog = append(catalog, entry)
	}
	data, err := canonical.Marshal(catalog)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
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
	digest, err := schemaRowsDigest(rows)
	require.NoError(t, err)
	return digest
}

func schemaContractFile(record schemaContract, summarySlug string) string {
	return fmt.Sprintf("%06d-%s-%s.schema", record.Version, record.Digest, summarySlug)
}

var schemaContractName = regexp.MustCompile(`^([0-9]{6})-([^-]+)-([a-z0-9]+(?:-[a-z0-9]+)*)\.schema$`)

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
		if match == nil || !canonical.IsSHA256Hex(match[2]) || len(match[3]) > 48 {
			return nil, fmt.Errorf("invalid schema contract filename %s", entry.Name())
		}
		version, _ := strconv.Atoi(match[1])
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect schema contract %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return nil, fmt.Errorf("schema contract %s must be an empty regular file", entry.Name())
		}
		records = append(records, schemaContract{Version: version, Digest: match[2]})
	}
	return records, nil
}

func checkSchemaContract(records []schemaContract, version int, digest string) error {
	matches, mismatches := 0, 0
	for _, record := range records {
		if record.Version == version {
			if record.Digest == digest {
				matches++
			} else {
				mismatches++
			}
		}
	}
	name := schemaContractFile(schemaContract{Version: version, Digest: digest}, "<summary-slug>")
	guidance := "add an empty file with a short lowercase hyphenated summary in " + name
	if mismatches > 0 {
		return fmt.Errorf("schema version %d differs from recorded layouts; if the records are yours and unmerged, remove the old records and %s; if main already records this version, preserve main's record, set currentStorageSchemaVersion to %d, rerun this test against the combined layout, and add the record it prints", version, guidance, version+1)
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
	for _, name := range []string{schemaContractFile(schemaContract{Version: 49, Digest: digest}, "add-documents"), "000049-" + digest + "-add-records.schema"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0600))
	}
	records, err := loadSchemaContracts(dir)
	require.NoError(t, err)
	require.Equal(t, []schemaContract{{Version: 49, Digest: digest}, {Version: 49, Digest: digest}}, records)

	for _, name := range []string{
		"49-" + digest + "-valid.schema",
		"000049-" + strings.Repeat("A", 64) + "-valid.schema",
		"000049-abc-valid.schema",
		"000049-" + digest + "-.schema",
		"000049-" + digest + "-Upper.schema",
		"000049-" + digest + "-two--words.schema",
		"000049-" + digest + "-" + strings.Repeat("a", 49) + ".schema",
	} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, nil, 0600))
		_, err := loadSchemaContracts(dir)
		require.ErrorContains(t, err, "invalid schema contract filename")
		require.NoError(t, os.Remove(path))
	}
	path := filepath.Join(dir, "000049-"+digest+"-invalid.schema")
	require.NoError(t, os.WriteFile(path, []byte("summary"), 0600))
	_, err = loadSchemaContracts(dir)
	require.ErrorContains(t, err, "must be an empty regular file")
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	_, err = loadSchemaContracts(dir)
	require.ErrorContains(t, err, "must be an empty regular file")
}

func TestStorageSchemaContractChanges(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("a", 64)
	next := strings.Repeat("b", 64)
	records := []schemaContract{{Version: 49, Digest: digest}, {Version: 50, Digest: next}}
	require.ErrorContains(t, checkSchemaContract(nil, 49, digest), "000049-"+digest+"-<summary-slug>.schema")
	require.NoError(t, checkSchemaContract(records[:1], 49, digest))
	require.NoError(t, checkSchemaContract([]schemaContract{{Version: 49, Digest: digest}, {Version: 49, Digest: digest}}, 49, digest))
	err := checkSchemaContract(records[:1], 49, next)
	require.ErrorContains(t, err, "000049-"+next+"-<summary-slug>.schema")
	require.ErrorContains(t, err, "set currentStorageSchemaVersion to 50")
	require.NoError(t, checkSchemaContract(records, 50, next))
	err = checkSchemaContract([]schemaContract{{Version: 49, Digest: digest}, {Version: 49, Digest: next}}, 49, digest)
	require.ErrorContains(t, err, "set currentStorageSchemaVersion to 50")
}

func TestStorageSchemaContractReleased(t *testing.T) {
	t.Parallel()
	released, err := schemaContractReleased(nil, 48, []int{48})
	require.NoError(t, err)
	require.True(t, released)
	released, err = schemaContractReleased(nil, 49, []int{48})
	require.NoError(t, err)
	require.False(t, released)
	_, err = schemaContractReleased([]schemaContract{{Version: 49}}, 48, []int{48})
	require.ErrorContains(t, err, "schema contract version 49 exceeds currentStorageSchemaVersion 48")
}

func TestStorageSchemaTokenDigest(t *testing.T) {
	t.Parallel()
	row := [][4]string{{"table", "t", "t", "CREATE TABLE t (id INTEGER)"}}
	digest, err := schemaRowsDigest(row)
	require.NoError(t, err)
	require.Equal(t, "b30c0e59243ff78aa209ca6ca6a9f5c95ab77385a8e91542969c4da4fdb40382", digest)
	for _, pair := range [][2]string{
		{`CREATE TABLE t (a TEXT DEFAULT 'a b')`, `CREATE TABLE t (a TEXT DEFAULT 'ab')`},
		{`CREATE TABLE t (a INTEGER CHECK(a+2))`, `CREATE TABLE t (a INTEGER CHECK(a-2))`},
		{`CREATE TABLE t (a INTEGER)`, `create table t (a INTEGER)`},
	} {
		row[0][3] = pair[0]
		first, err := schemaRowsDigest(row)
		require.NoError(t, err)
		row[0][3] = pair[1]
		second, err := schemaRowsDigest(row)
		require.NoError(t, err)
		require.NotEqual(t, first, second, pair)
	}
	for _, sql := range []string{"CREATE TABLE t (a TEXT DEFAULT 'unterminated", "CREATE TABLE t (a INTEGER)\x00"} {
		row[0][3] = sql
		_, err := schemaRowsDigest(row)
		require.Error(t, err, sql)
	}
}

func TestStorageSchemaCatalogChanges(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			db, _ := newReleasedFixtureStore(t, filepath.Join(t.TempDir(), "current.db"), driver.driver, schemaSQL, currentStorageSchemaVersion)
			probe := func(sql string) string {
				_, err := db.ExecContext(t.Context(), "SAVEPOINT probe")
				require.NoError(t, err)
				_, err = db.ExecContext(t.Context(), sql)
				require.NoError(t, err)
				digest := schemaCatalogDigest(t, db)
				_, err = db.ExecContext(t.Context(), "ROLLBACK TO probe; RELEASE probe")
				require.NoError(t, err)
				return digest
			}
			trigger := `CREATE TRIGGER contract_probe AFTER INSERT ON nodes BEGIN SELECT 1; END`
			baseDigest := schemaCatalogDigest(t, db)
			var unique bool
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT "unique" FROM pragma_index_list(?) WHERE name=?`, "nodes", "nodes_parent").Scan(&unique))
			require.False(t, unique)
			for _, statement := range []string{
				`CREATE INDEX contract_probe ON nodes(id)`,
				`DROP INDEX nodes_parent; CREATE INDEX nodes_parent ON nodes(id)`,
				`DROP INDEX nodes_parent`,
			} {
				require.Equal(t, baseDigest, probe(statement), statement)
			}
			for _, statement := range []string{
				`CREATE UNIQUE INDEX contract_probe ON nodes(id)`,
				`ALTER TABLE nodes ADD COLUMN contract_probe TEXT`,
				`CREATE VIEW contract_v AS SELECT id FROM nodes`,
				trigger,
			} {
				require.NotEqual(t, baseDigest, probe(statement), statement)
			}
			base := probe(`CREATE TABLE contract_a (id INTEGER CHECK (id > 1))`)
			require.Equal(t, base, probe("CREATE TABLE contract_a  ( id INTEGER /* note */ CHECK(id>1) )"))
			first := probe(`CREATE TABLE contract_a (id INTEGER)`)
			second := probe(`CREATE TABLE contract_b (id INTEGER)`)
			combined := probe(`CREATE TABLE contract_a (id INTEGER); CREATE TABLE contract_b (id INTEGER)`)
			records := []schemaContract{{Version: 49, Digest: first}, {Version: 49, Digest: second}}
			require.NoError(t, checkSchemaContract(records[:1], 49, first))
			require.NoError(t, checkSchemaContract(records[1:], 49, second))
			require.ErrorContains(t, checkSchemaContract(records, 49, combined), "set currentStorageSchemaVersion to 50")
			require.NoError(t, checkSchemaContract(append(records, schemaContract{Version: 50, Digest: combined}), 50, combined))
		})
	}
}
