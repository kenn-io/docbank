// Package pgtest owns disposable databases on the configured PostgreSQL test server.
package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// NewDatabase creates a database on the explicitly configured test server.
// CI supplies a disposable cluster; local runs explicitly configure one.
// Every test owns a separate database, so it can exercise real role grants and
// failed DDL.
func NewDatabase(tb testing.TB) (*sql.DB, string) {
	tb.Helper()
	return newDatabase(tb, "")
}

// CopyDatabase creates a database as a copy of template, which no session may
// be connected to. Copying takes none of the locks that installing the same
// objects with DDL would.
func CopyDatabase(tb testing.TB, template string) (*sql.DB, string) {
	tb.Helper()
	return newDatabase(tb, template)
}

func newDatabase(tb testing.TB, template string) (*sql.DB, string) {
	tb.Helper()
	c := ServerConfig(tb)
	name := fmt.Sprintf("db_%x", uuid.New())
	create := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if template != "" {
		// FILE_COPY copies the template's files. The default, WAL_LOG, reads
		// every page through the server's buffers and writes it to the WAL;
		// under a full test run that took ten times as long.
		create += " TEMPLATE " + pgx.Identifier{template}.Sanitize() + " STRATEGY FILE_COPY"
	}
	admin := stdlib.OpenDB(*c)
	_, err := admin.ExecContext(tb.Context(), create)
	require.NoError(tb, errors.Join(err, admin.Close()))
	tb.Cleanup(func() { drops.add(name) })
	c.Database = name
	db := stdlib.OpenDB(*c)
	tb.Cleanup(func() { require.NoError(tb, db.Close()) })
	return db, c.User
}

// drops removes the databases that tests no longer use. DROP DATABASE waits
// for a checkpoint, and the server runs one checkpoint at a time, so tests
// that dropped their own databases waited for each other. The tests' cleanup
// only queues the drop, and several drops share each checkpoint.
var drops = startDrops()

// pendingDrops bounds the databases a process has queued and not yet
// dropped, so a server whose data directory is in memory keeps few of them.
const pendingDrops = 16

// databaseDrops counts queued drops under a mutex: tests in synctest bubbles
// queue them, and a sync.WaitGroup must not be shared with a bubble.
type databaseDrops struct {
	queue   chan string
	mu      sync.Mutex
	done    *sync.Cond
	pending int
	err     error
}

func startDrops() *databaseDrops {
	d := &databaseDrops{queue: make(chan string, pendingDrops)}
	d.done = sync.NewCond(&d.mu)
	// The goroutine starts with the process, outside any synctest bubble,
	// and serves every test for the life of the process. Drops from the
	// test processes that run at once share checkpoints.
	go d.run()
	return d
}

func (d *databaseDrops) add(name string) {
	d.mu.Lock()
	d.pending++
	d.mu.Unlock()
	d.queue <- name
}

func (d *databaseDrops) run() {
	// The drops outlive the tests that queued them.
	ctx := context.Background()
	var admin *sql.DB
	for name := range d.queue {
		err := func() error {
			if admin == nil {
				config, err := pgx.ParseConfig(os.Getenv("DOCBANK_TEST_POSTGRES_DSN"))
				if err != nil {
					return fmt.Errorf("parse DOCBANK_TEST_POSTGRES_DSN: %w", err)
				}
				admin = stdlib.OpenDB(*config)
				// Connections count against the server's limit, which the
				// tests share, so none stays open between drops.
				admin.SetMaxIdleConns(0)
			}
			_, err := admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
			return err
		}()
		d.mu.Lock()
		if err != nil {
			d.err = errors.Join(d.err, fmt.Errorf("dropping test database %s: %w", name, err))
		}
		d.pending--
		d.done.Broadcast()
		d.mu.Unlock()
	}
}

// Finish waits until every test database is dropped and returns code, or 1
// when a drop failed. TestMain of each package that creates test databases
// passes it the tests' exit code.
func Finish(code int) int {
	drops.mu.Lock()
	defer drops.mu.Unlock()
	for drops.pending > 0 {
		drops.done.Wait()
	}
	if drops.err != nil {
		fmt.Fprintln(os.Stderr, drops.err)
		return 1
	}
	return code
}

// ServerConfig returns the connection settings of the configured test server
// and skips the test when none is configured.
func ServerConfig(tb testing.TB) *pgx.ConnConfig {
	tb.Helper()
	dsn := os.Getenv("DOCBANK_TEST_POSTGRES_DSN")
	if dsn == "" {
		tb.Skip("set DOCBANK_TEST_POSTGRES_DSN to run PostgreSQL tests")
	}
	c, err := pgx.ParseConfig(dsn)
	require.NoError(tb, err)
	return c
}

// schemaInstallSlots bounds how many tests install the Store schema at once on
// the test server, in all test processes together. One installation holds
// about 2,400 locks until it commits, and with default settings the server's
// shared lock table has room for about 6,400, so a few more installations at
// once fail with "out of shared memory".
const schemaInstallSlots = 2

// schemaInstallLock is the first key of the advisory locks that stand for the
// slots. The second key is the slot number.
const schemaInstallLock = 0x64626b73

// ReserveSchemaInstall waits for a free schema installation slot and returns
// the function that frees it. The slot is also freed when the test ends.
func ReserveSchemaInstall(tb testing.TB) (release func()) {
	tb.Helper()
	ctx := tb.Context()
	conn, err := pgx.ConnectConfig(ctx, ServerConfig(tb))
	require.NoError(tb, err)
	// Closing the session releases its advisory lock.
	var once sync.Once
	release = func() {
		once.Do(func() { require.NoError(tb, conn.Close(context.WithoutCancel(ctx))) })
	}
	tb.Cleanup(release)
	for slot := range schemaInstallSlots {
		var locked bool
		require.NoError(tb, conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, schemaInstallLock, slot).Scan(&locked))
		if locked {
			return release
		}
	}
	_, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, schemaInstallLock, rand.IntN(schemaInstallSlots)) //nolint:gosec // Spreads waiters over the slots; no security use.
	require.NoError(tb, err)
	return release
}
