package modernc

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// statementCacheSize matches the mattn adapter's _stmt_cache_size: each
// connection keeps up to this many prepared statements between calls.
const statementCacheSize = 128

// modernc.org/sqlite prepares, plans and finalizes every statement that
// database/sql runs directly on a connection. Planning dominates Docbank's
// short queries, and in pure Go every SQLite allocation it makes also takes
// modernc libc's process-wide allocator lock. cachingDriver keeps each
// connection's prepared statements, as mattn/go-sqlite3 does, so repeated
// statements skip both.
type cachingDriver struct{ inner driver.Driver }

// moderncConn lists the connection interfaces database/sql uses that
// modernc.org/sqlite implements; the wrapper must keep every one of them.
type moderncConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type moderncStmt interface {
	driver.Stmt
	driver.StmtExecContext
	driver.StmtQueryContext
}

type moderncRows interface {
	driver.Rows
	driver.RowsColumnTypeDatabaseTypeName
	driver.RowsColumnTypeLength
	driver.RowsColumnTypeNullable
	driver.RowsColumnTypePrecisionScale
	driver.RowsColumnTypeScanType
}

func (d cachingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	inner, ok := conn.(moderncConn)
	if !ok {
		return nil, errors.Join(fmt.Errorf("modernc.org/sqlite connection %T lacks a database/sql interface Docbank uses", conn), conn.Close())
	}
	return &cachingConn{moderncConn: inner}, nil
}

// cachingConn holds idle prepared statements, least recently returned first,
// and finalizes the oldest when full. A statement in use is removed from the
// cache, so a query repeated while its rows are still open prepares a second
// statement instead of sharing one.
type cachingConn struct {
	moderncConn

	mu     sync.Mutex
	idle   []idleStmt
	closed bool
}

type idleStmt struct {
	query string
	stmt  moderncStmt
}

func (c *cachingConn) take(ctx context.Context, query string) (moderncStmt, error) {
	c.mu.Lock()
	for i, entry := range slices.Backward(c.idle) {
		if entry.query == query {
			c.idle = slices.Delete(c.idle, i, i+1)
			c.mu.Unlock()
			return entry.stmt, nil
		}
	}
	c.mu.Unlock()
	prepared, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	stmt, ok := prepared.(moderncStmt)
	if !ok {
		return nil, errors.Join(fmt.Errorf("modernc.org/sqlite statement %T lacks context execution", prepared), prepared.Close())
	}
	return stmt, nil
}

// put returns a statement after a successful call. After any failure the
// statement is finalized instead, so a cached statement never carries state
// from an error into the next call.
func (c *cachingConn) put(query string, stmt moderncStmt, err error) error {
	c.mu.Lock()
	if err != nil || c.closed {
		c.mu.Unlock()
		return stmt.Close()
	}
	var evicted moderncStmt
	if len(c.idle) == statementCacheSize {
		evicted = c.idle[0].stmt
		c.idle = slices.Delete(c.idle, 0, 1)
	}
	c.idle = append(c.idle, idleStmt{query: query, stmt: stmt})
	c.mu.Unlock()
	if evicted != nil {
		return evicted.Close()
	}
	return nil
}

func (c *cachingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	stmt, err := c.take(ctx, query)
	if err != nil {
		return nil, err
	}
	result, err := stmt.ExecContext(ctx, args)
	if closeErr := c.put(query, stmt, err); err == nil && closeErr != nil {
		return nil, closeErr
	}
	return result, err
}

func (c *cachingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	stmt, err := c.take(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := stmt.QueryContext(ctx, args)
	if err != nil {
		return nil, errors.Join(err, c.put(query, stmt, err))
	}
	inner, ok := rows.(moderncRows)
	if !ok {
		err := fmt.Errorf("modernc.org/sqlite rows %T lack column type metadata", rows)
		return nil, errors.Join(err, rows.Close(), c.put(query, stmt, err))
	}
	return &cachedRows{moderncRows: inner, conn: c, query: query, stmt: stmt}, nil
}

func (c *cachingConn) Close() error {
	c.mu.Lock()
	c.closed = true
	idle := c.idle
	c.idle = nil
	c.mu.Unlock()
	errs := make([]error, 0, len(idle)+1)
	for _, entry := range idle {
		errs = append(errs, entry.stmt.Close())
	}
	return errors.Join(append(errs, c.moderncConn.Close())...)
}

// cachedRows returns its statement to the connection once database/sql
// closes the rows; modernc resets the statement on close.
type cachedRows struct {
	moderncRows

	conn  *cachingConn
	query string
	stmt  moderncStmt
}

func (r *cachedRows) Close() error {
	if r.stmt == nil {
		return nil
	}
	stmt := r.stmt
	r.stmt = nil
	err := r.moderncRows.Close()
	return errors.Join(err, r.conn.put(r.query, stmt, err))
}
