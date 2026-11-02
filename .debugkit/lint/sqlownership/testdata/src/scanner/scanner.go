package scanner

import "database/sql"

type Rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func Read(rows Rows) error {
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			return err
		}
	}
	return rows.Err()
}
func Forward(rows *sql.Rows) error { return Read(rows) }
func Values(rows Rows) ([]int, error) {
	var values []int
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
func Unchecked(rows Rows) error {
	for rows.Next() {
		var value int
		if err := rows.Scan(&value); err != nil {
			return err
		}
	}
	return nil
}

// Defined converts to and from *sql.Rows.
type Defined sql.Rows

func ReadDefined(rows *Defined) error { return (*sql.Rows)(rows).Err() }

// Ext is a defined row type with a method rows do not have.
type Ext sql.Rows

func (r *Ext) Err() error { return (*sql.Rows)(r).Err() }
func (r *Ext) Extra()     {}

func ReadExt(it interface {
	Err() error
	Extra()
}) error {
	return it.Err()
}

// Ptr is a named pointer to rows.
type Ptr *sql.Rows

func ReadPtr(p Ptr) error { return (*sql.Rows)(p).Err() }
