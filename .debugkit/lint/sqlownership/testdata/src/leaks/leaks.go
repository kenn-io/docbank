package leaks

import (
	"context"
	"database/sql"
	"sync"
)

func Leak(ctx context.Context, db *sql.DB, mu *sync.Mutex) error {
	mu.Lock()
	defer mu.Unlock()
	rows, err := db.QueryContext(ctx, "SELECT value FROM items") // want "Rows/Stmt/NamedStmt was not closed"
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	return rows.Err()
}
func ReturnOther(ctx context.Context, db *sql.DB, other *sql.Rows, mu *sync.Mutex) (*sql.Rows, error) {
	mu.Lock()
	defer mu.Unlock()
	rows, err := db.QueryContext(ctx, "SELECT value FROM items") // want "Rows/Stmt/NamedStmt was not closed"
	if err != nil {
		return nil, err
	}
	rows.Next()
	return other, nil
}
func Overwritten(ctx context.Context, db *sql.DB, mu *sync.Mutex) (rows *sql.Rows, err error) {
	mu.Lock()
	defer mu.Unlock()
	rows, _ = db.QueryContext(ctx, "SELECT value FROM items") // want "Rows/Stmt/NamedStmt was not closed"
	rows, err = db.QueryContext(ctx, "SELECT value FROM other")
	return
}
func Replaced(ctx context.Context, db *sql.DB, mu *sync.Mutex, again bool) (rows *sql.Rows, err error) {
	mu.Lock()
	defer mu.Unlock()
	rows, err = db.QueryContext(ctx, "SELECT value FROM items") // want "Rows/Stmt/NamedStmt was not closed"
	if again {
		rows, err = db.QueryContext(ctx, "SELECT value FROM other")
	}
	return
}
func EarlyExit(ctx context.Context, db *sql.DB, mu *sync.Mutex, skip bool) (*sql.Rows, error) {
	mu.Lock()
	defer mu.Unlock()
	rows, err := db.QueryContext(ctx, "SELECT value FROM items") // want "Rows/Stmt/NamedStmt was not closed"
	if skip {
		return nil, nil
	}
	return rows, err
}
func InvertedExit(ctx context.Context, db *sql.DB, mu *sync.Mutex, skip bool) (*sql.Rows, error) {
	mu.Lock()
	defer mu.Unlock()
	rows, err := db.QueryContext(ctx, "SELECT value FROM items") // want "Rows/Stmt/NamedStmt was not closed"
	if err == nil {
		if skip {
			return nil, nil
		}
		return rows, nil
	}
	return nil, err
}
