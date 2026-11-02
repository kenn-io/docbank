package consumer

import (
	"context"
	"database/sql"
	"scanner"
)

func Read(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanner.Read(rows)
}
func Forward(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanner.Forward(rows)
}

func Values(ctx context.Context, db *sql.DB) ([]int, error) {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanner.Values(rows)
}

func Defined(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanner.ReadDefined((*scanner.Defined)(rows))
}

func Ext(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanner.ReadExt((*scanner.Ext)(rows))
}

func Ptr(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()
	return scanner.ReadPtr(scanner.Ptr(rows))
}

func readPtr(p scanner.Ptr) error { return (*sql.Rows)(p).Err() }

func LocalPtr(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT value FROM items")
	if err != nil {
		return err
	}
	defer rows.Close()
	return readPtr(scanner.Ptr(rows))
}
