package sqlite_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

type driverObservations struct {
	name                     string
	validateOK               bool
	nilValidateRejected      bool
	emptyNameRejected        bool
	create                   bool
	readWriteExisting        bool
	missingReadWriteRejected bool
	readOnlyRead             bool
	readOnlyWriteRejected    bool
	readOnlyImmediate        bool
	invalidAccessRejected    bool
	createWAL                bool
	createForeignKeys        bool
	defaultBusyTimeout       int64
	explicitBusyTimeout      int64
	journalMode              string
	foreignKeysValue         int64
	wal                      bool
	foreignKeys              bool
	deferredWriterAllowed    bool
	immediateBusy            bool
	specialPath              bool
	relativePath             bool
	independentPools         bool
	realBusy                 bool
	wrappedBusy              bool
	realUnique               bool
	wrappedUnique            bool
	nilFalse                 bool
	textFalse                bool
	notNullFalse             bool
	checkFalse               bool
	foreignKeyFalse          bool
	syntaxFalse              bool
	primaryKeyFalse          bool
}

type emptyNameDriver struct{}

func (emptyNameDriver) Name() string { return "" }

func (emptyNameDriver) Open(string, docsqlite.OpenOptions) (*sql.DB, error) {
	return nil, errors.New("empty-name test driver is not openable")
}

func (emptyNameDriver) IsBusy(error) bool            { return false }
func (emptyNameDriver) IsUniqueViolation(error) bool { return false }

func TestModerncDriverContract(t *testing.T) {
	observations := exerciseDriverContract(t, modernc.Driver{})
	assertDriverContract(t, observations)
}

func exerciseDriverContract(t *testing.T, driver docsqlite.Driver) driverObservations {
	t.Helper()
	var observations driverObservations

	observations.name = driver.Name()
	observations.validateOK = docsqlite.Validate(driver) == nil
	observations.nilValidateRejected = docsqlite.Validate(nil) != nil
	observations.emptyNameRejected = docsqlite.Validate(emptyNameDriver{}) != nil

	baseDir := t.TempDir()
	databasePath := filepath.Join(baseDir, "contract.db")
	createContractDatabase(t, driver, databasePath)
	resetJournalMode(t, driver, databasePath)

	db, err := driver.Open(databasePath, docsqlite.OpenOptions{
		Access:          docsqlite.ReadWriteExisting,
		TransactionMode: docsqlite.Deferred,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	require.NoError(t, db.PingContext(t.Context()))
	observations.readWriteExisting = execAndReadMarker(t, db, "updated")
	observations.defaultBusyTimeout = pragmaInt(t, db, "busy_timeout")
	observations.journalMode = strings.ToLower(pragmaString(t, db, "journal_mode"))
	observations.wal = observations.journalMode == "wal"
	observations.foreignKeysValue = pragmaInt(t, db, "foreign_keys")
	observations.foreignKeys = observations.foreignKeysValue == 1
	require.NoError(t, db.Close())

	missingDB, err := driver.Open(filepath.Join(baseDir, "missing.db"), docsqlite.OpenOptions{
		Access:          docsqlite.ReadWriteExisting,
		TransactionMode: docsqlite.Deferred,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, missingDB.Close()) }()
	observations.missingReadWriteRejected = missingDB.PingContext(t.Context()) != nil
	require.NoError(t, missingDB.Close())

	readOnly, err := driver.Open(databasePath, docsqlite.OpenOptions{
		Access:          docsqlite.ReadOnlyImmutable,
		TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, readOnly.Close()) }()
	require.NoError(t, readOnly.PingContext(t.Context()))
	var marker string
	require.NoError(t, readOnly.QueryRowContext(t.Context(),
		`SELECT value FROM contract WHERE id = 1`).Scan(&marker))
	observations.readOnlyRead = marker == "updated"
	_, writeErr := readOnly.ExecContext(t.Context(), `UPDATE contract SET value = 'rejected' WHERE id = 1`)
	observations.readOnlyWriteRejected = writeErr != nil
	readOnlyTx, err := readOnly.BeginTx(t.Context(), nil)
	observations.readOnlyImmediate = err == nil
	if err == nil {
		t.Cleanup(func() { _ = readOnlyTx.Rollback() })
		require.NoError(t, readOnlyTx.QueryRowContext(t.Context(),
			`SELECT value FROM contract WHERE id = 1`).Scan(&marker))
		require.NoError(t, readOnlyTx.Rollback())
	}
	require.NoError(t, readOnly.Close())

	invalidDB, err := driver.Open(filepath.Join(baseDir, "invalid.db"), docsqlite.OpenOptions{
		Access:          docsqlite.AccessMode(255),
		TransactionMode: docsqlite.Deferred,
	})
	observations.invalidAccessRejected = err != nil && invalidDB == nil
	if invalidDB != nil {
		require.NoError(t, invalidDB.Close())
	}

	explicitDB, err := driver.Open(databasePath, docsqlite.OpenOptions{
		Access:          docsqlite.ReadWriteExisting,
		TransactionMode: docsqlite.Deferred,
		BusyTimeout:     1234 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, explicitDB.Close()) }()
	require.NoError(t, explicitDB.PingContext(t.Context()))
	observations.explicitBusyTimeout = pragmaInt(t, explicitDB, "busy_timeout")
	require.NoError(t, explicitDB.Close())

	observations.create, observations.createWAL, observations.createForeignKeys = observeCreate(t, driver, filepath.Join(baseDir, "created.db"))
	observations.deferredWriterAllowed, observations.immediateBusy, observations.realBusy,
		observations.wrappedBusy = observeTransactionLocks(t, driver, baseDir)
	observations.specialPath = observeSpecialPath(t, driver, baseDir)
	observations.relativePath = observeRelativePath(t, driver, baseDir)
	observations.independentPools = observeIndependentPools(t, driver, databasePath)
	observations.realUnique, observations.wrappedUnique = observeUniqueClassification(t, driver, databasePath)
	observations.nilFalse = !driver.IsBusy(nil) && !driver.IsUniqueViolation(nil)
	observations.textFalse = !driver.IsBusy(errors.New("database is locked")) &&
		!driver.IsUniqueViolation(errors.New("UNIQUE constraint failed: contract.value"))
	observations.notNullFalse, observations.checkFalse, observations.foreignKeyFalse,
		observations.syntaxFalse, observations.primaryKeyFalse = observeFalsePositiveClassifications(t, driver, databasePath)

	return observations
}

func assertDriverContract(t *testing.T, observations driverObservations) {
	t.Helper()
	require.NotEmpty(t, observations.name)
	require.True(t, observations.validateOK)
	require.True(t, observations.nilValidateRejected)
	require.True(t, observations.emptyNameRejected)
	require.True(t, observations.create)
	require.True(t, observations.readWriteExisting)
	require.True(t, observations.missingReadWriteRejected)
	require.True(t, observations.readOnlyRead)
	require.True(t, observations.readOnlyWriteRejected)
	require.True(t, observations.readOnlyImmediate)
	require.True(t, observations.invalidAccessRejected)
	require.True(t, observations.createWAL)
	require.True(t, observations.createForeignKeys)
	require.Equal(t, int64(5000), observations.defaultBusyTimeout)
	require.Equal(t, int64(1234), observations.explicitBusyTimeout)
	require.True(t, observations.wal)
	require.True(t, observations.foreignKeys)
	require.True(t, observations.deferredWriterAllowed)
	require.True(t, observations.immediateBusy)
	require.True(t, observations.specialPath)
	require.True(t, observations.relativePath)
	require.True(t, observations.independentPools)
	require.True(t, observations.realBusy)
	require.True(t, observations.wrappedBusy)
	require.True(t, observations.realUnique)
	require.True(t, observations.wrappedUnique)
	require.True(t, observations.nilFalse)
	require.True(t, observations.textFalse)
	require.True(t, observations.notNullFalse)
	require.True(t, observations.checkFalse)
	require.True(t, observations.foreignKeyFalse)
	require.True(t, observations.syntaxFalse)
	require.True(t, observations.primaryKeyFalse)
}

func createContractDatabase(t *testing.T, driver docsqlite.Driver, path string) {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access:          docsqlite.Create,
		TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	require.NoError(t, db.PingContext(t.Context()))
	for _, statement := range []string{
		`CREATE TABLE contract (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE, checked INTEGER NOT NULL CHECK (checked > 0), parent_id INTEGER NOT NULL REFERENCES parent(id))`,
		`CREATE TABLE parent (id INTEGER PRIMARY KEY)`,
		`INSERT INTO parent (id) VALUES (1)`,
		`INSERT INTO contract (id, value, checked, parent_id) VALUES (1, 'original', 1, 1)`,
	} {
		_, err = db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
}

func resetJournalMode(t *testing.T, driver docsqlite.Driver, path string) {
	t.Helper()
	db := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	var mode string
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA journal_mode=DELETE").Scan(&mode))
	require.Equal(t, "delete", strings.ToLower(mode))
	require.NoError(t, db.Close())
}

func observeCreate(t *testing.T, driver docsqlite.Driver, path string) (created, wal, foreignKeys bool) {
	t.Helper()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access:          docsqlite.Create,
		TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	require.NoError(t, db.PingContext(t.Context()))
	wal = strings.EqualFold(pragmaString(t, db, "journal_mode"), "wal")
	foreignKeys = pragmaInt(t, db, "foreign_keys") == 1
	_, err = db.ExecContext(t.Context(), `CREATE TABLE created (value TEXT)`)
	if err != nil {
		require.NoError(t, db.Close())
		return false, wal, foreignKeys
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO created (value) VALUES ('synthetic')`)
	created = err == nil
	require.NoError(t, db.Close())
	return created, wal, foreignKeys
}

func execAndReadMarker(t *testing.T, db *sql.DB, value string) bool {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `UPDATE contract SET value = ? WHERE id = 1`, value)
	if err != nil {
		return false
	}
	var marker string
	if err := db.QueryRowContext(t.Context(), `SELECT value FROM contract WHERE id = 1`).Scan(&marker); err != nil {
		return false
	}
	return marker == value
}

func observeTransactionLocks(t *testing.T, driver docsqlite.Driver, baseDir string) (deferredWriterAllowed, immediateBusy, realBusy, wrappedBusy bool) {
	t.Helper()
	deferredPath := filepath.Join(baseDir, "deferred.db")
	createContractDatabase(t, driver, deferredPath)
	deferredOne := openAndPing(t, driver, deferredPath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred, BusyTimeout: 50 * time.Millisecond,
	})
	deferredTwo := openAndPing(t, driver, deferredPath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred, BusyTimeout: 50 * time.Millisecond,
	})
	deferredTx, err := deferredOne.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = deferredTwo.ExecContext(t.Context(), `UPDATE contract SET value = 'deferred' WHERE id = 1`)
	deferredWriterAllowed = err == nil
	require.NoError(t, deferredTx.Rollback())
	require.NoError(t, deferredOne.Close())
	require.NoError(t, deferredTwo.Close())

	immediatePath := filepath.Join(baseDir, "immediate.db")
	createContractDatabase(t, driver, immediatePath)
	immediateOne := openAndPing(t, driver, immediatePath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate, BusyTimeout: 50 * time.Millisecond,
	})
	immediateTwo := openAndPing(t, driver, immediatePath, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred, BusyTimeout: 50 * time.Millisecond,
	})
	immediateTx, err := immediateOne.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = immediateTwo.ExecContext(t.Context(), `UPDATE contract SET value = 'blocked' WHERE id = 1`)
	immediateBusy = err != nil && driver.IsBusy(err)
	realBusy = immediateBusy
	wrappedBusy = err != nil && driver.IsBusy(fmt.Errorf("busy wrapper: %w", err))
	require.NoError(t, immediateTx.Rollback())
	require.NoError(t, immediateOne.Close())
	require.NoError(t, immediateTwo.Close())
	return deferredWriterAllowed, immediateBusy, realBusy, wrappedBusy
}

func observeSpecialPath(t *testing.T, driver docsqlite.Driver, baseDir string) bool {
	t.Helper()
	path := filepath.Join(baseDir, "name with spaces & = #.db")
	db := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Deferred,
	})
	_, err := db.ExecContext(t.Context(), `CREATE TABLE special (value TEXT)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = os.Stat(path)
	return err == nil
}

func observeRelativePath(t *testing.T, driver docsqlite.Driver, baseDir string) bool {
	t.Helper()
	t.Chdir(baseDir)
	const path = "relative name & = #.db"
	db := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Deferred,
	})
	_, err := db.ExecContext(t.Context(), `CREATE TABLE relative (value TEXT)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = os.Stat(path)
	return err == nil
}

func observeIndependentPools(t *testing.T, driver docsqlite.Driver, path string) bool {
	t.Helper()
	first := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred, BusyTimeout: 17 * time.Millisecond,
	})
	second := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred, BusyTimeout: 29 * time.Millisecond,
	})
	firstTimeout := pragmaInt(t, first, "busy_timeout")
	secondTimeout := pragmaInt(t, second, "busy_timeout")
	require.NoError(t, first.Close())
	_, err := second.ExecContext(t.Context(), `UPDATE contract SET value = 'independent' WHERE id = 1`)
	if err != nil {
		require.NoError(t, second.Close())
		return false
	}
	require.NoError(t, second.Close())
	return firstTimeout == 17 && secondTimeout == 29
}

func observeUniqueClassification(t *testing.T, driver docsqlite.Driver, path string) (classified, wrapped bool) {
	t.Helper()
	db := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO contract (id, value, checked, parent_id) VALUES (2, 'independent', 1, 1)`)
	classified = err != nil && driver.IsUniqueViolation(err)
	wrapped = err != nil && driver.IsUniqueViolation(fmt.Errorf("unique wrapper: %w", err))
	require.NoError(t, db.Close())
	return classified, wrapped
}

func observeFalsePositiveClassifications(t *testing.T, driver docsqlite.Driver, path string) (bool, bool, bool, bool, bool) {
	t.Helper()
	db := openAndPing(t, driver, path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	constraintIsFalse := func(err error) bool {
		return err != nil && !driver.IsBusy(err) && !driver.IsUniqueViolation(err)
	}
	_, notNullErr := db.ExecContext(t.Context(),
		`INSERT INTO contract (id, value, checked, parent_id) VALUES (3, NULL, 1, 1)`)
	_, checkErr := db.ExecContext(t.Context(),
		`INSERT INTO contract (id, value, checked, parent_id) VALUES (3, 'check', 0, 1)`)
	_, foreignKeyErr := db.ExecContext(t.Context(),
		`INSERT INTO contract (id, value, checked, parent_id) VALUES (3, 'foreign', 1, 99)`)
	_, syntaxErr := db.ExecContext(t.Context(), `INSRT INTO contract (id) VALUES (3)`)
	_, primaryKeyErr := db.ExecContext(t.Context(),
		`INSERT INTO contract (id, value, checked, parent_id) VALUES (1, 'primary', 1, 1)`)
	require.NoError(t, db.Close())
	return constraintIsFalse(notNullErr), constraintIsFalse(checkErr),
		constraintIsFalse(foreignKeyErr), constraintIsFalse(syntaxErr), constraintIsFalse(primaryKeyErr)
}

func openAndPing(t *testing.T, driver docsqlite.Driver, path string, options docsqlite.OpenOptions) *sql.DB {
	t.Helper()
	db, err := driver.Open(path, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(t.Context()))
	return db
}

func pragmaInt(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var value int64
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&value))
	return value
}

func pragmaString(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var value string
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&value))
	return value
}
